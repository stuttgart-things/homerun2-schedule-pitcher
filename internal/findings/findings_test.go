package findings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/metrics"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

func at(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, berlin) }

func ptr(v float64) *float64 { return &v }

func disk(key string, pct float64, sev string) ReportedItem {
	return ReportedItem{Key: "dev4-vm/disk:" + key, Title: key + " at " + strings.TrimSuffix(strings.TrimSuffix(
		time.Duration(pct).String(), "ns"), "s") + "%", Severity: sev, Value: ptr(pct), Threshold: ptr(70), Host: "dev4-vm"}
}

func report(items ...ReportedItem) Report {
	return Report{Source: "dev-maintenance", Run: "r", Findings: items}
}

func TestValidate(t *testing.T) {
	ok := report(disk("/home", 72, "INFO"))
	if err := ok.Validate(); err != nil || ok.Findings[0].Severity != "info" {
		t.Fatalf("valid report: %v %q", err, ok.Findings[0].Severity)
	}
	if err := (&Report{Source: "x"}).Validate(); err != nil {
		t.Fatalf("empty set must be valid (everything resolved): %v", err)
	}
	bad := []struct {
		r    Report
		want string
	}{
		{Report{Source: "a b"}, "source"},
		{report(ReportedItem{Title: "t", Severity: "info"}), "key is required"},
		{report(ReportedItem{Key: "k", Severity: "info"}), "title is required"},
		{report(ReportedItem{Key: "k", Title: "t", Severity: "panic"}), "severity"},
		{report(disk("/a", 1, "info"), disk("/a", 2, "info")), "duplicate key"},
	}
	for _, tt := range bad {
		if err := tt.r.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Validate = %v, want %q", err, tt.want)
		}
	}
}

func changes(es []Event) string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Change)+":"+e.Finding.Key)
	}
	return strings.Join(out, ",")
}

func byKey(fs []Finding) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range fs {
		m[f.Key] = f
	}
	return m
}

func TestApplyLifecycle(t *testing.T) {
	t0 := at(5, 4, 0)
	fs, ev := Apply(nil, report(disk("/home", 72, "info"), disk("/var", 74, "info")), t0)
	if changes(ev) != "new:dev4-vm/disk:/home,new:dev4-vm/disk:/var" {
		t.Fatalf("first run: %s", changes(ev))
	}

	t1 := at(6, 4, 0)
	fs, ev = Apply(byKey(fs), report(disk("/home", 88, "warning")), t1)
	m := byKey(fs)
	if changes(ev) != "worse:dev4-vm/disk:/home,resolved:dev4-vm/disk:/var" {
		t.Fatalf("second run: %s", changes(ev))
	}
	home := m["dev4-vm/disk:/home"]
	if !home.FirstSeen.Equal(t0) || !home.WorsenedAt.Equal(t1) || len(home.History) != 2 || *home.Value != 88 {
		t.Fatalf("home = %+v", home)
	}
	if v := m["dev4-vm/disk:/var"]; v.Status != StatusResolved || !v.ResolvedAt.Equal(t1) {
		t.Fatalf("var = %+v", v)
	}

	// Improving is no event; /var comes back and reopens with its old first_seen.
	t2 := at(7, 4, 0)
	fs, ev = Apply(m, report(disk("/home", 75, "info"), disk("/var", 80, "info")), t2)
	if changes(ev) != "reopened:dev4-vm/disk:/var" {
		t.Fatalf("third run: %s", changes(ev))
	}
	v := byKey(fs)["dev4-vm/disk:/var"]
	if v.Status != StatusOpen || !v.FirstSeen.Equal(t0) || !v.ReopenedAt.Equal(t2) || !v.OpenSince().Equal(t2) || !v.ResolvedAt.IsZero() {
		t.Fatalf("reopened = %+v", v)
	}

	// An acknowledgement survives runs, a resolved finding is not resolved twice.
	m = byKey(fs)
	h := m["dev4-vm/disk:/home"]
	h.Status, h.AckBy = StatusAcknowledged, "patrick"
	m[h.Key] = h
	fs, _ = Apply(m, report(disk("/home", 76, "info")), at(8, 4, 0))
	m = byKey(fs)
	if m["dev4-vm/disk:/home"].Status != StatusAcknowledged {
		t.Fatalf("ack lost: %+v", m["dev4-vm/disk:/home"])
	}
	_, ev = Apply(m, report(disk("/home", 76, "info")), at(9, 4, 0))
	if len(ev) != 0 {
		t.Fatalf("resolved again: %s", changes(ev))
	}
}

