package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

var now = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

func cs(id, typ string, mutate func(*state.State)) scheduler.CheckStatus {
	c := profile.Check{ID: id, Type: typ, Target: "omni.example:443", Tags: []string{"expiry"}, URL: "https://runbook",
		Thresholds: profile.TypeThresholds[profile.TypeGitHubTokenExpiry]}
	st := state.New(id)
	st.LastRun, st.Band = now, status.OK
	if mutate != nil {
		mutate(&st)
	}
	return scheduler.CheckStatus{Check: c, State: st, Band: st.Band.String()}
}

func TestFromStatuses(t *testing.T) {
	sts := []scheduler.CheckStatus{
		cs("ok-token", profile.TypeGitHubTokenExpiry, nil),
		cs("never-ran", profile.TypeGitHubTokenExpiry, func(s *state.State) { s.LastRun = time.Time{}; s.Band = status.Unknown }),
		cs("pat", profile.TypeGitHubTokenExpiry, func(s *state.State) {
			s.Band, s.Expiry, s.Subject, s.Summary = status.Error, now.Add(10*24*time.Hour+3*time.Hour), "octocat", "token expires"
		}),
		cs("tls", profile.TypeTLSEndpoint, func(s *state.State) {
			s.Band, s.Expiry, s.Failing, s.LastError = status.Warning, now.Add(20*24*time.Hour), true, "dial timeout"
		}),
		cs("down", profile.TypeTLSEndpoint, func(s *state.State) {
			s.Band, s.Failing, s.LastError = status.OK, true, "connection refused"
		}),
	}
	sts = append(sts, cs("paused", profile.TypeGitHubTokenExpiry, func(s *state.State) { s.Band = status.Critical }))
	sts[len(sts)-1].Check.Paused = true

	r := FromStatuses("checks-machinery", sts, now, "sys")
	if err := r.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	got := map[string]findings.ReportedItem{}
	for _, f := range r.Findings {
		got[f.Key] = f
	}
	if len(got) != 4 {
		t.Fatalf("findings = %v", r.Findings)
	}
	pat := got["pat"]
	if pat.Severity != "error" || pat.Title != "GitHub token pat (octocat) expires in 10 days (2026-10-15)" ||
		*pat.Value != 10.1 || *pat.Threshold != 14 || pat.URL != "https://runbook" || pat.Tags[0] != "check" {
		t.Errorf("pat = %+v (value %v threshold %v)", pat, *pat.Value, *pat.Threshold)
	}
	if f := got["tls"]; f.Severity != "warning" || f.Host != "omni.example" {
		t.Errorf("tls = %+v", f)
	}
	if f := got["tls"+CouldNotCheckSuffix]; f.Severity != "warning" || f.Title != "Could not check TLS certificate of omni.example:443" {
		t.Errorf("tls could-not-check = %+v", f)
	}
	if _, ok := got["down"+CouldNotCheckSuffix]; !ok {
		t.Error("failing ok check must report could-not-check")
	}
	if r.Source != "checks-machinery" || r.Run == "" {
		t.Errorf("source/run = %q/%q", r.Source, r.Run)
	}
}

type fakeStatuser struct {
	sts    []scheduler.CheckStatus
	allRan bool
}

func (f fakeStatuser) Statuses(context.Context) ([]scheduler.CheckStatus, error) { return f.sts, nil }
func (f fakeStatuser) AllRan(context.Context) (bool, error)                      { return f.allRan, nil }

type captureSender struct{ reports []findings.Report }

func (c *captureSender) Send(_ context.Context, r findings.Report) error {
	c.reports = append(c.reports, r)
	return nil
}

func TestReporterWaitsForAllChecks(t *testing.T) {
	cap := &captureSender{}
	r := &Reporter{Scheduler: fakeStatuser{allRan: false}, Sender: cap, Source: "s", Now: func() time.Time { return now }}
	if err := r.Report(context.Background()); err != nil || len(cap.reports) != 0 {
		t.Fatalf("reported before all checks ran: %v %d", err, len(cap.reports))
	}
	r.Scheduler = fakeStatuser{allRan: true}
	if err := r.Report(context.Background()); err != nil || len(cap.reports) != 1 {
		t.Fatalf("not reported: %v %d", err, len(cap.reports))
	}
}

func TestHTTPSender(t *testing.T) {
	calls := 0
	var got findings.Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if calls == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	h := HTTP{Addr: srv.URL + "/findings", Token: "tok", Client: srv.Client()}
	if err := h.Send(context.Background(), findings.Report{Source: "checks-x"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || got.Source != "checks-x" {
		t.Fatalf("calls = %d, got %+v", calls, got)
	}
	h.Token = "wrong"
	if err := h.Send(context.Background(), findings.Report{Source: "checks-x"}); err == nil {
		t.Fatal("expected 401 error")
	}
}

type busyIngester struct{ calls int }

func (b *busyIngester) Ingest(context.Context, findings.Report) (findings.IngestResult, error) {
	b.calls++
	if b.calls < 3 {
		return findings.IngestResult{}, findings.ErrBusy
	}
	return findings.IngestResult{}, nil
}

func TestLocalRetriesBusy(t *testing.T) {
	b := &busyIngester{}
	if err := (Local{Service: b}).Send(context.Background(), findings.Report{Source: "s"}); err != nil || b.calls != 3 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
	if err := (Local{Service: b}).Send(context.Background(), findings.Report{Source: "bad source"}); err == nil {
		t.Fatal("invalid report accepted")
	}
}
