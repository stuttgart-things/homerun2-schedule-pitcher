package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// ExpirationHeader carries the expiry of the token used for a GitHub API call.
const ExpirationHeader = "github-authentication-token-expiration"

// expirationLayouts are the formats GitHub uses for ExpirationHeader,
// e.g. "2027-03-28 10:15:00 UTC" or "2027-03-28 10:15:00 +0100".
var expirationLayouts = []string{
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 -0700",
	time.RFC3339,
}

// GitHubTokenExpiry reads a token's expiry from the GitHub API. It calls
// GET /rate_limit, which does not count against the rate limit.
type GitHubTokenExpiry struct {
	APIURL string
	// Token is resolved on every run, so a rotated Secret is picked up.
	Token   func(ctx context.Context) (string, error)
	Owner   bool
	Timeout time.Duration
	Client  *http.Client
}

func (g *GitHubTokenExpiry) Run(ctx context.Context) (Result, error) {
	token, err := g.Token(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("resolving token: %w", err)
	}
	if token == "" {
		return Result{}, fmt.Errorf("token is empty")
	}

	resp, err := g.get(ctx, token, "/rate_limit")
	if err != nil {
		return Result{}, err
	}
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Result{
			MinBand: status.Critical,
			Problem: "GitHub rejects the token (401), it is expired or revoked",
		}, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Result{}, fmt.Errorf("GET /rate_limit returned %s", resp.Status)
	}

	var r Result
	if owner := g.owner(ctx, token); owner != "" {
		r.Subject = owner
	}

	raw := resp.Header.Get(ExpirationHeader)
	if raw == "" {
		r.NoExpiry = true
		r.Summary = "token has no expiration date"
		return r, nil
	}
	exp, err := parseExpiration(raw)
	if err != nil {
		return Result{}, err
	}
	r.Expiry = exp
	r.Summary = "token expires on " + exp.Format("2006-01-02 15:04 MST")
	return r, nil
}

func parseExpiration(raw string) (time.Time, error) {
	for _, layout := range expirationLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %s header %q", ExpirationHeader, raw)
}

// owner returns the login of the token's user. It is best effort: GitHub App
// tokens, for example, cannot call /user.
func (g *GitHubTokenExpiry) owner(ctx context.Context, token string) string {
	if !g.Owner {
		return ""
	}
	resp, err := g.get(ctx, token, "/user")
	if err != nil {
		slog.Debug("looking up token owner failed", "error", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return ""
	}
	return u.Login
}

func (g *GitHubTokenExpiry) get(ctx context.Context, token, path string) (*http.Response, error) {
	url := strings.TrimSuffix(g.APIURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp, nil
}

func (g *GitHubTokenExpiry) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: g.Timeout}
}
