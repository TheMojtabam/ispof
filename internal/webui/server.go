// Package webui mounts the API handlers alongside the embedded HTML
// admin UI and applies cross-cutting middleware (access log, basic auth).
package webui

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/TheMojtabam/ispof/internal/api"
)

//go:embed all:assets
var embedded embed.FS

// Config drives the HTTP layer. APIServer is the wired-up router; the
// webui package is the *delivery* mechanism, not where business logic
// lives.
type Config struct {
	APIServer *api.Server
	BasicAuth string // "user:password" — empty disables auth
}

// New returns the full http.Handler with all middleware applied.
func New(cfg Config) (http.Handler, error) {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		return nil, fmt.Errorf("locate embedded assets: %w", err)
	}

	mux := http.NewServeMux()
	cfg.APIServer.Register(mux)
	// Anything not matched above falls through to the static file server.
	mux.Handle("/", http.FileServer(http.FS(sub)))

	var h http.Handler = mux
	h = withAccessLog(h)
	if cfg.BasicAuth != "" {
		h = withBasicAuth(h, cfg.BasicAuth)
	}
	return h, nil
}

// ─────────────────────────── middleware ───────────────────────────

func withBasicAuth(next http.Handler, creds string) http.Handler {
	parts := strings.SplitN(creds, ":", 2)
	if len(parts) != 2 {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "panel misconfigured: -auth must be user:password", http.StatusInternalServerError)
		})
	}
	expectedUser := []byte(parts[0])
	expectedPass := []byte(parts[1])

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz is intentionally unauthenticated so installers and
		// reverse proxies can probe the panel before configuring auth.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		// Constant-time compare on every component to avoid leaking the
		// username via timing.
		userOK := subtle.ConstantTimeCompare([]byte(user), expectedUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), expectedPass) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Ispof"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/api/stream/") {
			return // too chatty to log
		}
		slog.Info("http",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("dur", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	written  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.written {
		return
	}
	s.status = code
	s.written = true
	s.ResponseWriter.WriteHeader(code)
}

// Flush propagates to the underlying flusher when present. Required for
// our SSE handler to actually push events instead of buffering them
// until the response is closed.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