func TestApplyHistoryCapped(t *testing.T) {
	var m map[string]Finding
	for i := range HistoryLimit + 5 {
		fs, _ := Apply(m, report(disk("/home", float64(70+i), "info")), at(5, 0, i))
		m = byKey(fs)
	}
	if h := m["dev4-vm/disk:/home"].History; len(h) != HistoryLimit || *h[len(h)-1].Value != float64(70+HistoryLimit+4) {
		t.Fatalf("history = %d entries", len(h))
	}
}

func TestImmediate(t *testing.T) {
	hours := OfficeHours{Start: 8, End: 18, Loc: berlin}
	f := func(sev string) Event { return Event{Change: ChangeNew, Finding: Finding{Severity: sev}} }
	tests := []struct {
		e    Event
		now  time.Time
		want bool
	}{
		{f(SeverityCritical), at(5, 3, 0), true},
		{f(SeverityError), at(5, 10, 0), true},
		{f(SeverityError), at(5, 3, 0), false},
		{f(SeverityWarning), at(5, 10, 0), false},
		{Event{Change: ChangeResolved, Finding: Finding{Severity: SeverityCritical}}, at(5, 10, 0), false},
		// Pitched on its own in this open period: its resolution goes out at once, at night too.
		{Event{Change: ChangeResolved, Finding: Finding{Severity: SeverityCritical, FirstSeen: at(5, 2, 0), NotifiedAt: at(5, 2, 0)}}, at(5, 3, 0), true},
		// Pitched in an earlier open period only (reopened since): not immediate.
		{Event{Change: ChangeResolved, Finding: Finding{Severity: SeverityCritical, FirstSeen: at(1, 2, 0), NotifiedAt: at(1, 2, 0), ReopenedAt: at(5, 2, 0)}}, at(5, 3, 0), false},
	}
	for _, tt := range tests {
		if got := Immediate(tt.e, tt.now, hours); got != tt.want {
			t.Errorf("Immediate(%s %s at %s) = %v", tt.e.Change, tt.e.Finding.Severity, tt.now.Format("15:04"), got)
		}
	}
}

