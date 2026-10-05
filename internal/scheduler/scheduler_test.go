package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/checks"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

const testProfile = `
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  checks:
    - {id: pat, type: github-token-expiry, tokenFrom: {env: SP_UNUSED}}
    - {id: tls, type: tls-endpoint, target: 'example.test:443', paused: true}
`

type fakeChecker struct {
	res checks.Result
	err error
}

func (f *fakeChecker) Run(context.Context) (checks.Result, error) { return f.res, f.err }

// noSecrets fails every secret lookup, so checks built after Sync never
// reach a real API.
type noSecrets struct{}

func (noSecrets) Resolve(context.Context, *profile.ValueFrom) (string, error) {
	return "", errors.New("no secrets in tests")
}

type recorder struct {
	msgs []pitcher.Message
	err  error
}

func (r *recorder) Pitch(_ context.Context, m pitcher.Message) error {
	if r.err != nil {
		return r.err
	}
	r.msgs = append(r.msgs, m)
	return nil
}

func setup(t *testing.T) (*Scheduler, *fakeChecker, *recorder, *time.Time) {
	t.Helper()
	p, err := profile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s, err := New(p, store.NewMemory(), rec, noSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, s.loc)
	s.now = func() time.Time { return now }
	fc := &fakeChecker{}
	s.entries["pat"].checker = fc
	return s, fc, rec, &now
}

func TestRunPitchesOnceAndRetriesFailedDelivery(t *testing.T) {
	s, fc, rec, now := setup(t)
	ctx := context.Background()
	fc.res = checks.Result{Expiry: now.Add(5 * 24 * time.Hour), Summary: "token expires"}

	rec.err = errors.New("omni down")
	if _, err := s.Run(ctx, "pat"); err == nil {
		t.Fatal("expected delivery error")
	}
	rec.err = nil
	*now = now.Add(time.Hour)
	st, err := s.Run(ctx, "pat")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || rec.msgs[0].Severity != "error" || st.PitchedBand != status.Error {
		t.Fatalf("msgs = %+v, state = %+v", rec.msgs, st)
	}
	*now = now.Add(time.Hour)
	if _, err := s.Run(ctx, "pat"); err != nil || len(rec.msgs) != 1 {
		t.Fatalf("second run pitched again: %d msgs, %v", len(rec.msgs), err)
	}
	h, _ := s.History(ctx, "pat", 10)
	if len(h) != 3 || len(h[0].Pitched) != 0 || len(h[1].Pitched) != 1 || h[1].Pitched[0] != "firing" || len(h[2].Pitched) != 0 {
		t.Fatalf("history = %+v", h)
	}
}

func TestRunErrors(t *testing.T) {
	s, _, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Run(ctx, "nope"); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("err = %v", err)
	}
	_, _ = s.store.TryLock(ctx, "pat", time.Minute)
	if _, err := s.Run(ctx, "pat"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAllAndStatuses(t *testing.T) {
	s, fc, rec, now := setup(t)
	ctx := context.Background()
	fc.res = checks.Result{NoExpiry: true, Summary: "token has no expiration date"}
	if err := s.RunAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || rec.msgs[0].Severity != "info" {
		t.Fatalf("msgs = %+v", rec.msgs)
	}
	sts, err := s.Statuses(ctx)
	if err != nil || len(sts) != 2 {
		t.Fatalf("statuses = %+v, %v", sts, err)
	}
	if sts[0].Band != "ok" || sts[0].NextRun == nil || !sts[0].NextRun.Equal(time.Date(2026, 10, 5, 18, 0, 0, 0, s.loc)) {
		t.Errorf("pat status = %+v next %v", sts[0], sts[0].NextRun)
	}
	if sts[1].Band != "unknown" || sts[1].NextRun != nil || !sts[1].State.LastRun.IsZero() {
		t.Errorf("paused check ran or has next run: %+v", sts[1])
	}
	_ = now
}

func TestBrokenCheckerReportsCouldNotCheck(t *testing.T) {
	p, _ := profile.Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  checks:
    - {id: tls, type: tls-endpoint, target: 'example.test:443', caFile: /does/not/exist}
`))
	rec := &recorder{}
	s, err := New(p, store.NewMemory(), rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Run(context.Background(), "tls")
	if !st.Failing || len(rec.msgs) != 1 || rec.msgs[0].Title != "Could not check TLS certificate of example.test:443" {
		t.Fatalf("state = %+v msgs = %+v", st, rec.msgs)
	}
}

func TestDeliverAsFindings(t *testing.T) {
	s, fc, rec, now := setup(t)
	ctx := context.Background()
	reports := 0
	s.DeliverAsFindings(func(context.Context) { reports++ })
	fc.res = checks.Result{Expiry: now.Add(2 * 24 * time.Hour)}

	if _, err := s.Run(ctx, "pat"); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 0 || reports != 1 {
		t.Fatalf("pitched %d, reported %d", len(rec.msgs), reports)
	}
	if st, _ := s.store.Get(ctx, "pat"); st.Band != status.Critical { // 2 days left, PAT critical at 3d
		t.Fatalf("state not kept: %+v", st)
	}
	if _, err := s.Run(ctx, "nope"); err == nil || reports != 1 {
		t.Fatalf("unknown check reported: %d", reports)
	}
	ok, _ := s.AllRan(ctx)
	if !ok {
		t.Fatal("AllRan = false although the only active check ran (tls is paused)")
	}
}

func TestSync(t *testing.T) {
	s, _, _, _ := setup(t)
	ctx := context.Background()
	p, _ := profile.Parse([]byte(testProfile))
	extra, err := p.CompleteCheck(profile.Check{ID: "found", Type: profile.TypeTLSEndpoint, Target: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if extra.Origin != profile.OriginDiscovered || extra.Target != "example.test:443" {
		t.Fatalf("completed = %+v", extra)
	}

	if err := s.Sync(append(p.Spec.Checks, extra)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Checks()); got != 3 {
		t.Fatalf("checks = %d", got)
	}
	if _, err := s.Run(ctx, "found"); err != nil && errors.Is(err, ErrUnknownCheck) {
		t.Fatal("added check unknown")
	}

	// Removing it: unknown afterwards, other checks untouched.
	if err := s.Sync(p.Spec.Checks); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, "found"); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("removed check still runs: %v", err)
	}
	sts, _ := s.Statuses(ctx)
	if len(sts) != 2 || sts[0].Check.ID != "pat" {
		t.Fatalf("statuses = %+v", sts)
	}
}

func TestSyncWhileRunning(t *testing.T) {
	s, fc, _, now := setup(t)
	fc.res = checks.Result{Expiry: now.Add(100 * 24 * time.Hour)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 10)
	s.DeliverAsFindings(func(context.Context) { ran <- struct{}{} })
	s.Start(ctx)
	<-ran // initial run of pat

	p, _ := profile.Parse([]byte(testProfile))
	changed := p.Spec.Checks
	changed[0].Description = "changed"
	if err := s.Sync(changed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran: // the changed check runs right away
	case <-time.After(5 * time.Second):
		t.Fatal("changed check did not run")
	}
	if s.Checks()[0].Description != "changed" {
		t.Fatal("not replaced")
	}
}
