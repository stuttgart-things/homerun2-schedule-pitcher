// Package report turns check results into findings and sends them, either to
// the local findings service (central instance) or to a central instance's
// POST /findings (agent).
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// CouldNotCheckSuffix is appended to the key of a check that cannot complete.
const CouldNotCheckSuffix = ":could-not-check"

// FromStatuses builds the complete report of a set of checks: one finding per
// check that is not ok, and one "could not check" finding per failing check.
// Checks that are ok, paused or never ran are absent, so their findings
// resolve.
func FromStatuses(source string, statuses []scheduler.CheckStatus, now time.Time, system string) findings.Report {
	r := findings.Report{Source: source, Run: source + "-" + now.UTC().Format("20060102T150405Z")}
	for _, cs := range statuses {
		c, st := cs.Check, cs.State
		if c.Paused || st.LastRun.IsZero() {
			continue
		}
		tags := append([]string{"check", c.Type}, c.Tags...)
		host := hostOf(c)

		if st.Failing {
			n := state.Notification{Kind: state.CheckFailing, CheckID: c.ID, Error: st.LastError, At: now}
			m := pitcher.Render(n, c, system)
			r.Findings = append(r.Findings, findings.ReportedItem{
				Key: c.ID + CouldNotCheckSuffix, Title: m.Title, Message: m.Text,
				Severity: findings.SeverityWarning, Host: host, Tags: tags, URL: c.URL,
			})
			// The last known band is kept only while it is not ok; it is
			// reported too, so a known expiry does not drop off the list.
		}
		if st.Band <= status.OK {
			continue
		}
		n := state.Notification{
			Kind: state.Firing, CheckID: c.ID, Band: st.Band, Summary: st.Summary,
			Subject: st.Subject, Problem: st.Problem, Expiry: st.Expiry, At: now,
		}
		m := pitcher.Render(n, c, system)
		it := findings.ReportedItem{
			Key: c.ID, Title: m.Title, Message: m.Text, Severity: st.Band.String(),
			Host: host, Tags: tags, URL: c.URL,
		}
		if !st.Expiry.IsZero() {
			it.Value = ptr(days(st.Expiry.Sub(now)))
			it.Threshold = ptr(days(threshold(c.Thresholds, st.Band)))
		}
		r.Findings = append(r.Findings, it)
	}
	return r
}

func hostOf(c profile.Check) string {
	switch c.Type {
	case profile.TypeTLSEndpoint:
		if h, _, err := net.SplitHostPort(c.Target); err == nil {
			return h
		}
	case profile.TypeVaultTokenTTL:
		if u, err := url.Parse(c.Addr); err == nil {
			return u.Hostname()
		}
	}
	return ""
}

func threshold(t profile.Thresholds, b status.Band) time.Duration {
	switch b {
	case status.Critical:
		return t.Critical.D()
	case status.Error:
		return t.Error.D()
	default:
		return t.Warning.D()
	}
}

// days rounds to one decimal.
func days(d time.Duration) float64 {
	return math.Round(d.Hours()/24*10) / 10
}

func ptr(v float64) *float64 { return &v }

// Sender delivers a report.
type Sender interface {
	Send(ctx context.Context, r findings.Report) error
}

// Ingester is the local findings service.
type Ingester interface {
	Ingest(ctx context.Context, r findings.Report) (findings.IngestResult, error)
}

// Local sends to the findings service of this instance. A report that finds
// the source busy is retried briefly.
type Local struct{ Service Ingester }

func (l Local) Send(ctx context.Context, r findings.Report) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		_, err := l.Service.Ingest(ctx, r)
		if !errors.Is(err, findings.ErrBusy) || attempt == 10 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// HTTP posts to a central instance's POST /findings.
type HTTP struct {
	Addr   string
	Token  string
	Client *http.Client
}

func (h HTTP) Send(ctx context.Context, r findings.Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		code, msg, err := h.post(ctx, body)
		// 409: another report of this source is being applied right now.
		if err == nil && code == http.StatusConflict && attempt < 5 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if err != nil {
			return err
		}
		if code < 200 || code >= 300 {
			return fmt.Errorf("POST %s returned %d: %s", h.Addr, code, msg)
		}
		return nil
	}
}

func (h HTTP) post(ctx context.Context, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Addr, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("POST %s: %w", h.Addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

// Statuser is what Reporter needs from the scheduler.
type Statuser interface {
	Statuses(ctx context.Context) ([]scheduler.CheckStatus, error)
	AllRan(ctx context.Context) (bool, error)
}

// Reporter sends the complete check results after a run.
type Reporter struct {
	Scheduler Statuser
	Sender    Sender
	Source    string
	System    string
	Now       func() time.Time
}

// Report sends the current results. It waits until every active check ran
// once, so a fresh start does not resolve findings of checks still pending.
func (r *Reporter) Report(ctx context.Context) error {
	ok, err := r.Scheduler.AllRan(ctx)
	if err != nil || !ok {
		return err
	}
	sts, err := r.Scheduler.Statuses(ctx)
	if err != nil {
		return err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	rep := FromStatuses(r.Source, sts, now(), r.System)
	if err := r.Sender.Send(ctx, rep); err != nil {
		return fmt.Errorf("reporting %s: %w", r.Source, err)
	}
	slog.Info("check results reported", "source", r.Source, "findings", len(rep.Findings))
	return nil
}

// AfterRun adapts Report to scheduler.DeliverAsFindings and logs failures;
// the next run reports the complete set again.
func (r *Reporter) AfterRun(ctx context.Context) {
	if err := r.Report(ctx); err != nil {
		slog.Error("reporting check results failed", "error", err)
	}
}
