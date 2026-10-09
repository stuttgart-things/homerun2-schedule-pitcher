package findings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/robfig/cron/v3"
)

// ReminderSource is the findings source of the reminders.
const ReminderSource = "reminders"

// Reminder is due once (Due) or repeatedly (Schedule).
type Reminder struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Message string    `json:"message,omitempty"`
	URL     string    `json:"url,omitempty"`
	Tags    []string  `json:"tags,omitempty"`
	Due     time.Time `json:"due,omitzero"`
	// Recurrence is the cron expression behind Schedule, for display.
	Recurrence string        `json:"recurrence,omitempty"`
	Schedule   cron.Schedule `json:"-"`
	// Lead is how long before the due date the reminder opens.
	Lead time.Duration `json:"-"`
}

// Recurring reports whether the reminder repeats.
func (r Reminder) Recurring() bool { return r.Schedule != nil }

// ReminderState is what the service remembers per reminder.
type ReminderState struct {
	// FirstSeen anchors a recurring reminder: occurrences before it are
	// not due.
	FirstSeen time.Time `json:"firstSeen,omitzero"`
	// DoneThrough is the last occurrence marked done.
	DoneThrough time.Time `json:"doneThrough,omitzero"`
	DoneAt      time.Time `json:"doneAt,omitzero"`
	DoneBy      string    `json:"doneBy,omitempty"`
}

// Reminder states.
const (
	ReminderUpcoming = "upcoming"
	ReminderOpen     = "open"
	ReminderDueToday = "due today"
	ReminderOverdue  = "overdue"
	ReminderDone     = "done"
)

// ReminderStatus is a reminder with its current occurrence.
type ReminderStatus struct {
	Reminder
	State ReminderState `json:"state"`
	// Next is the occurrence that is open or comes next; zero once a
	// one-off reminder is done.
	Next   time.Time `json:"next,omitzero"`
	Status string    `json:"status"`
	// Days until Next in calendar days (negative when overdue).
	Days int `json:"days"`
}

// ErrUnknownReminder is returned for a reminder that is not in the profile.
var ErrUnknownReminder = errors.New("unknown reminder")

// next returns the occurrence of r that is open or comes next.
func (r Reminder) next(st ReminderState) (time.Time, bool) {
	if !r.Recurring() {
		if !st.DoneThrough.IsZero() {
			return time.Time{}, false
		}
		return r.Due, true
	}
	anchor := st.FirstSeen.Add(-time.Second)
	if !st.DoneThrough.IsZero() {
		anchor = st.DoneThrough
	}
	t := r.Schedule.Next(anchor)
	return t, !t.IsZero()
}

// statusOf evaluates r at now in loc.
func statusOf(r Reminder, st ReminderState, now time.Time, loc *time.Location) ReminderStatus {
	rs := ReminderStatus{Reminder: r, State: st, Status: ReminderDone}
	next, ok := r.next(st)
	if !ok {
		return rs
	}
	rs.Next = next
	rs.Days = calendarDays(now.In(loc), next.In(loc))
	switch {
	case rs.Days < 0:
		rs.Status = ReminderOverdue
	case rs.Days == 0:
		rs.Status = ReminderDueToday
	case !now.Before(next.Add(-r.Lead)):
		rs.Status = ReminderOpen
	default:
		rs.Status = ReminderUpcoming
	}
	return rs
}

// calendarDays counts the days from the date of a to the date of b.
func calendarDays(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}

func days(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// item renders an open reminder as a finding.
func (rs ReminderStatus) item(loc *time.Location) (ReportedItem, bool) {
	due := rs.Next.In(loc).Format("Mon 2006-01-02")
	var sev, msg string
	switch rs.Status {
	case ReminderOpen:
		sev, msg = SeverityWarning, fmt.Sprintf("Due in %s (%s)", days(rs.Days), due)
	case ReminderDueToday:
		sev, msg = SeverityError, fmt.Sprintf("Due today (%s)", due)
	case ReminderOverdue:
		sev, msg = SeverityCritical, fmt.Sprintf("Overdue by %s (was due %s)", days(-rs.Days), due)
	default:
		return ReportedItem{}, false
	}
	if rs.Message != "" {
		msg += "\n" + rs.Message
	}
	return ReportedItem{Key: rs.ID, Title: rs.Title, Severity: sev, Message: msg, URL: rs.URL, Tags: rs.Tags}, true
}

// reminderStates loads the states and records FirstSeen for new reminders.
func (s *Service) reminderStates(ctx context.Context, now time.Time) (map[string]ReminderState, error) {
	states, err := s.store.ReminderStates(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range s.cfg.Reminders {
		st := states[r.ID]
		if st.FirstSeen.IsZero() {
			st.FirstSeen = now
			if err := s.store.PutReminderState(ctx, r.ID, st); err != nil {
				return nil, err
			}
			states[r.ID] = st
		}
	}
	return states, nil
}

// Reminders returns the configured reminders with their current state,
// soonest first.
func (s *Service) Reminders(ctx context.Context) ([]ReminderStatus, error) {
	now := s.now()
	states, err := s.reminderStates(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]ReminderStatus, 0, len(s.cfg.Reminders))
	for _, r := range s.cfg.Reminders {
		out = append(out, statusOf(r, states[r.ID], now, s.cfg.Hours.Loc))
	}
	slices.SortStableFunc(out, func(a, b ReminderStatus) int {
		switch {
		case a.Next.IsZero() != b.Next.IsZero():
			if a.Next.IsZero() {
				return 1
			}
			return -1
		default:
			return a.Next.Compare(b.Next)
		}
	})
	return out, nil
}

// evaluateReminders ingests the open reminders as the complete set of the
// reminders source.
func (s *Service) evaluateReminders(ctx context.Context) error {
	if len(s.cfg.Reminders) == 0 {
		// Resolves what is left from reminders removed from the profile.
		stored, err := s.store.List(ctx, ReminderSource)
		if err != nil || len(stored) == 0 {
			return err
		}
	}
	list, err := s.Reminders(ctx)
	if err != nil {
		return err
	}
	rep := Report{Source: ReminderSource, Findings: []ReportedItem{}}
	for _, rs := range list {
		if it, ok := rs.item(s.cfg.Hours.Loc); ok {
			rep.Findings = append(rep.Findings, it)
		}
	}
	_, err = s.Ingest(ctx, rep)
	return err
}

// ReminderDone marks the current occurrence of a reminder done: a one-off
// reminder is finished, a recurring one moves on to its next occurrence.
func (s *Service) ReminderDone(ctx context.Context, id, by string) (ReminderStatus, error) {
	i := slices.IndexFunc(s.cfg.Reminders, func(r Reminder) bool { return r.ID == id })
	if i < 0 {
		return ReminderStatus{}, ErrUnknownReminder
	}
	r := s.cfg.Reminders[i]
	now := s.now()
	states, err := s.reminderStates(ctx, now)
	if err != nil {
		return ReminderStatus{}, err
	}
	st := states[id]
	next, ok := r.next(st)
	if !ok {
		return statusOf(r, st, now, s.cfg.Hours.Loc), nil
	}
	st.DoneThrough, st.DoneAt, st.DoneBy = next, now, by
	if err := s.store.PutReminderState(ctx, id, st); err != nil {
		return ReminderStatus{}, err
	}
	slog.Info("reminder done", "reminder", id, "occurrence", next, "by", by)
	if err := s.evaluateReminders(ctx); err != nil {
		slog.Warn("re-evaluating reminders failed", "error", err)
	}
	return statusOf(r, st, now, s.cfg.Hours.Loc), nil
}