func TestBuildUpdate(t *testing.T) {
	since, now := at(5, 10, 0), at(5, 11, 0)
	all := []Finding{
		{Source: "s", Key: "new", Severity: "warning", Status: StatusOpen, FirstSeen: at(5, 10, 30)},
		{Source: "s", Key: "new-notified", Severity: "critical", Status: StatusOpen, FirstSeen: at(5, 10, 30), NotifiedAt: at(5, 10, 30)},
		{Source: "s", Key: "worse", Title: "worse", Severity: "error", Status: StatusOpen, FirstSeen: at(1, 0, 0), WorsenedAt: at(5, 10, 15)},
		{Source: "s", Key: "old", Severity: "warning", Status: StatusOpen, FirstSeen: at(1, 0, 0)},
		{Source: "s", Key: "acked", Severity: "warning", Status: StatusAcknowledged, FirstSeen: at(1, 0, 0)},
		{Source: "s", Key: "ack-expired", Severity: "info", Status: StatusOpen, FirstSeen: at(1, 0, 0), AckExpiredAt: at(5, 11, 0)},
		{Source: "s", Key: "resolved", Severity: "info", Status: StatusResolved, FirstSeen: at(1, 0, 0), ResolvedAt: at(5, 10, 45)},
		{Source: "s", Key: "came-and-went", Severity: "info", Status: StatusResolved, FirstSeen: at(5, 10, 10), ResolvedAt: at(5, 10, 50)},
		{Source: "s", Key: "resolved-earlier", Severity: "info", Status: StatusResolved, FirstSeen: at(1, 0, 0), ResolvedAt: at(5, 9, 0)},
	}
	d := Build(KindUpdate, all, since, now)
	keys := func(l []Finding) string {
		var k []string
		for _, f := range l {
			k = append(k, f.Key)
		}
		return strings.Join(k, ",")
	}
	if keys(d.New) != "new" || keys(d.Worse) != "worse" || keys(d.AckExpired) != "ack-expired" || keys(d.Resolved) != "resolved" || len(d.Open) != 0 {
		t.Fatalf("new=%s worse=%s ackexp=%s resolved=%s open=%d", keys(d.New), keys(d.Worse), keys(d.AckExpired), keys(d.Resolved), len(d.Open))
	}
	if d.Severity() != "error" || d.Title() != "Findings: 1 new, 1 worse, 1 acknowledgement expired, 1 resolved" {
		t.Fatalf("severity %q title %q", d.Severity(), d.Title())
	}
	if !strings.Contains(d.Text(), "Worse:\n- [error] worse (open for 4 days, s)") {
		t.Fatalf("text:\n%s", d.Text())
	}

	if d := Build(KindUpdate, all, now, now.Add(time.Hour)); !d.Empty() {
		t.Fatalf("nothing changed but not empty: %+v", d)
	}
	onlyResolved := Build(KindUpdate, all[6:7], since, now)
	if onlyResolved.Severity() != "success" {
		t.Fatalf("severity = %q", onlyResolved.Severity())
	}
}

func TestBuildSummaries(t *testing.T) {
	all := []Finding{
		{Source: "s", Key: "b", Title: "/var at 74%", Host: "vm-b", Severity: "warning", Status: StatusOpen, FirstSeen: at(3, 4, 0)},
		{Source: "s", Key: "a", Title: "/home at 72%", Host: "vm-a", Severity: "info", Status: StatusAcknowledged, AckBy: "patrick", AckNote: "cleanup", FirstSeen: at(1, 4, 0)},
		{Source: "s", Key: "c", Title: "/opt at 71%", Severity: "info", Status: StatusOpen, FirstSeen: at(5, 4, 0), NotifiedAt: at(5, 4, 0)},
		{Source: "s", Key: "r", Title: "/tmp at 90%", Severity: "warning", Status: StatusResolved, FirstSeen: at(2, 4, 0), ResolvedAt: at(5, 4, 0)},
	}
	start := Build(KindStartOfDay, all, at(4, 18, 0), at(5, 8, 0))
	if len(start.Open) != 3 || start.Open[0].Key != "a" || len(start.New) != 1 || len(start.Resolved) != 1 {
		t.Fatalf("start = %+v", start)
	}
	if start.Title() != "Findings, start of day: 3 open (overnight: 1 new, 1 resolved)" || start.Severity() != "warning" {
		t.Fatalf("title %q severity %q", start.Title(), start.Severity())
	}
	text := start.Text()
	for _, want := range []string{
		"New overnight:\n- [info] /opt at 71% (s)",
		"Resolved overnight:\n- [warning] /tmp at 90% (was open for 3 days, s)",
		"- [info] /home at 72% (vm-a, open for 4 days, acknowledged by patrick: cleanup, s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("start text misses %q:\n%s", want, text)
		}
	}

	end := Build(KindEndOfDay, all, at(5, 8, 0), at(5, 18, 0))
	if end.Title() != "Findings, end of day: 0 resolved today, 3 still open" || end.Empty() {
		t.Fatalf("end title %q", end.Title())
	}
	if Build(KindEndOfDay, nil, at(5, 8, 0), at(5, 18, 0)).Empty() != true {
		t.Fatal("empty end of day must be empty")
	}
}

