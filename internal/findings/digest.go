package findings

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// OfficeHours are the same every day: [Start, End) in Loc.
type OfficeHours struct {
	Start int
	End   int
	Loc   *time.Location
}

// Inside reports whether t is within office hours.
func (o OfficeHours) Inside(t time.Time) bool {
	h := t.In(o.Loc).Hour()
	return h >= o.Start && h < o.End
}

// Immediate reports whether an event is pitched on its own right away:
// critical always, error only inside office hours. Resolutions wait for the
// next update.
func Immediate(e Event, now time.Time, o OfficeHours) bool {
	if e.Change == ChangeResolved {
		return false
	}
	switch e.Finding.Severity {
	case SeverityCritical:
		return true
	case SeverityError:
		return o.Inside(now)
	}
	return false
}

// DigestKind is the kind of an office-hours message.
type DigestKind string

const (
	KindUpdate     DigestKind = "update"
	KindStartOfDay DigestKind = "start-of-day"
	KindEndOfDay   DigestKind = "end-of-day"
)

// Digest is the content of one office-hours message.
type Digest struct {
	Kind  DigestKind
	Since time.Time
	At    time.Time

	New        []Finding
	Reopened   []Finding
	Worse      []Finding
	AckExpired []Finding
	Resolved   []Finding
	// Open is every open finding, oldest first (summaries only).
	Open []Finding
}

// Changes counts what changed since the last message.
func (d Digest) Changes() int {
	return len(d.New) + len(d.Reopened) + len(d.Worse) + len(d.AckExpired) + len(d.Resolved)
}

// Empty reports whether there is nothing worth a message.
func (d Digest) Empty() bool {
	if d.Kind == KindUpdate {
		return d.Changes() == 0
	}
	return d.Changes() == 0 && len(d.Open) == 0
}

// Build collects what changed in (since, now] and, for summaries, what is open.
// Changes that were already pitched on their own are left out of updates.
func Build(kind DigestKind, all []Finding, since, now time.Time) Digest {
	d := Digest{Kind: kind, Since: since, At: now}
	after := func(t time.Time) bool { return t.After(since) && !t.After(now) }
	notified := func(f Finding, at time.Time) bool {
		return kind == KindUpdate && !f.NotifiedAt.Before(at)
	}
	for _, f := range all {
		if !f.IsOpen() {
			// Came and went within the window: nothing to report.
			if after(f.ResolvedAt) && !after(f.OpenSince()) {
				d.Resolved = append(d.Resolved, f)
			}
			continue
		}
		if kind != KindUpdate {
			d.Open = append(d.Open, f)
		}
		switch {
		case after(f.OpenSince()):
			if notified(f, f.OpenSince()) {
				continue
			}
			if after(f.ReopenedAt) {
				d.Reopened = append(d.Reopened, f)
			} else {
				d.New = append(d.New, f)
			}
		case after(f.WorsenedAt):
			if !notified(f, f.WorsenedAt) {
				d.Worse = append(d.Worse, f)
			}
		case after(f.AckExpiredAt):
			d.AckExpired = append(d.AckExpired, f)
		}
	}
	for _, l := range [][]Finding{d.New, d.Reopened, d.Worse, d.AckExpired, d.Resolved} {
		sortBySeverity(l)
	}
	sort.SliceStable(d.Open, func(i, j int) bool { return d.Open[i].OpenSince().Before(d.Open[j].OpenSince()) })
	return d
}

func sortBySeverity(l []Finding) {
	sort.SliceStable(l, func(i, j int) bool {
		if Rank(l[i].Severity) != Rank(l[j].Severity) {
			return Rank(l[i].Severity) > Rank(l[j].Severity)
		}
		return l[i].ID() < l[j].ID()
	})
}

// Severity of the message: the worst finding that needs attention, success
// if only resolutions are reported.
func (d Digest) Severity() string {
	attention := append(append(append(append([]Finding{}, d.New...), d.Reopened...), d.Worse...), d.AckExpired...)
	if d.Kind != KindUpdate {
		attention = d.Open
	}
	worst := ""
	for _, f := range attention {
		if worst == "" || Rank(f.Severity) > Rank(worst) {
			worst = f.Severity
		}
	}
	if worst == "" {
		if len(d.Resolved) > 0 {
			return "success"
		}
		return SeverityInfo
	}
	return worst
}

// maxLines bounds each list in a message.
const maxLines = 25

// Title is a one-line overview.
func (d Digest) Title() string {
	switch d.Kind {
	case KindStartOfDay:
		return fmt.Sprintf("Findings, start of day: %d open%s", len(d.Open), counts(d, " (overnight: ", ")"))
	case KindEndOfDay:
		return fmt.Sprintf("Findings, end of day: %d resolved today, %d still open", len(d.Resolved), len(d.Open))
	default:
		return "Findings: " + counts(d, "", "")
	}
}

func counts(d Digest, pre, post string) string {
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add(len(d.New), "new")
	add(len(d.Reopened), "reopened")
	add(len(d.Worse), "worse")
	add(len(d.AckExpired), "acknowledgement expired")
	if d.Kind != KindEndOfDay {
		add(len(d.Resolved), "resolved")
	}
	if len(parts) == 0 {
		return ""
	}
	return pre + strings.Join(parts, ", ") + post
}

// Text renders the lists.
func (d Digest) Text() string {
	var b strings.Builder
	section := func(title string, l []Finding, withAge bool) {
		if len(l) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", title)
		for i, f := range l {
			if i == maxLines {
				fmt.Fprintf(&b, "- … and %d more\n", len(l)-maxLines)
				break
			}
			b.WriteString("- " + Line(f, d.At, withAge) + "\n")
		}
		b.WriteString("\n")
	}
	switch d.Kind {
	case KindStartOfDay:
		section("New overnight", append(append([]Finding{}, d.New...), d.Reopened...), false)
		section("Worse overnight", d.Worse, false)
		section("Resolved overnight", d.Resolved, false)
		section("Open, oldest first", d.Open, true)
	case KindEndOfDay:
		section("Resolved today", d.Resolved, false)
		section("Still open, oldest first", d.Open, true)
	default:
		section("New", d.New, false)
		section("Reopened", d.Reopened, true)
		section("Worse", d.Worse, true)
		section("Acknowledgement expired, still open", d.AckExpired, true)
		section("Resolved", d.Resolved, false)
	}
	return strings.TrimSpace(b.String())
}

// Line renders one finding, e.g.
// "[warning] /home at 72% (dev4-vm, open for 3 days, acknowledged by patrick: cleanup)".
func Line(f Finding, now time.Time, withAge bool) string {
	var extra []string
	if f.Host != "" && !strings.Contains(f.Title, f.Host) {
		extra = append(extra, f.Host)
	}
	if f.Status == StatusResolved {
		extra = append(extra, "was open for "+Age(f.ResolvedAt.Sub(f.OpenSince())))
	} else if withAge {
		extra = append(extra, "open for "+Age(now.Sub(f.OpenSince())))
	}
	if f.Status == StatusAcknowledged {
		ack := "acknowledged by " + f.AckBy
		if f.AckNote != "" {
			ack += ": " + f.AckNote
		}
		extra = append(extra, ack)
	}
	extra = append(extra, f.Source)
	return fmt.Sprintf("[%s] %s (%s)", f.Severity, f.Title, strings.Join(extra, ", "))
}

// Age renders a duration coarsely: "40 minutes", "5 hours", "3 days".
func Age(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	default:
		return plural(int(d/(24*time.Hour)), "day")
	}
}
