package checks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// TLSEndpoint reads the certificate expiry of a TLS endpoint.
type TLSEndpoint struct {
	Target     string // host:port
	ServerName string // SNI and name to verify; defaults to the target host
	Roots      *x509.CertPool
	Chain      bool // earliest expiry of the whole presented chain, not only the leaf
	Timeout    time.Duration
	// Now is used to verify the chain; defaults to time.Now.
	Now func() time.Time
}

// NewTLSEndpoint builds a TLS check. caFile adds a CA to the system roots.
func NewTLSEndpoint(target, serverName, caFile string, chain bool, timeout time.Duration) (*TLSEndpoint, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
	}
	if serverName == "" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", target, err)
		}
		serverName = host
	}
	return &TLSEndpoint{Target: target, ServerName: serverName, Roots: roots, Chain: chain, Timeout: timeout}, nil
}

func (t *TLSEndpoint) Run(ctx context.Context) (Result, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: t.Timeout},
		// Verification is done below, so an expired or untrusted certificate
		// is still read and reported instead of failing the handshake.
		Config: &tls.Config{ServerName: t.ServerName, InsecureSkipVerify: true}, //nolint:gosec // verified manually
	}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", t.Target)
	if err != nil {
		return Result{}, fmt.Errorf("TLS dial %s: %w", t.Target, err)
	}
	defer func() { _ = conn.Close() }()

	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Result{}, fmt.Errorf("%s presented no certificate", t.Target)
	}
	return t.evaluate(certs), nil
}

func (t *TLSEndpoint) evaluate(certs []*x509.Certificate) Result {
	leaf := certs[0]
	expiring := leaf
	if t.Chain {
		for _, c := range certs[1:] {
			if c.NotAfter.Before(expiring.NotAfter) {
				expiring = c
			}
		}
	}

	r := Result{Expiry: expiring.NotAfter, Subject: leaf.Subject.CommonName}
	if r.Subject == "" && len(leaf.DNSNames) > 0 {
		r.Subject = leaf.DNSNames[0]
	}
	what := "certificate"
	if expiring != leaf {
		what = fmt.Sprintf("chain certificate %q", expiring.Subject.CommonName)
	}
	r.Summary = fmt.Sprintf("%s of %s expires on %s", what, t.Target, expiring.NotAfter.UTC().Format("2006-01-02 15:04 MST"))

	if err := t.verify(certs); err != nil {
		r.MinBand = status.Error
		r.Problem = err.Error()
	}
	return r
}

// verify checks trust and hostname. Expiry is left to the thresholds, so it is
// verified at a time inside the validity window.
func (t *TLSEndpoint) verify(certs []*x509.Certificate) error {
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	at := now()
	leaf := certs[0]
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter.Add(-time.Minute)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       t.ServerName,
		Roots:         t.Roots,
		Intermediates: inter,
		CurrentTime:   at,
	})
	if err == nil {
		return nil
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return fmt.Errorf("certificate is not valid for %s", t.ServerName)
	}
	return fmt.Errorf("certificate is not trusted: %v", err)
}
