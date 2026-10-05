package pitcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
)

// Pitcher delivers a rendered message.
type Pitcher interface {
	Pitch(ctx context.Context, m Message) error
}

const requestTimeout = 10 * time.Second

// HTTP posts messages to omni-pitcher, either as a Grafana webhook
// (POST /pitch/grafana) or as a generic homerun message (POST /pitch).
type HTTP struct {
	Addr   string
	Format string
	Token  string
	client *http.Client
}

// NewHTTP builds an HTTP pitcher. caFile adds a CA to the system roots.
func NewHTTP(addr, format, token, caFile string, insecure bool) (*HTTP, error) {
	if format != profile.FormatGrafana && format != profile.FormatGeneric {
		return nil, fmt.Errorf("unknown pitcher format %q", format)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via profile
	if caFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading pitcher CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		tlsCfg.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	return &HTTP{
		Addr:   addr,
		Format: format,
		Token:  token,
		client: &http.Client{Timeout: requestTimeout, Transport: transport},
	}, nil
}

func (h *HTTP) Pitch(ctx context.Context, m Message) error {
	var body any = m.HomerunMessage()
	if h.Format == profile.FormatGrafana {
		body = GrafanaPayload(m)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Addr, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", h.Addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s returned %s: %s", h.Addr, resp.Status, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Ready asks omni-pitcher whether it accepts messages (GET /ready next to the
// pitch URL, falling back to /health on older versions).
func (h *HTTP) Ready(ctx context.Context) error {
	code, err := h.get(ctx, "ready")
	if err == nil && code == http.StatusNotFound {
		code, err = h.get(ctx, "health")
	}
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("pitcher readiness returned %d", code)
	}
	return nil
}

func (h *HTTP) get(ctx context.Context, endpoint string) (int, error) {
	u, err := ProbeURL(h.Addr, endpoint)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ProbeURL derives an omni-pitcher endpoint from the pitch URL by cutting
// everything from the /pitch segment on: https://h/pitch/grafana -> https://h/<endpoint>.
func ProbeURL(addr, endpoint string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parsing pitcher addr %q: %w", addr, err)
	}
	p := strings.TrimSuffix(u.Path, "/")
	if i := strings.LastIndex(p, "/pitch"); i >= 0 {
		p = p[:i]
	}
	u.Path = path.Join("/", p, endpoint)
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// GrafanaAlert is the subset of the Grafana webhook that omni-pitcher reads.
type GrafanaAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
	Fingerprint  string            `json:"fingerprint"`
}

type GrafanaWebhook struct {
	Receiver     string            `json:"receiver"`
	Status       string            `json:"status"`
	Alerts       []GrafanaAlert    `json:"alerts"`
	CommonLabels map[string]string `json:"commonLabels"`
	Title        string            `json:"title"`
	Message      string            `json:"message"`
}

// GrafanaPayload renders a message as a Grafana webhook with one alert.
// omni-pitcher uses alertname as title, the summary as text and receiver as
// system; other labels become tags.
func GrafanaPayload(m Message) GrafanaWebhook {
	st := "firing"
	if m.Resolved {
		st = "resolved"
	}
	labels := map[string]string{
		"alertname": m.Title,
		"severity":  m.Severity,
		"check":     m.CheckID,
		"type":      m.Type,
	}
	if len(m.Tags) > 0 {
		labels["tags"] = strings.Join(m.Tags, ",")
	}
	if m.Assignee != "" {
		labels["assignee"] = m.Assignee
	}
	sum := sha256.Sum256([]byte(m.AlertName))
	alert := GrafanaAlert{
		Status:       st,
		Labels:       labels,
		Annotations:  map[string]string{"summary": m.Text},
		StartsAt:     m.At.Format(time.RFC3339),
		GeneratorURL: m.URL,
		Fingerprint:  hex.EncodeToString(sum[:8]),
	}
	if m.Resolved {
		alert.EndsAt = alert.StartsAt
	}
	return GrafanaWebhook{
		Receiver:     m.System,
		Status:       st,
		Alerts:       []GrafanaAlert{alert},
		CommonLabels: map[string]string{"check": m.CheckID},
		Title:        m.Title,
		Message:      m.Text,
	}
}

// File appends messages as JSON lines to a file (local development).
type File struct {
	Path string
	mu   sync.Mutex
}

func (f *File) Pitch(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.OpenFile(f.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening pitch file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	data, err := json.Marshal(m.HomerunMessage())
	if err != nil {
		return err
	}
	_, err = fh.Write(append(data, '\n'))
	return err
}

// Writer prints messages in a readable form (dry runs).
type Writer struct {
	W  io.Writer
	mu sync.Mutex
}

func (w *Writer) Pitch(_ context.Context, m Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := fmt.Fprintf(w.W, "[%s] %s\n  %s\n", strings.ToUpper(m.Severity), m.Title, strings.ReplaceAll(m.Text, "\n", "\n  "))
	return err
}
