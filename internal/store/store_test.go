package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

func newRedis(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedis(client, ""), mr
}

func stores(t *testing.T) map[string]Store {
	r, _ := newRedis(t)
	return map[string]Store{"memory": NewMemory(), "redis": r}
}

func TestStateRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 5, 8, 0, 0, 123, time.UTC)
	want := state.State{
		CheckID: "pat", Band: status.Error, LastRun: ts, LastOK: ts.Add(-time.Hour), Expiry: ts.Add(72 * time.Hour),
		Summary: "token expires", Subject: "octocat", Problem: "p", Failing: true, FailingSince: ts,
		LastError: "boom", PitchedBand: status.Error, PitchedAt: ts, FailPitchedAt: ts, NoExpiryNotified: true,
	}
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			got, err := s.Get(ctx, "pat")
			if err != nil || got != state.New("pat") {
				t.Fatalf("empty Get = %+v, %v", got, err)
			}
			if err := s.Put(ctx, want); err != nil {
				t.Fatal(err)
			}
			got, err = s.Get(ctx, "pat")
			if err != nil {
				t.Fatal(err)
			}
			if !got.LastRun.Equal(want.LastRun) || !got.Expiry.Equal(want.Expiry) {
				t.Fatalf("times differ: %+v", got)
			}
			// Compare without monotonic/location details.
			got.LastRun, got.LastOK, got.Expiry, got.FailingSince, got.PitchedAt, got.FailPitchedAt = want.LastRun, want.LastOK, want.Expiry, want.FailingSince, want.PitchedAt, want.FailPitchedAt
			if got != want {
				t.Fatalf("Get = %+v\nwant %+v", got, want)
			}

			// Fields that became empty are cleared.
			cleared := want
			cleared.Failing, cleared.LastError, cleared.FailingSince = false, "", time.Time{}
			_ = s.Put(ctx, cleared)
			got, _ = s.Get(ctx, "pat")
			if got.Failing || got.LastError != "" || !got.FailingSince.IsZero() {
				t.Fatalf("not cleared: %+v", got)
			}
		})
	}
}

func TestHistory(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
			for i := range HistoryLimit + 20 {
				e := HistoryEntry{At: base.Add(time.Duration(i) * time.Hour), Band: status.Warning, Summary: "run"}
				if i == HistoryLimit+19 {
					e.Pitched = []string{"firing"}
				}
				if err := s.AddHistory(ctx, "pat", e); err != nil {
					t.Fatal(err)
				}
			}
			h, err := s.History(ctx, "pat", 5)
			if err != nil || len(h) != 5 {
				t.Fatalf("History = %d entries, %v", len(h), err)
			}
			if !h[0].At.Equal(base.Add(time.Duration(HistoryLimit+19)*time.Hour)) || h[0].Band != status.Warning || h[0].Pitched[0] != "firing" {
				t.Fatalf("newest = %+v", h[0])
			}
			all, _ := s.History(ctx, "pat", 1000)
			if len(all) < HistoryLimit || len(all) > HistoryLimit+20 {
				t.Fatalf("kept %d entries", len(all))
			}
			if none, _ := s.History(ctx, "other", 5); len(none) != 0 {
				t.Fatalf("other = %v", none)
			}
		})
	}
}

func TestLocks(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if ok, err := s.TryLock(ctx, "pat", time.Minute); !ok || err != nil {
				t.Fatalf("first lock: %v %v", ok, err)
			}
			if ok, _ := s.TryLock(ctx, "pat", time.Minute); ok {
				t.Fatal("second lock succeeded")
			}
			if ok, _ := s.TryLock(ctx, "tls", time.Minute); !ok {
				t.Fatal("lock of another check failed")
			}
			_ = s.Unlock(ctx, "pat")
			if ok, _ := s.TryLock(ctx, "pat", time.Minute); !ok {
				t.Fatal("lock after unlock failed")
			}
			if err := s.Ping(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRedisLockOwnershipAndExpiry(t *testing.T) {
	a, mr := newRedis(t)
	b := NewRedis(a.client, "")
	ctx := context.Background()

	if ok, _ := a.TryLock(ctx, "pat", time.Minute); !ok {
		t.Fatal("a could not lock")
	}
	// b must not release a's lock.
	_ = b.Unlock(ctx, "pat")
	if ok, _ := b.TryLock(ctx, "pat", time.Minute); ok {
		t.Fatal("b took a's lock")
	}
	mr.FastForward(2 * time.Minute)
	if ok, _ := b.TryLock(ctx, "pat", time.Minute); !ok {
		t.Fatal("lock did not expire")
	}
	// a's late unlock must not release b's lock.
	_ = a.Unlock(ctx, "pat")
	if !mr.Exists(DefaultPrefix + ":lock:pat") {
		t.Fatal("a released b's lock")
	}
}

func TestRedisKeyLayout(t *testing.T) {
	r, mr := newRedis(t)
	ctx := context.Background()
	_ = r.Put(ctx, state.State{CheckID: "pat", Band: status.Warning, PitchedBand: status.Warning})
	_ = r.AddHistory(ctx, "pat", HistoryEntry{At: time.Now()})
	if got := mr.HGet("homerun2-schedule-pitcher:check:pat:state", "band"); got != "warning" {
		t.Errorf("band field = %q", got)
	}
	if !mr.Exists("homerun2-schedule-pitcher:check:pat:history") {
		t.Error("history stream missing")
	}

	p := NewRedis(r.client, "custom")
	_ = p.Put(ctx, state.New("x"))
	if !mr.Exists("custom:check:x:state") {
		t.Error("prefix not used")
	}
}

func TestRedisPingFails(t *testing.T) {
	r, mr := newRedis(t)
	mr.Close()
	if err := r.Ping(context.Background()); err == nil {
		t.Fatal("ping succeeded against a closed server")
	}
}
