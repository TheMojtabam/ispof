package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pechenyeru/quiccochet/internal/panel/auth"
)

type ctxKey string

const ctxUser ctxKey = "user"

// requireAuth gates routes behind a Bearer JWT.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" {
			// fallback: cookie-stored token (so embedded HTML can use fetch w/o JS Authorization header trick)
			if c, err := r.Cookie("qcc_token"); err == nil {
				raw = c.Value
			}
		}
		sub, err := auth.VerifyJWT(s.cfg.JWTSecret, raw)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "auth required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, sub)))
	})
}

// requireAgent gates /agent endpoints behind a shared agent token.
func (s *Server) requireAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Agent-Token")
		if tok == "" || tok != s.cfg.AgentToken {
			writeErr(w, http.StatusUnauthorized, "agent token invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ipWhitelist restricts panel access to configured CIDRs. /agent and /installer
// bypass this check (their auth is token-based).
func (s *Server) ipWhitelist(next http.Handler) http.Handler {
	if len(s.cfg.IPWhitelist) == 0 {
		return next
	}
	nets := make([]*net.IPNet, 0, len(s.cfg.IPWhitelist))
	for _, c := range s.cfg.IPWhitelist {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			nets = append(nets, n)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/agent/") || strings.HasPrefix(r.URL.Path, "/installer/") {
			next.ServeHTTP(w, r)
			return
		}
		ip := net.ParseIP(clientIP(r))
		for _, n := range nets {
			if n.Contains(ip) {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, http.StatusForbidden, "ip not whitelisted")
	})
}

// LoginRequest body
type LoginRequest struct {
	User string `json:"user"`
	Pass string `json:"pass"`
	OTP  string `json:"otp,omitempty"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RateLimitLogins && !s.limiter.Allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.User != s.cfg.AdminUser {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if !auth.CheckPassword(s.cfg.AdminPass, req.Pass) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if s.cfg.TOTPKey != "" && !auth.VerifyTOTP(s.cfg.TOTPKey, req.OTP) {
		writeErr(w, http.StatusUnauthorized, "invalid 2FA code")
		return
	}

	ttl := time.Duration(s.cfg.SessionTimeoutMin) * time.Minute
	tok, err := auth.IssueJWT(s.cfg.JWTSecret, req.User, ttl)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "qcc_token",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		Secure:   s.cfg.BehindTLS,
	})
	writeJSON(w, 200, map[string]any{"token": tok, "expires_in": int(ttl.Seconds())})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(ctxUser).(string)
	writeJSON(w, 200, map[string]any{
		"user":    user,
		"side":    s.cfg.Side,
		"version": "0.4.1",
		"twofa":   s.cfg.TOTPKey != "",
	})
}
