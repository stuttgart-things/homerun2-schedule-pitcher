// Package checks implements the check types of a SchedulePitcherProfile.
package checks

import (
	"context"
	"fmt"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/httpclient"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// Result is what a check found. A check that could not complete returns an
// error instead; that is never reported as a finding about the target.
type Result struct {
	// Expiry is when the watched thing expires. Zero if not applicable.
	Expiry time.Time
	// NoExpiry is set when the thing is known to never expire.
	NoExpiry bool
	// MinBand raises the band regardless of thresholds, e.g. critical for a
	// revoked token. Problem then says why.
	MinBand status.Band
	Problem string
	// Summary describes the finding in one sentence.
	Summary string
	// Subject names what was checked, e.g. the token owner or the certificate CN.
	Subject string
}

// Checker runs one check.
type Checker interface {
	Run(ctx context.Context) (Result, error)
}

// SecretResolver resolves secret references from the profile.
type SecretResolver interface {
	Resolve(ctx context.Context, v *profile.ValueFrom) (string, error)
}

// New builds the checker for a check definition.
func New(c profile.Check, secrets SecretResolver) (Checker, error) {
	switch c.Type {
	case profile.TypeGitHubTokenExpiry:
		return &GitHubTokenExpiry{
			APIURL:  c.APIURL,
			Token:   func(ctx context.Context) (string, error) { return secrets.Resolve(ctx, c.TokenFrom) },
			Owner:   c.Owner == nil || *c.Owner,
			Timeout: c.Timeout.D(),
		}, nil
	case profile.TypeTLSEndpoint:
		return NewTLSEndpoint(c.Target, c.ServerName, c.CAFile, c.Chain, c.Timeout.D())
	case profile.TypeVaultTokenTTL:
		client, err := httpclient.New(c.CAFile, c.Insecure, c.Timeout.D())
		if err != nil {
			return nil, err
		}
		return &VaultTokenTTL{
			Addr:      c.Addr,
			Token:     func(ctx context.Context) (string, error) { return secrets.Resolve(ctx, c.TokenFrom) },
			Namespace: c.VaultNamespace,
			Client:    client,
		}, nil
	default:
		return nil, fmt.Errorf("unknown check type %q", c.Type)
	}
}
