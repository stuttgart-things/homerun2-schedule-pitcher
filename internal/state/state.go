// Package state turns check results into bands and decides what to pitch
// (design #1, section 3.4).
package state

import (
	"time"

	"github.com/robfig/cron/v3"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/checks"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// State is what is remembered about a check between runs.
type State struct {
	CheckID string `json:"checkId"`
	// Band is the last known band. A failing run keeps the previous band.
	Band     status.Band `json:"band"`
	LastRun  time.Time   `json:"lastRun"`
	LastOK   time.Time   `json:"lastOk"`
	Expiry   time.Time   `json:"expiry"`
	NoExpiry bool        `json:"noExpiry"`
	Summary  string      `json:"summary"`
	Subject  string      `json:"subject"`
	Problem  string      `json:"problem"`

	// Failing is the separate "could not check" condition.
	Failing      bool      `json:"failing"`
	FailingSince time.Time `json:"failingSince"`
	LastError    string    `json:"lastError"`

	// PitchedBand is the band of the open alert; OK means none is open.
	PitchedBand   status.Band `json:"pitchedBand"`
	PitchedAt     time.Time   `json:"pitchedAt"`
	FailPitchedAt time.Time   `json:"failPitchedAt"`
	// NoExpiryNotified is set once the "never expires" info was pitched.
	NoExpiryNotified bool `json:"noExpiryNotified"`
}

// New returns the state of a check that never ran.
func New(id string) State {
	return State{CheckID: id, Band: status.Unknown, PitchedBand: status.OK}
}

// Kind says what a notification is about.
type Kind string

const (
	// Firing reports a check in a band worse than ok.
	Firing Kind = "firing"
	// Resolved reports that a firing check is ok again.
	Resolved Kind = "resolved"
	// Info is a one-off notice, e.g. a token without expiry.
	Info Kind = "info"
	// CheckFailing reports that the check itself could not complete.
	CheckFailing Kind = "check-failing"
	// CheckRecovered reports that a failing check completes again.
	CheckRecovered Kind = "check-recovered"
)

// Notification is one message to pitch.
type Notification struct {
	Kind    Kind
	CheckID string
	Band    status.Band
	Summary string
	Subject string
	Problem string
	Expiry  time.Time
	Error   string
	At      time.Time
}

// Severity maps a notification to a homerun severity.
func (n Notification) Severity() string {
	switch n.Kind {
	case Firing:
		return n.Band.String()
	case Resolved, CheckRecovered:
		return "success"
	case CheckFailing:
		return "warning"
	default:
		return "info"
	}
}

// BandFor derives the band from the time left until expiry.
func BandFor(expiry, now time.Time, t profile.Thresholds) status.Band {
	left := expiry.Sub(now)
	switch {
	case left <= t.Critical.D():
		return status.Critical
	case left <= t.Error.D():
		return status.Error
	case left <= t.Warning.D():
		return status.Warning
	default:
		return status.OK
	}
}

// Evaluate applies one run to the previous state. runErr means the check could
// not complete. remind is the re-pitch cadence inside an unchanged band.
func Evaluate(prev State, res checks.Result, runErr error, now time.Time, thresholds profile.Thresholds, remind cron.Schedule) (State, []Notification) {
	next := prev
	next.LastRun = now
	var out []Notification
	note := func(k Kind, b status.Band) {
		out = append(out, Notification{
			Kind: k, CheckID: next.CheckID, Band: b, Summary: next.Summary,
			Subject: next.Subject, Problem: next.Problem, Expiry: next.Expiry, Error: next.LastError, At: now,
		})
	}

	if runErr != nil {
		next.LastError = runErr.Error()
		if !prev.Failing {
			next.Failing = true
			next.FailingSince = now
		}
		if !prev.Failing || due(remind, prev.FailPitchedAt, now) {
			next.FailPitchedAt = now
			note(CheckFailing, next.Band)
		}
		return next, out
	}

	next.LastOK = now
	next.Expiry = res.Expiry
	next.NoExpiry = res.NoExpiry
	next.Summary = res.Summary
	next.Subject = res.Subject
	next.Problem = res.Problem

	if prev.Failing {
		next.Failing = false
		next.FailingSince = time.Time{}
		if !prev.FailPitchedAt.IsZero() {
			note(CheckRecovered, next.Band)
		}
		next.FailPitchedAt = time.Time{}
	}
	next.LastError = ""

	band := status.OK
	if !res.Expiry.IsZero() {
		band = BandFor(res.Expiry, now, thresholds)
	}
	band = status.Max(band, res.MinBand)
	next.Band = band

	switch {
	case band > status.OK:
		if band != prev.PitchedBand || due(remind, prev.PitchedAt, now) {
			next.PitchedBand = band
			next.PitchedAt = now
			note(Firing, band)
		}
	case prev.PitchedBand > status.OK:
		next.PitchedBand = status.OK
		next.PitchedAt = now
		note(Resolved, status.OK)
	}

	if !res.NoExpiry {
		next.NoExpiryNotified = false
	} else if band == status.OK && !prev.NoExpiryNotified {
		next.NoExpiryNotified = true
		note(Info, band)
	}
	return next, out
}

// due reports whether the remind schedule fired since last.
func due(remind cron.Schedule, last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	if remind == nil {
		return false
	}
	return !remind.Next(last).After(now)
}
