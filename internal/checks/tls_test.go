package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, notAfter time.Time) testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-2 * 365 * 24 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca testCA) leaf(t *testing.T, name string, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
}

func serveTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func writeCA(t *testing.T, ca testCA) string {
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTLSEndpointLeafAndChain(t *testing.T) {
	caExpiry := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	leafExpiry := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	ca := newCA(t, caExpiry)
	addr := serveTLS(t, ca.leaf(t, "svc.example.test", leafExpiry))

	c, err := NewTLSEndpoint(addr, "svc.example.test", writeCA(t, ca), false, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Expiry.Equal(leafExpiry) || r.MinBand != status.OK || r.Subject != "svc.example.test" {
		t.Fatalf("leaf result = %+v", r)
	}

	c.Chain = true
	r, err = c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Expiry.Equal(caExpiry) || !strings.Contains(r.Summary, "test-ca") {
		t.Fatalf("chain result = %+v", r)
	}
}

func TestTLSEndpointUntrustedAndWrongName(t *testing.T) {
	ca := newCA(t, time.Now().Add(365*24*time.Hour))
	addr := serveTLS(t, ca.leaf(t, "svc.example.test", time.Now().Add(90*24*time.Hour)))

	c, _ := NewTLSEndpoint(addr, "svc.example.test", "", false, 5*time.Second)
	r, err := c.Run(context.Background())
	if err != nil || r.MinBand != status.Error || !strings.Contains(r.Problem, "not trusted") {
		t.Fatalf("untrusted: %+v, %v", r, err)
	}

	c, _ = NewTLSEndpoint(addr, "other.example.test", writeCA(t, ca), false, 5*time.Second)
	r, err = c.Run(context.Background())
	if err != nil || r.MinBand != status.Error || !strings.Contains(r.Problem, "not valid for other.example.test") {
		t.Fatalf("wrong name: %+v, %v", r, err)
	}
}

func TestTLSEndpointExpiredStillRead(t *testing.T) {
	ca := newCA(t, time.Now().Add(365*24*time.Hour))
	expired := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	addr := serveTLS(t, ca.leaf(t, "svc.example.test", expired))

	c, _ := NewTLSEndpoint(addr, "svc.example.test", writeCA(t, ca), false, 5*time.Second)
	r, err := c.Run(context.Background())
	if err != nil || !r.Expiry.Equal(expired) || r.MinBand != status.OK {
		t.Fatalf("expired: %+v, %v", r, err)
	}
}

func TestTLSEndpointCouldNotCheck(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	c, _ := NewTLSEndpoint(addr, "", "", false, time.Second)
	if _, err := c.Run(context.Background()); err == nil {
		t.Fatal("expected dial error")
	}
	if _, err := NewTLSEndpoint(addr, "", "/does/not/exist", false, time.Second); err == nil {
		t.Fatal("expected CA file error")
	}
}
