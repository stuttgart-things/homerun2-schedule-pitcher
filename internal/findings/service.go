package findings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/metrics"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
)

// Locker serialises work across replicas (the check store implements it).
type Locker interface {
	TryLock(ctx context.Context, id string, ttl time.Duration) (bool, error)
	Unlock(ctx context.Context, id string) error
}

// Config controls delivery and housekeeping.
type Config struct {
	Hours OfficeHours
	// AckExpiry re-opens an acknowledged finding after this long.
	AckExpiry time.Duration
	// Retention deletes resolved findings after this long.
	Retention time.Duration
	System    string
	Assignee  string
}

// ErrBusy is returned when a report of the same source is being applied.
var ErrBusy = errors.New("a report of this source is being processed")

// ErrNotFound is returned for an unknown finding.
var ErrNotFound = errors.New("finding not found")

// Service ingests reports, pitches immediate findings and sends the
// office-hours messages.
type Service struct {
	store   Store
	locker  Locker
	pitcher pitcher.Pitcher
	cfg     Config
	now     func() time.Time
}

func NewService(st Store, locker Locker, p pitcher.Pitcher, cfg Config) *Service {
	return &Service{store: st, locker: locker, pitcher: p, cfg: cfg, now: time.Now}
}

const lockTTL = time.Minute

func (s *Service) withLock(ctx context.Context, id string, fn func() error) error {
	ok, err := s.locker.TryLock(ctx, id, lockTTL)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBusy
	}
	defer func() { _ = s.locker.Unlock(context.WithoutCancel(ctx), id) }()
	return fn()
}

// IngestResult summarises what a report changed.
type IngestResult struct {
	Source   string `json:"source"`
	Open     int    `json:"open"`
	New      int    `json:"new"`
	Reopened int    `json:"reopened"`
	Worse    int    `json:"worse"`
	Resolved int    `json:"resolved"`
	Pitched  int    `json:"pitched"`
}

// Ingest applies a validated report.
func (s *Service) Ingest(ctx context.Context, r Report) (IngestResult, error) {
	res := IngestResult{Source: r.Source}
	err := s.withLock(ctx, "findings:"+r.Source, func() error {
		now := s.now()
		stored, err := s.store.List(ctx, r.Source)
		if err != nil {
			return err
		}
		prev := make(map[string]Finding, len(stored))
		for _, f := range stored {
			prev[f.Key] = f
		}
		updated, events := Apply(prev, r, now)
		if err := s.store.Save(ctx, updated...); err != nil {
			return err
		}

		var notified []Finding
		for _, e := range events {
			switch e.Change {
			case ChangeNew:
				res.New++
			case ChangeReopened:
				res.Reopened++
			case ChangeWorse:
				res.Worse++
			case ChangeResolved:
				res.Resolved++
			}
			if !Immediate(e, now, s.cfg.Hours) {
				continue
			}
			err := s.pitcher.Pitch(ctx, s.renderFinding(e, now))
			metrics.Pitched(err)
			if err != nil {
				// Not marked as notified: the next update carries it.
				slog.Error("pitching finding failed", "finding", e.Finding.ID(), "error", err)
				continue
			}
			f := e.Finding
			if e.Change == ChangeResolved {
				f.ResolvedNotifiedAt = now
			} else {
				f.NotifiedAt = now
			}
			notified = append(notified, f)
			res.Pitched++
		}
		for _, f := range updated {
			if f.IsOpen() {
				res.Open++
			}
		}
		return s.store.Save(ctx, notified...)
	})
	if err == nil {
		slog.Info("findings report applied", "source", r.Source, "run", r.Run, "open", res.Open,
			"new", res.New, "reopened", res.Reopened, "worse", res.Worse, "resolved", res.Resolved, "pitched", res.Pitched)
	}
	return res, err
}

// Acknowledge marks a finding as being worked on.
func (s *Service) Acknowledge(ctx context.Context, source, key, by, note string) (Finding, error) {
	var out Finding
	err := s.withLock(ctx, "findings:"+source, func() error {
		f, ok, err := s.store.Get(ctx, source, key)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if !f.IsOpen() {
			return fmt.Errorf("finding is resolved")
		}
		f.Status = StatusAcknowledged
		f.AckBy, f.AckNote, f.AckAt, f.AckExpiredAt = by, note, s.now(), time.Time{}
		out = f
		return s.store.Save(ctx, f)
	})
	return out, err
}

// List returns findings filtered by status and source ("" = all), worst
// first, then oldest first.
func (s *Service) List(ctx context.Context, status, source string) ([]Finding, error) {
	all, err := s.store.List(ctx, source)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, f := range all {
		switch {
		case status == "":
		case status == "open" && f.IsOpen():
		case f.Status == status:
		default:
			continue
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b Finding) int {
		if Rank(a.Severity) != Rank(b.Severity) {
			return Rank(b.Severity) - Rank(a.Severity)
		}
		return a.OpenSince().Compare(b.OpenSince())
	})
	return out, nil
}

// Start runs Tick at the top of every hour until ctx is done.
func (s *Service) Start(ctx context.Context) {
	c := cron.New(cron.WithLocation(s.cfg.Hours.Loc))
	_, _ = c.AddFunc("0 * * * *", func() {
		if err := s.Tick(ctx); err != nil && !errors.Is(err, ErrBusy) {
			slog.Error("findings delivery failed", "error", err)
		}
	})
	c.Start()
	go func() {
		<-ctx.Done()
		<-c.Stop().Done()
	}()
}

