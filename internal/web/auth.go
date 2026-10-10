package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CookieName is the HttpOnly session cookie set after a successful login.
const CookieName = "doge_web_session"

// bearerPrefix marks the Authorization header carrying the raw access token
// (API clients, curl, a separately deployed web UI).
const bearerPrefix = "Bearer "

const (
	sessionTTL      = 7 * 24 * time.Hour
	tokenBytes      = 32
	loginFailDelay  = 300 * time.Millisecond
	maxCookieLength = 256
)

// authState owns the access token and the logged-in session cookies. It is
// intentionally process-scoped: every startup generates (or accepts) a fresh
// token, so a leaked cookie never outlives the process.
type authState struct {
	mu       sync.Mutex
	token    string
	sessions map[string]time.Time // cookie value -> expiry
}

// newAuthState builds the authenticator with the given token. Empty tokens
// are rejected; pass an empty string to auto-generate one.
func newAuthState(token string) (*authState, error) {
	if token == "" {
		var err error
		token, err = generateToken()
		if err != nil {
			return nil, err
		}
	}
	if len(token) > 1024 {
		return nil, fmt.Errorf("token too long")
	}
	return &authState{token: token, sessions: map[string]time.Time{}}, nil
}

// generateToken produces a cryptographically random hex token.
func generateToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Token exposes the current access token (for console printing / files).
func (a *authState) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

// validToken compares a candidate token in constant time.
func (a *authState) validToken(candidate string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return subtle.ConstantTimeCompare([]byte(a.token), []byte(candidate)) == 1
}

// login validates a presented token and, on success, issues a new session
// cookie value.
func (a *authState) login(token string) (string, bool) {
	if !a.validToken(token) {
		time.Sleep(loginFailDelay) // slow brute force on the random token
		return "", false
	}
	value, err := generateToken()
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	a.sessions[value] = time.Now().Add(sessionTTL)
	a.mu.Unlock()
	return value, true
}

// authenticated reports whether a cookie value belongs to a valid session.
func (a *authState) authenticated(cookie string) bool {
	if cookie == "" || len(cookie) > maxCookieLength {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[cookie]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(a.sessions, cookie)
		return false
	}
	return true
}

// logout revokes a session cookie.
func (a *authState) logout(cookie string) {
	a.mu.Lock()
	delete(a.sessions, cookie)
	a.mu.Unlock()
}

// setSessionCookie writes the session cookie on an authenticated response.
func setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// clearSessionCookie expires the session cookie client-side.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// requireAuth wraps a handler, rejecting requests without a valid credential.
// Three credential channels are accepted, in order:
//
//   - Authorization: Bearer <access token> — API clients and a separately
//     deployed web UI; the header value is compared in constant time.
//   - ?token=<access token> — WebSocket / EventSource connections, which
//     cannot set headers from a browser. Leaks into proxies' access logs more
//     easily than the header form, so clients that can set headers should.
//   - the session cookie issued by /api/auth/login — the embedded web UI.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.authorized(r); !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// authorized validates the request's credential and reports which channel
// carried it ("bearer", "query" or "cookie"); via is only informative.
func (a *authState) authorized(r *http.Request) (via string, ok bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
		if tok := strings.TrimSpace(strings.TrimPrefix(h, bearerPrefix)); tok != "" {
			return "bearer", a.checkToken(tok)
		}
	}
	if tok := strings.TrimSpace(r.URL.Query().Get("token")); tok != "" {
		return "query", a.checkToken(tok)
	}
	if c, err := r.Cookie(CookieName); err == nil && a.authenticated(c.Value) {
		return "cookie", true
	}
	return "", false
}

// checkToken compares a candidate access token, slowing failed attempts the
// same way login does.
func (a *authState) checkToken(candidate string) bool {
	if !a.validToken(candidate) {
		time.Sleep(loginFailDelay)
		return false
	}
	return true
}