// --- service

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

func (r *recorder) titles() string {
	var t []string
	for _, m := range r.msgs {
		t = append(t, m.Severity+"|"+m.Title)
	}
	return strings.Join(t, "\n")
}

func newService(t *testing.T, st Store) (*Service, *recorder, *time.Time) {
	t.Helper()
	rec := &recorder{}
	now := at(5, 4, 0)
	s := NewService(st, store.NewMemory(), rec, Config{
		Hours: OfficeHours{Start: 8, End: 18, Loc: berlin}, AckExpiry: 3 * 24 * time.Hour, Retention: 7 * 24 * time.Hour,
		System: "sys", Assignee: "patrick.hermann",
	})
	s.now = func() time.Time { return now }
	return s, rec, &now
}

func TestServiceDay(t *testing.T) {
	ctx := context.Background()
	s, rec, now := newService(t, NewMemory())

	// 04:00 nightly run: info and error wait, critical goes out at once.
	res, err := s.Ingest(ctx, report(disk("/home", 72, "info"), disk("/var", 91, "error"), disk("/", 97, "critical")))
	if err != nil || res.New != 3 || res.Pitched != 1 || res.Open != 3 {
		t.Fatalf("ingest = %+v, %v", res, err)
	}
	if rec.titles() != "critical|dev4-vm: / at 97%" || rec.msgs[0].Assignee != "patrick.hermann" {
		t.Fatalf("immediate = %q", rec.titles())
	}

	// Ticks outside office hours send nothing.
	*now = at(5, 5, 0)
	_ = s.Tick(ctx)
	if len(rec.msgs) != 1 {
		t.Fatalf("pitched outside office hours: %s", rec.titles())
	}

	// 08:00 start of day lists everything, the critical one included.
	*now = at(5, 8, 0)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 2 || !strings.HasPrefix(rec.msgs[1].Title, "Findings, start of day: 3 open") || rec.msgs[1].Severity != "critical" {
		t.Fatalf("start of day: %s", rec.titles())
	}
	// A second tick in the same hour (restart, other replica) sends nothing.
	_ = s.Tick(ctx)
	if len(rec.msgs) != 2 {
		t.Fatal("start of day sent twice")
	}

	// 09:00 nothing changed: no update.
	*now = at(5, 9, 0)
	_ = s.Tick(ctx)
	if len(rec.msgs) != 2 {
		t.Fatalf("empty update sent: %s", rec.titles())
	}

	// 09:30 a fix resolves /var and / ; the new error is immediate in office
	// hours, and so is the resolution of /, which was pitched at 04:00.
	*now = at(5, 9, 30)
	n := len(rec.msgs)
	res, _ = s.Ingest(ctx, report(disk("/home", 72, "info"), disk("/opt", 92, "error")))
	if res.Resolved != 2 || res.Pitched != 2 {
		t.Fatalf("ingest = %+v", res)
	}
	var resolvedMsg, firingMsg pitcher.Message
	for _, m := range rec.msgs[n:] {
		if m.Resolved {
			resolvedMsg = m
		}
	}
	firingMsg = rec.msgs[0]
	if resolvedMsg.Severity != "success" || resolvedMsg.AlertName != firingMsg.AlertName ||
		!strings.Contains(resolvedMsg.Text, "Resolved after 5 hours (was critical)") || resolvedMsg.Source != "dev-maintenance" || resolvedMsg.Key != "dev4-vm/disk:/" {
		t.Fatalf("resolution = %+v (firing %+v)", resolvedMsg, firingMsg)
	}
	// The update reports only /var; the resolution of / went out already.
	*now = at(5, 10, 0)
	_ = s.Tick(ctx)
	if got := rec.msgs[len(rec.msgs)-1].Title; got != "Findings: 1 resolved" {
		t.Fatalf("update: %s", rec.titles())
	}

	// Acknowledge, expire after 3 days, re-surface in the update.
	f, err := s.Acknowledge(ctx, "dev-maintenance", "dev4-vm/disk:/home", "patrick", "cleanup")
	if err != nil || f.Status != StatusAcknowledged {
		t.Fatalf("ack = %+v, %v", f, err)
	}
	if _, err := s.Acknowledge(ctx, "dev-maintenance", "nope", "p", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack unknown = %v", err)
	}
	*now = at(8, 8, 0)
	_ = s.Tick(ctx) // start of day of day 8
	*now = at(8, 9, 0)
	_ = s.Tick(ctx)
	*now = at(8, 10, 0)
	_ = s.Tick(ctx)
	if got := rec.msgs[len(rec.msgs)-1].Title; got != "Findings: 1 acknowledgement expired" {
		t.Fatalf("ack expiry: %s", rec.titles())
	}

	// 18:00 end of day.
	*now = at(8, 18, 0)
	_ = s.Tick(ctx)
	if got := rec.msgs[len(rec.msgs)-1].Title; got != "Findings, end of day: 0 resolved today, 2 still open" {
		t.Fatalf("end of day: %s", rec.titles())
	}

	// Resolved findings are deleted after the retention.
	*now = at(12, 10, 0)
	_ = s.Tick(ctx)
	all, _ := s.List(ctx, "", "")
	if len(all) != 2 {
		t.Fatalf("after retention: %d findings", len(all))
	}
	open, _ := s.List(ctx, "open", "")
	if len(open) != 2 || open[0].Severity != "error" {
		t.Fatalf("open = %+v", open)
	}
}

