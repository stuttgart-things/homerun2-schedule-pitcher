package state

import (
	"errors"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/checks"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

const day = 24 * time.Hour

var (
	berlin, _  = time.LoadLocation("Europe/Berlin")
	thresholds = profile.Thresholds{
		Warning:  profile.Duration(30 * day),
		Error:    profile.Duration(7 * day),
		Critical: profile.Duration(day),
	}
	remind, _ = profile.ParseSchedule("0 8 * * *", berlin)
	start     = time.Date(2026, 10, 5, 12, 0, 0, 0, berlin)
)

func expiring(left time.Duration, now time.Time) checks.Result {
	return checks.Result{Expiry: now.Add(left), Summary: "token expires"}
}

func kinds(ns []Notification) []Kind {
	var out []Kind
	for _, n := range ns {
		out = append(out, n.Kind)
	}
	return out
}

func eq(a []Kind, b ...Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBandFor(t *testing.T) {
	tests := []struct {
		left time.Duration
		want status.Band
	}{
		{60 * day, status.OK},
		{30*day + time.Hour, status.OK},
		{30 * day, status.Warning},
		{8 * day, status.Warning},
		{7 * day, status.Error},
		{2 * day, status.Error},
		{day, status.Critical},
		{-day, status.Critical},
	}
	for _, tt := range tests {
		if got := BandFor(start.Add(tt.left), start, thresholds); got != tt.want {
			t.Errorf("BandFor(%v) = %v, want %v", tt.left, got, tt.want)
		}
	}
}

// TestLifecycle walks one token from ok through the bands to rotation.
func TestLifecycle(t *testing.T) {
	expiry := start.Add(40 * day)
	res := checks.Result{Expiry: expiry}
	s := New("pat")

	step := func(now time.Time, r checks.Result, err error, want ...Kind) {
		t.Helper()
		var ns []Notification
		s, ns = Evaluate(s, r, err, now, thresholds, remind)
		if !eq(kinds(ns), want...) {
			t.Fatalf("%s: notifications = %v, want %v", now.Format(time.DateTime), kinds(ns), want)
		}
	}

	step(start, res, nil)                                   // ok, nothing
	step(start.Add(6*time.Hour), res, nil)                  // still ok
	step(expiry.Add(-30*day), res, nil, Firing)             // enters warning
	step(expiry.Add(-30*day+6*time.Hour), res, nil)         // same band, no remind passed (12:00 -> 18:00)
	step(expiry.Add(-29*day-4*time.Hour), res, nil, Firing) // 08:00 next day: remind
	step(expiry.Add(-29*day-2*time.Hour), res, nil)         // 10:00: already reminded today
	if s.Band != status.Warning || s.PitchedBand != status.Warning {
		t.Fatalf("band = %v, pitched = %v", s.Band, s.PitchedBand)
	}
	step(expiry.Add(-7*day), res, nil, Firing) // worse band pitches at once
	if s.PitchedBand != status.Error {
		t.Fatalf("pitched = %v, want error", s.PitchedBand)
	}

	// Rotated: new expiry a year out.
	rotated := checks.Result{Expiry: expiry.Add(365 * day)}
	step(expiry.Add(-6*day), rotated, nil, Resolved)
	step(expiry.Add(-5*day), rotated, nil)
	if s.PitchedBand != status.OK || s.Band != status.OK {
		t.Fatalf("after rotation band = %v, pitched = %v", s.Band, s.PitchedBand)
	}
}

func TestImprovingButStillBadPitches(t *testing.T) {
	s := New("c")
	s, ns := Evaluate(s, expiring(day/2, start), nil, start, thresholds, remind)
	if !eq(kinds(ns), Firing) || s.PitchedBand != status.Critical {
		t.Fatalf("got %v / %v", kinds(ns), s.PitchedBand)
	}
	now := start.Add(time.Hour)
	s, ns = Evaluate(s, expiring(20*day, now), nil, now, thresholds, remind)
	if !eq(kinds(ns), Firing) || ns[0].Band != status.Warning {
		t.Fatalf("got %v", ns)
	}
}

func TestMinBand(t *testing.T) {
	s, ns := Evaluate(New("c"), checks.Result{MinBand: status.Critical, Problem: "401"}, nil, start, thresholds, remind)
	if s.Band != status.Critical || !eq(kinds(ns), Firing) || ns[0].Severity() != "critical" {
		t.Fatalf("band %v, notes %v", s.Band, ns)
	}
}

func TestCouldNotCheck(t *testing.T) {
	s, _ := Evaluate(New("c"), expiring(20*day, start), nil, start, thresholds, remind)
	if s.Band != status.Warning {
		t.Fatalf("band = %v", s.Band)
	}

	boom := errors.New("connection refused")
	now := start.Add(time.Hour)
	s, ns := Evaluate(s, checks.Result{}, boom, now, thresholds, remind)
	if !eq(kinds(ns), CheckFailing) || ns[0].Severity() != "warning" || ns[0].Error != boom.Error() {
		t.Fatalf("first failure: %v", ns)
	}
	if s.Band != status.Warning || !s.Failing || s.PitchedBand != status.Warning {
		t.Fatalf("failure must keep the band: %+v", s)
	}

	// Rate limited until the next remind time.
	now = start.Add(5 * time.Hour)
	s, ns = Evaluate(s, checks.Result{}, boom, now, thresholds, remind)
	if len(ns) != 0 {
		t.Fatalf("second failure pitched: %v", kinds(ns))
	}
	now = time.Date(2026, 10, 6, 8, 0, 0, 0, berlin)
	s, ns = Evaluate(s, checks.Result{}, boom, now, thresholds, remind)
	if !eq(kinds(ns), CheckFailing) {
		t.Fatalf("remind of failure: %v", kinds(ns))
	}

	// Recovers in the same band. The warning was last pitched yesterday, so
	// today's reminder for it is due as well.
	now = now.Add(time.Hour)
	s, ns = Evaluate(s, expiring(19*day, now), nil, now, thresholds, remind)
	if !eq(kinds(ns), CheckRecovered, Firing) || s.Failing || s.LastError != "" {
		t.Fatalf("recovery: %v %+v", kinds(ns), s)
	}
	// Failing and recovering again on the same day only reports the recovery.
	now = now.Add(time.Hour)
	s, _ = Evaluate(s, checks.Result{}, boom, now, thresholds, remind)
	now = now.Add(time.Hour)
	_, ns = Evaluate(s, expiring(19*day, now), nil, now, thresholds, remind)
	if !eq(kinds(ns), CheckRecovered) {
		t.Fatalf("second recovery: %v", kinds(ns))
	}
}

func TestNoExpiryInfoOnce(t *testing.T) {
	r := checks.Result{NoExpiry: true, Summary: "token has no expiration date"}
	s, ns := Evaluate(New("c"), r, nil, start, thresholds, remind)
	if !eq(kinds(ns), Info) || ns[0].Severity() != "info" {
		t.Fatalf("first: %v", kinds(ns))
	}
	s, ns = Evaluate(s, r, nil, start.Add(day), thresholds, remind)
	if len(ns) != 0 {
		t.Fatalf("second: %v", kinds(ns))
	}
	// Replaced by an expiring token, then by a non-expiring one again.
	s, _ = Evaluate(s, expiring(100*day, start), nil, start.Add(2*day), thresholds, remind)
	_, ns = Evaluate(s, r, nil, start.Add(3*day), thresholds, remind)
	if !eq(kinds(ns), Info) {
		t.Fatalf("after change: %v", kinds(ns))
	}
}
