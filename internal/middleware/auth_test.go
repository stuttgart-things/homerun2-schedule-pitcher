package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The API and POST /findings are bearer-only: sthings-portal's forwarded
// identity (X-Forwarded-User, #62) never authorizes them.
func TestTokenAuthIgnoresForwardedUser(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "secret")
	called := false
	h := TokenAuthMiddleware(func(http.ResponseWriter, *http.Request) { called = true })
	for _, auth := range []string{"", "Bearer wrong"} {
		r := httptest.NewRequest(http.MethodPost, "/findings", nil)
		r.Header.Set("X-Forwarded-User", "patrick")
		r.Header.Set("X-Forwarded-Groups", "portal-admin")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h(rr, r)
		if rr.Code != http.StatusUnauthorized || called {
			t.Fatalf("auth %q: %d, handler called %v", auth, rr.Code, called)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/checks", nil)
	r.Header.Set("Authorization", "Bearer secret")
	h(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("valid bearer rejected")
	}
}