func TestServicePitchFailureRetries(t *testing.T) {
	ctx := context.Background()
	s, rec, now := newService(t, NewMemory())
	_, _ = s.Ingest(ctx, report(disk("/home", 72, "info")))

	// A failed start-of-day summary stays due and goes out at the next tick.
	rec.err = errors.New("omni down")
	*now = at(5, 8, 0)
	if err := s.Tick(ctx); err == nil {
		t.Fatal("expected error")
	}
	rec.err = nil
	*now = at(5, 9, 0)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.msgs[len(rec.msgs)-1].Title; got != "Findings, start of day: 1 open (overnight: 1 new)" {
		t.Fatalf("start of day retry: %s", rec.titles())
	}

	// A failed update is covered by the next one.
	*now = at(5, 9, 5)
	_, _ = s.Ingest(ctx, report(disk("/home", 72, "info"), disk("/var", 80, "warning")))
	rec.err = errors.New("omni down")
	*now = at(5, 10, 0)
	if err := s.Tick(ctx); err == nil {
		t.Fatal("expected error")
	}
	rec.err = nil
	*now = at(5, 11, 0)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.msgs[len(rec.msgs)-1].Title; got != "Findings: 1 new" {
		t.Fatalf("update retry: %s", rec.titles())
	}

	// A missed end-of-day summary is sent until midnight, not the next day.
	rec.err = errors.New("omni down")
	*now = at(5, 18, 0)
	_ = s.Tick(ctx)
	rec.err = nil
	*now = at(5, 19, 0)
	_ = s.Tick(ctx)
	if got := rec.msgs[len(rec.msgs)-1].Title; !strings.HasPrefix(got, "Findings, end of day") {
		t.Fatalf("end of day retry: %s", rec.titles())
	}
	n := len(rec.msgs)
	*now = at(5, 20, 0)
	_ = s.Tick(ctx)
	if len(rec.msgs) != n {
		t.Fatalf("end of day sent twice: %s", rec.titles())
	}
}

