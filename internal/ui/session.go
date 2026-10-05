package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie.
const CookieName = "schedule_pitcher_session"

// Sessions signs login sessions with a key derived from AUTH_TOKEN, so
// rotating the token logs everyone out and no session store is needed.
type Sessions struct {
	token string
	key   []byte
	TTL   time.Duration
	now   func() time.Time
}

func NewSessions(token string) *Sessions {
	sum := sha256.Sum256([]byte("homerun2-schedule-pitcher-ui:" + token))
	return &Sessions{token: token, key: sum[:], TTL: 12 * time.Hour, now: time.Now}
}

// Enabled reports whether logins are possible at all.
func (s *Sessions) Enabled() bool { return s.token != "" }

// CheckToken compares a login token in constant time.
func (s *Sessions) CheckToken(t string) bool {
	return s.token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *Sessions) sign(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Issue sets the session cookie for name.
func (s *Sessions) Issue(w http.ResponseWriter, r *http.Request, name string) {
	exp := s.now().Add(s.TTL)
	payload := base64.RawURLEncoding.EncodeToString([]byte(name)) + "." + strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: payload + "." + s.sign(payload), Path: "/",
		Expires: exp, HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}

// Clear removes the session cookie.
func (s *Sessions) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// User returns the logged-in name of a request.
func (s *Sessions) User(r *http.Request) (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(s.sign(payload))) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() > exp {
		return "", false
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	return string(name), true
}
