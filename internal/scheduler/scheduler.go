// Package scheduler runs the profile's checks on their schedules, evaluates
// the results and pitches what the state machine asks for.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/checks"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/metrics"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

// LockTTL bounds how long one run may hold a check's lock.
const LockTTL = 5 * time.Minute

// ErrBusy is returned when another run of the same check is in progress.
var ErrBusy = errors.New("check is already running")

// ErrUnknownCheck is returned for a check id that is not in the profile.
var ErrUnknownCheck = errors.New("unknown check")

type entry struct {
	check    profile.Check
	checker  checks.Checker
	schedule cron.Schedule
	remind   cron.Schedule
}

// Scheduler owns the checks of one profile.
type Scheduler struct {
	store   store.Store
	pitcher pitcher.Pitcher
	system  string
	loc     *time.Location
	now     func() time.Time

	order   []string
	entries map[string]*entry

	mu   sync.Mutex
	cron *cron.Cron
}

// New builds a scheduler. A check whose checker cannot be built (for example
// a missing CA file) is kept and reports "could not check" on every run.
func New(p *profile.SchedulePitcherProfile, st store.Store, pt pitcher.Pitcher, secrets checks.SecretResolver) (*Scheduler, error) {
	s := &Scheduler{
		store:   st,
		pitcher: pt,
		system:  p.Spec.Defaults.System,
		loc:     p.Location(),
		now:     time.Now,
		entries: map[string]*entry{},
	}
	for _, c := range p.Spec.Checks {
		sched, err := profile.ParseSchedule(c.Schedule, s.loc)
		if err != nil {
			return nil, fmt.Errorf("check %s: schedule: %w", c.ID, err)
		}
		remind, err := profile.ParseSchedule(c.Remind, s.loc)
		if err != nil {
			return nil, fmt.Errorf("check %s: remind: %w", c.ID, err)
		}
		checker, err := checks.New(c, secrets)
		if err != nil {
			slog.Error("check cannot be set up", "check", c.ID, "error", err)
			checker = brokenChecker{err: err}
		}
		s.order = append(s.order, c.ID)
		s.entries[c.ID] = &entry{check: c, checker: checker, schedule: sched, remind: remind}
	}
	return s, nil
}

type brokenChecker struct{ err error }

func (b brokenChecker) Run(context.Context) (checks.Result, error) {
	return checks.Result{}, fmt.Errorf("check setup failed: %w", b.err)
}

// Start runs every active check once and then on its schedule and its remind
// cadence, until ctx is done.
func (s *Scheduler) Start(ctx context.Context) {
	c := cron.New(cron.WithLocation(s.loc), cron.WithParser(profile.CronParser))
	for _, id := range s.order {
		e := s.entries[id]
		if e.check.Paused {
			slog.Info("check paused", "check", id)
			continue
		}
		job := cron.FuncJob(func() { s.runLogged(ctx, id) })
		c.Schedule(e.schedule, job)
		// Running on the remind cadence too makes reminders go out at the
		// configured time instead of at the next regular run.
		c.Schedule(e.remind, job)
	}
	s.mu.Lock()
	s.cron = c
	s.mu.Unlock()
	c.Start()

	go func() {
		for _, id := range s.order {
			if !s.entries[id].check.Paused {
				s.runLogged(ctx, id)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		<-c.Stop().Done()
	}()
}

func (s *Scheduler) runLogged(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	if _, err := s.Run(ctx, id); err != nil && !errors.Is(err, ErrBusy) {
		slog.Error("check run failed", "check", id, "error", err)
	}
}

// RunAll runs every active check once, in profile order.
func (s *Scheduler) RunAll(ctx context.Context) error {
	var errs []error
	for _, id := range s.order {
		if s.entries[id].check.Paused {
			continue
		}
		if _, err := s.Run(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Run runs one check now. The returned error is about running the check or
// pitching, not about what the check found.
func (s *Scheduler) Run(ctx context.Context, id string) (state.State, error) {
	e, ok := s.entries[id]
	if !ok {
		return state.State{}, ErrUnknownCheck
	}
	locked, err := s.store.TryLock(ctx, id, LockTTL)
	if err != nil {
		return state.State{}, fmt.Errorf("taking lock: %w", err)
	}
	if !locked {
		return state.State{}, ErrBusy
	}
	defer func() { _ = s.store.Unlock(context.WithoutCancel(ctx), id) }()

	prev, err := s.store.Get(ctx, id)
	if err != nil {
		return state.State{}, fmt.Errorf("reading state: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 3*e.check.Timeout.D())
	res, runErr := e.checker.Run(runCtx)
	cancel()

	next, notes := state.Evaluate(prev, res, runErr, s.now().In(s.loc), e.check.Thresholds, e.remind)
	logRun(id, next, runErr)

	var pitchErrs []error
	var pitched []string
	for _, n := range notes {
		m := pitcher.Render(n, e.check, s.system)
		err := s.pitcher.Pitch(ctx, m)
		metrics.Pitched(err)
		if err != nil {
			pitchErrs = append(pitchErrs, err)
			continue
		}
		pitched = append(pitched, string(n.Kind))
		slog.Info("pitched", "check", id, "kind", n.Kind, "severity", m.Severity, "title", m.Title)
	}
	if len(pitchErrs) > 0 {
		// Keep the previous delivery bookkeeping so the next run tries again.
		next.PitchedBand = prev.PitchedBand
		next.PitchedAt = prev.PitchedAt
		next.FailPitchedAt = prev.FailPitchedAt
		next.NoExpiryNotified = prev.NoExpiryNotified
	}

	if err := s.store.Put(ctx, next); err != nil {
		return next, fmt.Errorf("writing state: %w", err)
	}
	if err := s.store.AddHistory(ctx, id, store.EntryFor(next, pitched)); err != nil {
		slog.Warn("writing history failed", "check", id, "error", err)
	}
	metrics.ObserveState(next)
	return next, errors.Join(pitchErrs...)
}

func logRun(id string, s state.State, runErr error) {
	if runErr != nil {
		slog.Warn("could not check", "check", id, "error", runErr)
		return
	}
	attrs := []any{"check", id, "band", s.Band.String(), "summary", s.Summary}
	if !s.Expiry.IsZero() {
		attrs = append(attrs, "expiry", s.Expiry.Format(time.RFC3339))
	}
	slog.Info("check ran", attrs...)
}

// CheckStatus is a check with its state and next run, for the API and UI.
type CheckStatus struct {
	Check   profile.Check `json:"-"`
	State   state.State   `json:"state"`
	Band    string        `json:"band"`
	NextRun *time.Time    `json:"nextRun,omitempty"`
}

// History returns the last runs of a check, newest first.
func (s *Scheduler) History(ctx context.Context, id string, limit int) ([]store.HistoryEntry, error) {
	if _, ok := s.entries[id]; !ok {
		return nil, ErrUnknownCheck
	}
	return s.store.History(ctx, id, limit)
}

// Statuses returns all checks in profile order.
func (s *Scheduler) Statuses(ctx context.Context) ([]CheckStatus, error) {
	now := s.now().In(s.loc)
	out := make([]CheckStatus, 0, len(s.order))
	for _, id := range s.order {
		e := s.entries[id]
		st, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		cs := CheckStatus{Check: e.check, State: st, Band: st.Band.String()}
		if !e.check.Paused {
			next := e.schedule.Next(now)
			if r := e.remind.Next(now); r.Before(next) {
				next = r
			}
			cs.NextRun = &next
		}
		out = append(out, cs)
	}
	return out, nil
}