func TestStores(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	for name, st := range map[string]Store{"memory": NewMemory(), "redis": NewRedis(client, "p")} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := Finding{Source: "dev", Key: "vm/disk:/home", Title: "t", Severity: "info", Status: StatusOpen, FirstSeen: at(5, 4, 0), Value: ptr(72)}
			b := Finding{Source: "dev-x", Key: "k", Status: StatusResolved, ResolvedAt: at(5, 5, 0)}
			if err := st.Save(ctx, a, b); err != nil {
				t.Fatal(err)
			}
			if l, _ := st.List(ctx, "dev"); len(l) != 1 || l[0].Key != a.Key || *l[0].Value != 72 || !l[0].FirstSeen.Equal(a.FirstSeen) {
				t.Fatalf("List(dev) = %+v", l)
			}
			if l, _ := st.List(ctx, ""); len(l) != 2 {
				t.Fatalf("List() = %d", len(l))
			}
			if f, ok, _ := st.Get(ctx, "dev", a.Key); !ok || f.Title != "t" {
				t.Fatalf("Get = %+v %v", f, ok)
			}
			_ = st.Delete(ctx, a)
			if _, ok, _ := st.Get(ctx, "dev", a.Key); ok {
				t.Fatal("not deleted")
			}
			_ = st.TouchSource(ctx, "checks-x", at(5, 9, 0))
			if src, err := st.Sources(ctx); err != nil || !src["checks-x"].Equal(at(5, 9, 0)) {
				t.Fatalf("Sources = %v, %v", src, err)
			}
			_ = st.PutReminderState(ctx, "r", ReminderState{FirstSeen: at(5, 9, 0), DoneBy: "p"})
			if rs, err := st.ReminderStates(ctx); err != nil || !rs["r"].FirstSeen.Equal(at(5, 9, 0)) || rs["r"].DoneBy != "p" {
				t.Fatalf("ReminderStates = %v, %v", rs, err)
			}
			d := Delivery{LastUpdate: at(5, 10, 0)}
			_ = st.PutDelivery(ctx, d)
			if got, _ := st.GetDelivery(ctx); !got.LastUpdate.Equal(d.LastUpdate) {
				t.Fatalf("delivery = %+v", got)
			}
		})
	}
	if got := mr.HGet("p:finding:dev-x:k", "status"); got != "resolved" {
		t.Errorf("flat status field = %q", got)
	}
	if !mr.Exists("p:findings") {
		t.Error("index set missing")
	}
}

func TestResolvedAfter(t *testing.T) {
	if got := resolvedAfter(10 * time.Second); got != "Resolved within a minute" {
		t.Errorf("got %q", got)
	}
	if got := resolvedAfter(5 * time.Hour); got != "Resolved after 5 hours" {
		t.Errorf("got %q", got)
	}
}

func TestResolvedNotifiedOnlySkippedInUpdates(t *testing.T) {
	f := Finding{Source: "s", Key: "k", Severity: "critical", Status: StatusResolved,
		FirstSeen: at(5, 2, 0), NotifiedAt: at(5, 2, 0), ResolvedAt: at(5, 10, 30), ResolvedNotifiedAt: at(5, 10, 30)}
	if d := Build(KindUpdate, []Finding{f}, at(5, 10, 0), at(5, 11, 0)); len(d.Resolved) != 0 {
		t.Fatalf("update repeats a resolution already pitched: %+v", d.Resolved)
	}
	if d := Build(KindEndOfDay, []Finding{f}, at(5, 8, 0), at(5, 18, 0)); len(d.Resolved) != 1 {
		t.Fatalf("end of day misses the resolution: %+v", d.Resolved)
	}
}

func newHeartbeatService(t *testing.T) (*Service, *recorder, *time.Time) {
	t.Helper()
	s, rec, now := newService(t, NewMemory())
	s.cfg.Heartbeat = HeartbeatConfig{
		Enabled: true, StaleAfter: 13 * time.Hour, Sources: map[string]time.Duration{"disk-dev4": 26 * time.Hour},
		Name: "platform-sthings", Status: func(context.Context) []string { return []string{"Checks: 3, not ok: 0"} },
	}
	return s, rec, now
}

