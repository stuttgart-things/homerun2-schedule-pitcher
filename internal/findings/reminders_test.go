package findings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

func weekly(t *testing.T) cron.Schedule {
	t.Helper()
	s, err := cron.ParseStandard("0 9 * * 1")
	if err != nil {
		t.Fatal(err)
	}
	s.(*cron.SpecSchedule).Location = berlin
	return s
}

func newReminderService(t *testing.T, rs ...Reminder) (*Service, *recorder, *time.Time) {
	t.Helper()
	rec := &recorder{}
	now := at(5, 10, 0) // Monday
	s := NewService(NewMemory(), store.NewMemory(), rec, Config{
		Hours: OfficeHours{Start: 8, End: 18, Loc: berlin}, AckExpiry: 3 * 24 * time.Hour, Retention: 7 * 24 * time.Hour,
		System: "sys", Assignee: "patrick.hermann", Reminders: rs,
	})
	s.now = func() time.Time { return now }
	return s, rec, &now
}

func reminderFinding(t *testing.T, s *Service, id string) Finding {
	t.Helper()
	f, ok, err := s.store.Get(context.Background(), ReminderSource, id)
	if err != nil || !ok {
		t.Fatalf("finding %s: %v %v", id, ok, err)
	}
	return f
}

func TestStatusOf(t *testing.T) {
	r := Reminder{ID: "r", Due: at(9, 0, 0), Lead: 3 * 24 * time.Hour}
	for _, tt := range []struct {
		now    time.Time
		status string
		days   int
	}{
		{at(5, 10, 0), ReminderUpcoming, 4},
		{at(6, 0, 0), ReminderOpen, 3},
		{at(8, 23, 59), ReminderOpen, 1},
		{at(9, 17, 0), ReminderDueToday, 0},
		{at(11, 8, 0), ReminderOverdue, -2},
	} {
		got := statusOf(r, ReminderState{}, tt.now, berlin)
		if got.Status != tt.status || got.Days != tt.days {
			t.Errorf("at %s: %s/%d, want %s/%d", tt.now, got.Status, got.Days, tt.status, tt.days)
		}
	}
	if got := statusOf(r, ReminderState{DoneThrough: r.Due}, at(11, 8, 0), berlin); got.Status != ReminderDone || !got.Next.IsZero() {
		t.Errorf("done one-off: %+v", got)
	}
	// Recurring: occurrences before FirstSeen do not count.
	w := Reminder{ID: "w", Schedule: weekly(t), Lead: 24 * time.Hour}
	got := statusOf(w, ReminderState{FirstSeen: at(5, 10, 0)}, at(5, 10, 0), berlin)
	if !got.Next.Equal(at(12, 9, 0)) || got.Status != ReminderUpcoming {
		t.Errorf("recurring: %+v", got)
	}
	got = statusOf(w, ReminderState{FirstSeen: at(5, 9, 0)}, at(5, 10, 0), berlin)
	if !got.Next.Equal(at(5, 9, 0)) || got.Status != ReminderDueToday {
		t.Errorf("recurring due at first sight: %+v", got)
	}
}

func TestRemindersLifecycle(t *testing.T) {
	ctx := context.Background()
	s, rec, now := newReminderService(t,
		Reminder{ID: "renew-cert", Title: "Renew the wildcard cert", Message: "order at the CA", Due: at(9, 0, 0), Lead: 7 * 24 * time.Hour},
		Reminder{ID: "weekly-review", Title: "Weekly review", Schedule: weekly(t), Recurrence: "0 9 * * 1", Lead: 24 * time.Hour},
	)

	// Monday 10:00: the cert is open, the review is next week.
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f := reminderFinding(t, s, "renew-cert")
	if f.Severity != SeverityWarning || !strings.HasPrefix(f.Message, "Due in 4 days (Fri 2026-10-09)\norder at the CA") {
		t.Fatalf("open: %s %q", f.Severity, f.Message)
	}
	if _, ok, _ := s.store.Get(ctx, ReminderSource, "weekly-review"); ok {
		t.Fatal("upcoming reminder must not be a finding")
	}

	// Friday: due today is an error, pitched at once in office hours.
	*now = at(9, 8, 0)
	n := len(rec.msgs)
	_ = s.Tick(ctx)
	if f := reminderFinding(t, s, "renew-cert"); f.Severity != SeverityError || !strings.HasPrefix(f.Message, "Due today") {
		t.Fatalf("due today: %s %q", f.Severity, f.Message)
	}
	if len(rec.msgs) == n || rec.msgs[n].Severity != SeverityError {
		t.Errorf("due today not pitched: %s", rec.titles())
	}

	// Saturday 00:30: outside office hours nothing is evaluated.
	*now = at(10, 0, 30)
	n = len(rec.msgs)
	_ = s.Tick(ctx)
	if f := reminderFinding(t, s, "renew-cert"); f.Severity != SeverityError || len(rec.msgs) != n {
		t.Fatalf("night: %s, %d new messages", f.Severity, len(rec.msgs)-n)
	}
	*now = at(10, 8, 0)
	_ = s.Tick(ctx)
	if f := reminderFinding(t, s, "renew-cert"); f.Severity != SeverityCritical || !strings.HasPrefix(f.Message, "Overdue by 1 day") {
		t.Fatalf("overdue: %s %q", f.Severity, f.Message)
	}

	// Done resolves the finding and finishes the one-off reminder.
	st, err := s.ReminderDone(ctx, "renew-cert", "patrick.hermann")
	if err != nil || st.Status != ReminderDone {
		t.Fatalf("done: %+v %v", st, err)
	}
	if f := reminderFinding(t, s, "renew-cert"); f.Status != StatusResolved {
		t.Fatalf("not resolved: %s", f.Status)
	}
	if last := rec.msgs[len(rec.msgs)-1]; last.Severity != "success" {
		t.Errorf("resolution not pitched: %s", rec.titles())
	}

	// The weekly review: done early moves to the occurrence after the
	// one that was open, not back to the same date.
	*now = at(12, 8, 0)
	_ = s.Tick(ctx)
	if f := reminderFinding(t, s, "weekly-review"); f.Severity != SeverityError {
		t.Fatalf("weekly due: %s", f.Severity)
	}
	st, _ = s.ReminderDone(ctx, "weekly-review", "p")
	if !st.Next.Equal(at(19, 9, 0)) || st.Status != ReminderUpcoming {
		t.Fatalf("after done: %+v", st)
	}
	*now = at(18, 10, 0)
	st, _ = s.ReminderDone(ctx, "weekly-review", "p")
	if !st.Next.Equal(at(26, 9, 0)) {
		t.Fatalf("done early: next %s", st.Next)
	}

	list, _ := s.Reminders(ctx)
	if len(list) != 2 || list[0].ID != "weekly-review" || list[1].Status != ReminderDone || list[0].State.DoneBy != "p" {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.ReminderDone(ctx, "nope", "p"); !errors.Is(err, ErrUnknownReminder) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestRemovedReminderResolves(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newReminderService(t, Reminder{ID: "x", Title: "x", Due: at(6, 0, 0), Lead: 7 * 24 * time.Hour})
	_ = s.Tick(ctx)
	if f := reminderFinding(t, s, "x"); f.Status != StatusOpen {
		t.Fatalf("open: %s", f.Status)
	}
	s.cfg.Reminders = nil
	if err := s.evaluateReminders(ctx); err != nil {
		t.Fatal(err)
	}
	if f := reminderFinding(t, s, "x"); f.Status != StatusResolved {
		t.Fatalf("removed reminder still %s", f.Status)
	}
}
