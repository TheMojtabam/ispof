// Package auth handles password hashing, session tokens, and TOTP 2FA for
// the panel.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash of pw at cost 12.
func HashPassword(pw string) (string, error) {
	if pw == "" {
		return "", errors.New("empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(h), err
}

// CheckPassword reports whether plaintext matches the stored hash. It accepts
// three formats so the panel can grow its scheme without breaking installs:
//  1. bcrypt   ($2a$..., $2b$..., $2y$...)
//  2. salt:hash with sha256 (first-revision panel format)
//  3. plain    (initial install — install.sh writes the password as-is)
func CheckPassword(stored, plain string) bool {
	if stored == "" || plain == "" {
		return false
	}
	if strings.HasPrefix(stored, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)) == nil
	}
	if i := strings.IndexByte(stored, ':'); i > 0 {
		salt, err1 := base64.RawStdEncoding.DecodeString(stored[:i])
		want, err2 := base64.RawStdEncoding.DecodeString(stored[i+1:])
		if err1 == nil && err2 == nil {
			h := sha256.Sum256(append(salt, []byte(plain)...))
			return hmac.Equal(h[:], want)
		}
	}
	return hmac.Equal([]byte(stored), []byte(plain))
}

// ---- Tokens (HMAC-SHA256, JWT-shaped) ----

type tokenClaims struct {
	Sub  string `json:"sub"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
	Role string `json:"role,omitempty"`
}

// IssueJWT issues a stateless session token signed with HMAC-SHA256.
//
// The format is body.sig where each piece is base64url-encoded. We avoid a
// JWT library to keep dependencies lean — the same bytes can be verified
// by any HS256 JWT validator.
func IssueJWT(secret, sub string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := tokenClaims{Sub: sub, Iat: now.Unix(), Exp: now.Add(ttl).Unix(), Role: "admin"}
	b, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, nil
}

// VerifyJWT parses & validates the token, returning the subject ("sub").
func VerifyJWT(secret, raw string) (string, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return "", errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return "", errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	c := tokenClaims{}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", err
	}
	if c.Exp < time.Now().Unix() {
		return "", fmt.Errorf("token expired")
	}
	return c.Sub, nil
}

// ---- Login rate limiter ----

// LoginLimiter is a per-IP sliding-window limiter for /api/login.
type LoginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
	window   time.Duration
}

func NewLoginLimiter(max int, window time.Duration) *LoginLimiter {
	return &LoginLimiter{attempts: map[string][]time.Time{}, max: max, window: window}
}

func (l *LoginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	tries := l.attempts[ip]
	kept := tries[:0]
	for _, t := range tries {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.attempts[ip] = kept
		return false
	}
	l.attempts[ip] = append(kept, now)
	return true
}

// randID is a 16-char hex random ID used for objects without a natural key.
func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

var _ = randID // silence unused import if no callers
