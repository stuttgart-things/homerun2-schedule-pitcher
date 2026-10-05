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
	s, err := New(p, store.NewMemory(), rec, nil)
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
