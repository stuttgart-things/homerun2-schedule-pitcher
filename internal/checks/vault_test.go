package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

func vaultServer(t *testing.T, code int, body string) (*VaultTokenTTL, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		if r.URL.Path != "/v1/auth/token/lookup-self" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	return &VaultTokenTTL{
		Addr:      srv.URL + "/",
		Token:     func(context.Context) (string, error) { return "s.test", nil },
		Namespace: "infra",
		Client:    srv.Client(),
		Now:       func() time.Time { return now },
	}, &got
}

func TestVaultTokenTTL(t *testing.T) {
	day := 24 * 3600
	tests := []struct {
		name        string
		code        int
		body        string
		wantErr     bool
		minBand     status.Band
		noExpiry    bool
		daysLeft    int
		problemPart string
	}{
		{name: "plain token", code: 200, body: `{"data":{"ttl":` + itoa(40*day) + `,"renewable":true,"display_name":"token-cert-manager"}}`, daysLeft: 40},
		{name: "periodic and renewed", code: 200, body: `{"data":{"ttl":` + itoa(31*day) + `,"period":` + itoa(32*day) + `,"renewable":true}}`, daysLeft: 31},
		{name: "periodic, not renewed (#3502)", code: 200, body: `{"data":{"ttl":` + itoa(12*day) + `,"period":"768h","renewable":true}}`, daysLeft: 12, minBand: status.Warning, problemPart: "not being renewed: 12d of its 32d period"},
		{name: "periodic but not renewable", code: 200, body: `{"data":{"ttl":` + itoa(5*day) + `,"period":` + itoa(32*day) + `,"renewable":false}}`, daysLeft: 5},
		{name: "root token", code: 200, body: `{"data":{"ttl":0,"display_name":"root","policies":["root"]}}`, noExpiry: true},
		{name: "forbidden", code: 403, body: `{"errors":["permission denied"]}`, minBand: status.Critical, problemPart: "(403)"},
		{name: "bad token", code: 400, body: `{"errors":["bad token"]}`, minBand: status.Critical, problemPart: "expired or revoked"},
		{name: "sealed", code: 503, body: `{"errors":["Vault is sealed"]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, req := vaultServer(t, tt.code, tt.body)
			r, err := v.Run(context.Background())
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "sealed") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("X-Vault-Token") != "s.test" || req.Header.Get("X-Vault-Namespace") != "infra" {
				t.Errorf("headers = %v", req.Header)
			}
			if r.MinBand != tt.minBand || r.NoExpiry != tt.noExpiry || !strings.Contains(r.Problem, tt.problemPart) {
				t.Fatalf("result = %+v", r)
			}
			if tt.daysLeft > 0 {
				if got := int(r.Expiry.Sub(v.Now()).Hours() / 24); got != tt.daysLeft {
					t.Errorf("days left = %d, want %d", got, tt.daysLeft)
				}
			}
		})
	}
}

func TestVaultTokenCouldNotCheck(t *testing.T) {
	v := &VaultTokenTTL{Addr: "http://127.0.0.1:1", Token: func(context.Context) (string, error) { return "x", nil }, Client: &http.Client{Timeout: time.Second}}
	if _, err := v.Run(context.Background()); err == nil {
		t.Fatal("expected a connection error")
	}
	v.Token = func(context.Context) (string, error) { return "", nil }
	if _, err := v.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