// Tick does the hourly housekeeping and sends the message that is due:
// the start-of-day summary at Start, the end-of-day summary at End, and an
// update for the hours in between.
func (s *Service) Tick(ctx context.Context) error {
	return s.withLock(ctx, "findings-delivery", func() error {
		now := s.now().In(s.cfg.Hours.Loc)
		if err := s.housekeeping(ctx, now); err != nil {
			return err
		}
		d, err := s.store.GetDelivery(ctx)
		if err != nil {
			return err
		}
		kind, since := s.due(d, now)
		if kind == "" {
			return nil
		}

		all, err := s.store.List(ctx, "")
		if err != nil {
			return err
		}
		digest := Build(kind, all, since, now)
		if !digest.Empty() {
			err := s.pitcher.Pitch(ctx, s.renderDigest(digest))
			metrics.Pitched(err)
			if err != nil {
				// The next tick covers this window again: summaries are due
				// until sent, and an update starts at LastUpdate.
				if d.LastUpdate.IsZero() {
					d.LastUpdate = since
					_ = s.store.PutDelivery(ctx, d)
				}
				return fmt.Errorf("pitching %s: %w", kind, err)
			}
			slog.Info("findings message pitched", "kind", kind, "title", digest.Title())
		}
		switch kind {
		case KindStartOfDay:
			d.LastStartOfDay = now
		case KindEndOfDay:
			d.LastEndOfDay = now
		}
		d.LastUpdate = now
		return s.store.PutDelivery(ctx, d)
	})
}

// due decides which message the tick at now sends, and since when.
// A summary that could not be sent stays due for the rest of the day (the
// start-of-day one until End, the end-of-day one until midnight).
func (s *Service) due(d Delivery, now time.Time) (DigestKind, time.Time) {
	h, start, end := now.Hour(), s.cfg.Hours.Start, s.cfg.Hours.End
	today := func(t time.Time) bool { return !t.IsZero() && sameDay(t.In(now.Location()), now) }
	switch {
	case h >= start && h < end && !today(d.LastStartOfDay):
		since := d.LastEndOfDay
		if since.IsZero() {
			since = now.Add(-time.Duration(24-end+start) * time.Hour)
		}
		return KindStartOfDay, since
	case h >= end && !today(d.LastEndOfDay) && today(d.LastStartOfDay):
		return KindEndOfDay, d.LastStartOfDay
	case h > start && h < end && !sameHour(d.LastUpdate, now):
		since := d.LastUpdate
		if since.IsZero() {
			since = now.Add(-time.Hour)
		}
		return KindUpdate, since
	}
	return "", time.Time{}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func sameHour(a, b time.Time) bool {
	return !a.IsZero() && a.In(b.Location()).Truncate(time.Hour).Equal(b.Truncate(time.Hour))
}

// housekeeping expires acknowledgements and deletes old resolved findings.
func (s *Service) housekeeping(ctx context.Context, now time.Time) error {
	all, err := s.store.List(ctx, "")
	if err != nil {
		return err
	}
	var expired, gone []Finding
	for _, f := range all {
		switch {
		case f.Status == StatusAcknowledged && s.cfg.AckExpiry > 0 && !now.Before(f.AckAt.Add(s.cfg.AckExpiry)):
			f.Status = StatusOpen
			f.AckExpiredAt = now
			expired = append(expired, f)
		case f.Status == StatusResolved && s.cfg.Retention > 0 && !now.Before(f.ResolvedAt.Add(s.cfg.Retention)):
			gone = append(gone, f)
		}
	}
	if err := s.store.Save(ctx, expired...); err != nil {
		return err
	}
	if len(expired)+len(gone) > 0 {
		slog.Info("findings housekeeping", "acknowledgements_expired", len(expired), "deleted", len(gone))
	}
	return s.store.Delete(ctx, gone...)
}

func (s *Service) renderFinding(e Event, now time.Time) pitcher.Message {
	f := e.Finding
	title := f.Title
	if f.Host != "" && !strings.Contains(title, f.Host) {
		title = f.Host + ": " + title
	}
	lines := []string{}
	if f.Message != "" {
		lines = append(lines, f.Message)
	}
	severity, resolved := f.Severity, false
	switch e.Change {
	case ChangeReopened:
		lines = append(lines, "Reopened: this finding was resolved before.")
	case ChangeWorse:
		lines = append(lines, "Got worse.")
	case ChangeResolved:
		// Same alert name, so a Grafana-style output pairs it with the alarm.
		severity, resolved = "success", true
		lines = append(lines, fmt.Sprintf("Resolved after %s (was %s).", Age(f.ResolvedAt.Sub(f.OpenSince())), f.Severity))
	}
	if f.Value != nil {
		v := fmt.Sprintf("Value: %g", *f.Value)
		if f.Threshold != nil {
			v += fmt.Sprintf(" (threshold %g)", *f.Threshold)
		}
		lines = append(lines, v)
	}
	lines = append(lines, "Source: "+f.Source+", key: "+f.Key)
	return pitcher.Message{
		AlertName: "finding:" + f.ID(),
		Title:     title,
		Text:      strings.Join(lines, "\n"),
		Severity:  severity,
		Resolved:  resolved,
		Source:    f.Source,
		Key:       f.Key,
		Type:      "finding",
		System:    s.cfg.System,
		Tags:      f.Tags,
		URL:       f.URL,
		Assignee:  s.cfg.Assignee,
		At:        now,
	}
}

func (s *Service) renderDigest(d Digest) pitcher.Message {
	return pitcher.Message{
		AlertName: "findings:" + string(d.Kind),
		Title:     d.Title(),
		Text:      d.Text(),
		Severity:  d.Severity(),
		Type:      "findings-" + string(d.Kind),
		System:    s.cfg.System,
		Tags:      []string{"findings"},
		Assignee:  s.cfg.Assignee,
		At:        d.At,
	}
}
