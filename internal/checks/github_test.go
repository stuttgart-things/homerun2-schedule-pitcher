package checks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

func githubServer(t *testing.T, handler http.HandlerFunc) *GitHubTokenExpiry {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &GitHubTokenExpiry{
		APIURL:  srv.URL,
		Token:   func(context.Context) (string, error) { return "ghp_test", nil },
		Owner:   true,
		Timeout: 5 * time.Second,
	}
}

func TestGitHubTokenExpiry(t *testing.T) {
	g := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/rate_limit":
			w.Header().Set(ExpirationHeader, "2027-03-28 10:15:00 UTC")
			_, _ = w.Write([]byte(`{}`))
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		}
	})
	r, err := g.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2027, 3, 28, 10, 15, 0, 0, time.UTC)
	if !r.Expiry.Equal(want) || r.Subject != "octocat" || r.NoExpiry {
		t.Fatalf("result = %+v", r)
	}
}

func TestGitHubTokenExpiryOffsetFormat(t *testing.T) {
	got, err := parseExpiration("2027-03-28 10:15:00 +0100")
	if err != nil || !got.Equal(time.Date(2027, 3, 28, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseExpiration("next tuesday"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGitHubTokenNoExpiry(t *testing.T) {
	g := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.WriteHeader(http.StatusForbidden)
		}
	})
	r, err := g.Run(context.Background())
	if err != nil || !r.NoExpiry || r.Subject != "" {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestGitHubTokenRevoked(t *testing.T) {
	g := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	r, err := g.Run(context.Background())
	if err != nil || r.MinBand != status.Critical || !strings.Contains(r.Problem, "401") {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestGitHubCouldNotCheck(t *testing.T) {
	g := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := g.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}

	g.Token = func(context.Context) (string, error) { return "", errors.New("secret not found") }
	if _, err := g.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "secret not found") {
		t.Fatalf("err = %v", err)
	}
}
