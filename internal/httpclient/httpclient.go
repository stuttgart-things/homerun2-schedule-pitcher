// Package httpclient builds HTTP clients that trust the system roots plus an
// optional CA file.
package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// New returns a client with a request timeout that trusts the system roots
// plus caFile. insecure skips verification (opt-in via profile).
func New(caFile string, insecure bool, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via profile
	if caFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		tlsCfg.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