func TestWatchdog(t *testing.T) {
	ctx := context.Background()
	s, _, now := newHeartbeatService(t)

	// An agent and a cron job report at 04:00; an unwatched source too.
	_, _ = s.Ingest(ctx, Report{Source: "checks-sthings-infra"})
	_, _ = s.Ingest(ctx, Report{Source: "disk-dev4"})
	_, _ = s.Ingest(ctx, Report{Source: "teams-test"})

	// 16:00: the agent is silent for 12h, under its 13h limit.
	*now = at(5, 16, 0)
	_ = s.Tick(ctx)
	if open, _ := s.List(ctx, "open", WatchdogSource); len(open) != 0 {
		t.Fatalf("watchdog too early: %+v", open)
	}

	// 18:00: 14h silent -> warning finding; the 26h cron job and the
	// unwatched source are not reported.
	*now = at(5, 18, 0)
	_ = s.Tick(ctx)
	open, _ := s.List(ctx, "open", WatchdogSource)
	if len(open) != 1 || open[0].Key != "checks-sthings-infra" || open[0].Severity != SeverityWarning ||
		open[0].Title != "No report from checks-sthings-infra for 14 hours" {
		t.Fatalf("watchdog findings = %+v", open)
	}

	// The agent reports again: the watchdog finding resolves on the next tick.
	*now = at(5, 19, 0)
	_, _ = s.Ingest(ctx, Report{Source: "checks-sthings-infra"})
	*now = at(5, 20, 0)
	_ = s.Tick(ctx)
	if open, _ := s.List(ctx, "open", WatchdogSource); len(open) != 0 {
		t.Fatalf("watchdog did not resolve: %+v", open)
	}
}

func TestHeartbeat(t *testing.T) {
	ctx := context.Background()
	s, rec, now := newHeartbeatService(t)
	_, _ = s.Ingest(ctx, report(disk("/var", 74, "info"), disk("/home", 88, "warning")))
	_, _ = s.Ingest(ctx, Report{Source: "checks-sthings-infra"})

	*now = at(6, 8, 0) // the agent is 28h silent by now
	if err := s.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	m := rec.msgs[len(rec.msgs)-1]
	if m.Title != "schedule-pitcher alive · platform-sthings" || m.Severity != SeverityInfo || m.Type != "heartbeat" {
		t.Fatalf("heartbeat = %+v", m)
	}
	for _, want := range []string{"Checks: 3, not ok: 0", "Open findings: 2 (warning 1, info 1)",
		"- checks-sthings-infra: 1 day ago (SILENT)", "- dev-maintenance: 1 day ago"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("heartbeat text misses %q:\n%s", want, m.Text)
		}
	}
	// Once per hour, also after a restart or with a second replica.
	n := len(rec.msgs)
	_ = s.Heartbeat(ctx)
	if len(rec.msgs) != n {
		t.Fatal("heartbeat sent twice in the same hour")
	}
	// A failed delivery is retried by the next schedule.
	rec.err = errors.New("omni down")
	*now = at(7, 8, 0)
	if err := s.Heartbeat(ctx); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHeartbeatBaseline(t *testing.T) {
	ctx := context.Background()
	s, _, now := newHeartbeatService(t)
	read := func() float64 {
		mfs, _ := metrics.Registry.Gather()
		for _, mf := range mfs {
			if mf.GetName() == "schedule_pitcher_heartbeat_timestamp_seconds" {
				return mf.GetMetric()[0].GetGauge().GetValue()
			}
		}
		return -1
	}
	// No heartbeat yet: the start time.
	s.heartbeatBaseline(ctx)
	if got := read(); got != float64(now.Unix()) {
		t.Fatalf("baseline without heartbeat = %v, want %v", got, now.Unix())
	}
	// After a heartbeat and a restart: the stored heartbeat, not the restart.
	_ = s.store.PutDelivery(ctx, Delivery{LastHeartbeat: at(4, 8, 0)})
	*now = at(5, 12, 0)
	s.heartbeatBaseline(ctx)
	if got := read(); got != float64(at(4, 8, 0).Unix()) {
		t.Fatalf("baseline after restart = %v, want the stored heartbeat", got)
	}
}
