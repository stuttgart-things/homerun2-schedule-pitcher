package pitcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

var (
	now   = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	check = profile.Check{
		ID: "github-runner-pat", Type: profile.TypeGitHubTokenExpiry,
		Description: "PAT used by ARC runners", Tags: []string{"expiry", "pat"},
		URL: "https://github.com/settings/personal-access-tokens", Assignee: "platform-team",
	}
)

func TestRenderFiring(t *testing.T) {
	n := state.Notification{Kind: state.Firing, Band: status.Error, Expiry: now.Add(12*24*time.Hour + time.Hour),
		Summary: "token expires on 2026-10-17", Subject: "octocat", At: now}
	m := Render(n, check, "homerun2-schedule-pitcher")
	if m.Title != "GitHub token github-runner-pat (octocat) expires in 12 days (2026-10-17)" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Severity != "error" || m.Resolved {
		t.Errorf("severity %q resolved %v", m.Severity, m.Resolved)
	}
	if !strings.Contains(m.Text, "PAT used by ARC runners") || !strings.Contains(m.Text, "Subject: octocat") {
		t.Errorf("text = %q", m.Text)
	}
}

func TestRenderTitles(t *testing.T) {
	tests := []struct {
		n    state.Notification
		want string
	}{
		{state.Notification{Kind: state.Firing, Expiry: now.Add(-3 * 24 * time.Hour), At: now}, "GitHub token github-runner-pat expired 3 days ago (2026-10-02)"},
		{state.Notification{Kind: state.Firing, Expiry: now.Add(5 * time.Hour), At: now}, "GitHub token github-runner-pat expires in 5 hours (2026-10-05)"},
		{state.Notification{Kind: state.Firing, Problem: "GitHub rejects the token (401)", At: now}, "GitHub token github-runner-pat: GitHub rejects the token (401)"},
		{state.Notification{Kind: state.Firing, Problem: "certificate is not trusted", Expiry: now.Add(80 * 24 * time.Hour), At: now}, "GitHub token github-runner-pat: certificate is not trusted"},
		{state.Notification{Kind: state.Resolved, At: now}, "GitHub token github-runner-pat is ok again"},
		{state.Notification{Kind: state.CheckFailing, Error: "timeout", At: now}, "Could not check GitHub token github-runner-pat"},
	}
	for _, tt := range tests {
		if got := Render(tt.n, check, "s").Title; got != tt.want {
			t.Errorf("%s: title = %q, want %q", tt.n.Kind, got, tt.want)
		}
	}
}

func TestGrafanaPayload(t *testing.T) {
	m := Render(state.Notification{Kind: state.Resolved, At: now, Summary: "token expires on 2027-10-01"}, check, "homerun2-schedule-pitcher")
	p := GrafanaPayload(m)
	if p.Status != "resolved" || len(p.Alerts) != 1 || p.Receiver != "homerun2-schedule-pitcher" {
		t.Fatalf("payload = %+v", p)
	}
	a := p.Alerts[0]
	if a.Labels["alertname"] != m.Title || a.Labels["severity"] != "success" || a.Labels["check"] != check.ID ||
		a.Labels["assignee"] != "platform-team" || a.Annotations["summary"] != m.Text || a.GeneratorURL != check.URL {
		t.Fatalf("alert = %+v", a)
	}
	firing := GrafanaPayload(Render(state.Notification{Kind: state.Firing, Band: status.Warning, At: now}, check, "s"))
	if firing.Alerts[0].Fingerprint != a.Fingerprint {
		t.Error("firing and resolved of the same condition must share the fingerprint")
	}
	failing := GrafanaPayload(Render(state.Notification{Kind: state.CheckFailing, At: now}, check, "s"))
	if failing.Alerts[0].Fingerprint == a.Fingerprint {
		t.Error("could-not-check must have its own fingerprint")
	}
}

func TestHTTPPitch(t *testing.T) {
	for _, format := range []string{profile.FormatGrafana, profile.FormatGeneric} {
		t.Run(format, func(t *testing.T) {
			var body []byte
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				body, _ = io.ReadAll(r.Body)
			}))
			defer srv.Close()
			h, err := NewHTTP(srv.URL+"/pitch", format, "tok", "", false)
			if err != nil {
				t.Fatal(err)
			}
			m := Render(state.Notification{Kind: state.Firing, Band: status.Critical, Expiry: now, At: now}, check, "sys")
			if err := h.Pitch(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if auth != "Bearer tok" {
				t.Errorf("auth = %q", auth)
			}
			if format == profile.FormatGeneric {
				var msg homerun.Message
				_ = json.Unmarshal(body, &msg)
				if msg.Title != m.Title || msg.Severity != "critical" || msg.System != "sys" || msg.Author != Author ||
					msg.AssigneeName != "platform-team" || !strings.HasPrefix(msg.Tags, "check=github-runner-pat,type=github-token-expiry,expiry") {
					t.Errorf("message = %+v", msg)
				}
			} else {
				var p GrafanaWebhook
				_ = json.Unmarshal(body, &p)
				if p.Status != "firing" || p.Alerts[0].Labels["severity"] != "critical" {
					t.Errorf("payload = %s", body)
				}
			}
		})
	}
}

func TestHTTPPitchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	h, _ := NewHTTP(srv.URL, profile.FormatGeneric, "", "", false)
	err := h.Pitch(context.Background(), Message{At: now})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyFallsBackToHealth(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/ready" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h, _ := NewHTTP(srv.URL+"/pitch/grafana", profile.FormatGrafana, "", "", false)
	if err := h.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/ready,/health" {
		t.Errorf("paths = %v", paths)
	}
}

func TestProbeURL(t *testing.T) {
	tests := map[string]string{
		"https://omni.example/pitch/grafana": "https://omni.example/ready",
		"https://omni.example/pitch":         "https://omni.example/ready",
		"https://omni.example/":              "https://omni.example/ready",
		"http://h:8080/omni/pitch?x=1":       "http://h:8080/omni/ready",
	}
	for in, want := range tests {
		if got, _ := ProbeURL(in, "ready"); got != want {
			t.Errorf("ProbeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
