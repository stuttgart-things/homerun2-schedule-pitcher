// Package findings keeps findings reported by other jobs and delivers them
// in office hours (design #1, section 3.5).
package findings

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Severities, from least to most severe.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityError    = "error"
	SeverityCritical = "critical"
)

// Rank orders severities; unknown severities rank lowest.
func Rank(severity string) int {
	switch severity {
	case SeverityWarning:
		return 1
	case SeverityError:
		return 2
	case SeverityCritical:
		return 3
	default:
		return 0
	}
}

// Statuses of a finding.
const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
)

// HistoryLimit is how many observations a finding keeps.
const HistoryLimit = 10

// Limits for one report.
const (
	MaxFindingsPerReport = 1000
	MaxBodyBytes         = 1 << 20
)

// Report is the body of POST /findings: the complete current set of findings
// of one source.
type Report struct {
	Source   string         `json:"source"`
	Run      string         `json:"run,omitempty"`
	Findings []ReportedItem `json:"findings"`
}

// ReportedItem is one finding as a producer reports it.
type ReportedItem struct {
	// Key is stable across runs, e.g. "<host>/<check>[:<detail>]".
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Severity  string   `json:"severity"`
	Message   string   `json:"message,omitempty"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Host      string   `json:"host,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	URL       string   `json:"url,omitempty"`
}

var sourcePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Validate checks a report and normalises severities to lower case.
func (r *Report) Validate() error {
	var errs []error
	if !sourcePattern.MatchString(r.Source) {
		errs = append(errs, fmt.Errorf("source must match %s", sourcePattern))
	}
	if len(r.Findings) > MaxFindingsPerReport {
		errs = append(errs, fmt.Errorf("at most %d findings per report", MaxFindingsPerReport))
	}
	seen := map[string]bool{}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Severity = strings.ToLower(strings.TrimSpace(f.Severity))
		where := fmt.Sprintf("findings[%d]", i)
		switch {
		case strings.TrimSpace(f.Key) == "":
			errs = append(errs, fmt.Errorf("%s: key is required", where))
		case len(f.Key) > 512:
			errs = append(errs, fmt.Errorf("%s: key is longer than 512 characters", where))
		case seen[f.Key]:
			errs = append(errs, fmt.Errorf("%s: duplicate key %q", where, f.Key))
		}
		seen[f.Key] = true
		if strings.TrimSpace(f.Title) == "" {
			errs = append(errs, fmt.Errorf("%s: title is required", where))
		}
		switch f.Severity {
		case SeverityInfo, SeverityWarning, SeverityError, SeverityCritical:
		default:
			errs = append(errs, fmt.Errorf("%s: severity must be info, warning, error or critical, got %q", where, f.Severity))
		}
	}
	return errors.Join(errs...)
}

// Observation is one reported value of a finding.
type Observation struct {
	At       time.Time `json:"at"`
	Severity string    `json:"severity"`
	Value    *float64  `json:"value,omitempty"`
}

// Finding is the stored state of one source/key.
type Finding struct {
	Source    string   `json:"source"`
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Message   string   `json:"message,omitempty"`
	Severity  string   `json:"severity"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Host      string   `json:"host,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	URL       string   `json:"url,omitempty"`
	Status    string   `json:"status"`
	Run       string   `json:"run,omitempty"`

	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
	ReopenedAt time.Time `json:"reopenedAt,omitzero"`
	WorsenedAt time.Time `json:"worsenedAt,omitzero"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`

	AckBy        string    `json:"ackBy,omitempty"`
	AckAt        time.Time `json:"ackAt,omitzero"`
	AckNote      string    `json:"ackNote,omitempty"`
	AckExpiredAt time.Time `json:"ackExpiredAt,omitzero"`

	// NotifiedAt is when the finding was last pitched on its own (immediate
	// delivery), so the hourly update does not repeat that change.
	NotifiedAt time.Time `json:"notifiedAt,omitzero"`

	History []Observation `json:"history,omitempty"`
}

// ID identifies a finding across sources.
func (f Finding) ID() string { return f.Source + "/" + f.Key }

// IsOpen reports whether the finding is open or acknowledged.
func (f Finding) IsOpen() bool { return f.Status != StatusResolved }

// OpenSince is when the current open period began.
func (f Finding) OpenSince() time.Time {
	if f.ReopenedAt.After(f.FirstSeen) {
		return f.ReopenedAt
	}
	return f.FirstSeen
}

// Change is what one report did to a finding.
type Change string

const (
	ChangeNew      Change = "new"
	ChangeReopened Change = "reopened"
	ChangeWorse    Change = "worse"
	ChangeResolved Change = "resolved"
)

// Event is a change caused by a report.
type Event struct {
	Change  Change
	Finding Finding
}

// Apply merges a validated report into the stored findings of its source
// (prev, keyed by Key) and returns the findings to save and what changed.
// Findings that are open and missing from the report are resolved.
func Apply(prev map[string]Finding, r Report, now time.Time) ([]Finding, []Event) {
	var out []Finding
	var events []Event
	reported := map[string]bool{}

	for _, it := range r.Findings {
		reported[it.Key] = true
		f, exists := prev[it.Key]
		var change Change
		switch {
		case !exists:
			f = Finding{Source: r.Source, Key: it.Key, Status: StatusOpen, FirstSeen: now}
			change = ChangeNew
		case f.Status == StatusResolved:
			f.Status = StatusOpen
			f.ReopenedAt = now
			f.ResolvedAt = time.Time{}
			f.AckBy, f.AckAt, f.AckNote, f.AckExpiredAt = "", time.Time{}, "", time.Time{}
			change = ChangeReopened
		case Rank(it.Severity) > Rank(f.Severity):
			f.WorsenedAt = now
			change = ChangeWorse
		}
		f.Title, f.Message, f.Severity = it.Title, it.Message, it.Severity
		f.Value, f.Threshold, f.Host, f.Tags, f.URL = it.Value, it.Threshold, it.Host, it.Tags, it.URL
		f.Run = r.Run
		f.LastSeen = now
		f.History = append(f.History, Observation{At: now, Severity: it.Severity, Value: it.Value})
		if len(f.History) > HistoryLimit {
			f.History = f.History[len(f.History)-HistoryLimit:]
		}
		out = append(out, f)
		if change != "" {
			events = append(events, Event{Change: change, Finding: f})
		}
	}

	for key, f := range prev {
		if reported[key] || !f.IsOpen() {
			continue
		}
		f.Status = StatusResolved
		f.ResolvedAt = now
		out = append(out, f)
		events = append(events, Event{Change: ChangeResolved, Finding: f})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	sort.Slice(events, func(i, j int) bool { return events[i].Finding.Key < events[j].Finding.Key })
	return out, events
}
