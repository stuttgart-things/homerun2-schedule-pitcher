package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// VaultTokenTTL reads the remaining TTL of a Vault or OpenBao token with
// GET /v1/auth/token/lookup-self, using the token itself. It never renews
// the token and never sees another one; the token's policy needs read on
// auth/token/lookup-self.
type VaultTokenTTL struct {
	Addr string
	// Token is resolved on every run, so a rotated Secret is picked up.
	Token func(ctx context.Context) (string, error)
	// Namespace is sent as X-Vault-Namespace (Vault Enterprise / OpenBao namespaces).
	Namespace string
	Client    *http.Client
	Now       func() time.Time
}

// vaultSeconds accepts the forms Vault uses for durations in lookup-self:
// a number of seconds, a numeric string, or a Go duration string ("768h").
type vaultSeconds int64

func (v *vaultSeconds) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*v = 0
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*v = vaultSeconds(n)
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("cannot parse duration %q", s)
	}
	*v = vaultSeconds(d / time.Second)
	return nil
}

type vaultLookup struct {
	Data struct {
		TTL         vaultSeconds `json:"ttl"`
		Period      vaultSeconds `json:"period"`
		Renewable   bool         `json:"renewable"`
		DisplayName string       `json:"display_name"`
		Policies    []string     `json:"policies"`
	} `json:"data"`
	Errors []string `json:"errors"`
}

func (v *VaultTokenTTL) Run(ctx context.Context) (Result, error) {
	token, err := v.Token(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("resolving token: %w", err)
	}
	if token == "" {
		return Result{}, fmt.Errorf("token is empty")
	}

	url := strings.TrimSuffix(v.Addr, "/") + "/v1/auth/token/lookup-self"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("X-Vault-Token", token)
	if v.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.Namespace)
	}
	resp, err := v.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("GET lookup-self: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("reading lookup-self: %w", err)
	}

	var lk vaultLookup
	_ = json.Unmarshal(body, &lk)
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusBadRequest && containsAny(lk.Errors, "bad token", "invalid token")):
		return Result{
			MinBand: status.Critical,
			Problem: fmt.Sprintf("Vault rejects the token (%d): it is expired or revoked, or its policy lacks auth/token/lookup-self", resp.StatusCode),
		}, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Result{}, fmt.Errorf("GET lookup-self returned %s: %s", resp.Status, strings.Join(lk.Errors, "; "))
	}
	if err := json.Unmarshal(body, &lk); err != nil {
		return Result{}, fmt.Errorf("decoding lookup-self: %w", err)
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	d := lk.Data
	r := Result{Subject: d.DisplayName}
	if d.TTL <= 0 {
		r.NoExpiry = true
		r.Summary = "token has no TTL (root or non-expiring token)"
		return r, nil
	}
	ttl := time.Duration(d.TTL) * time.Second
	r.Expiry = now().Add(ttl)
	r.Summary = fmt.Sprintf("token TTL %s left, expires on %s", humanDuration(ttl), r.Expiry.UTC().Format("2006-01-02 15:04 MST"))
	if d.Period > 0 {
		period := time.Duration(d.Period) * time.Second
		r.Summary += fmt.Sprintf(" (periodic, period %s)", humanDuration(period))
		// A periodic token that is renewed keeps its TTL close to the period.
		// Below half of it, nobody renews it: the failure of
		// stuttgart-things/stuttgart-things#3502.
		if d.Renewable && ttl < period/2 {
			r.MinBand = status.Warning
			r.Problem = fmt.Sprintf("periodic token is not being renewed: %s of its %s period left", humanDuration(ttl), humanDuration(period))
		}
	}
	return r, nil
}

func containsAny(list []string, subs ...string) bool {
	for _, s := range list {
		for _, sub := range subs {
			if strings.Contains(strings.ToLower(s), sub) {
				return true
			}
		}
	}
	return false
}

// humanDuration renders whole days, or hours below two days.
func humanDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}
