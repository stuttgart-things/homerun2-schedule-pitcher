package profile

import (
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestLoadExample(t *testing.T) {
	p, err := Load("testdata/machinery.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Spec.Pitcher.Format != FormatGrafana {
		t.Errorf("format = %q", p.Spec.Pitcher.Format)
	}
	if p.Location().String() != "Europe/Berlin" {
		t.Errorf("location = %s", p.Location())
	}

	pat, ok := p.FindCheck("github-runner-pat")
	if !ok {
		t.Fatal("github-runner-pat not found")
	}
	if got := pat.Thresholds; got.Warning.D() != 30*day || got.Error.D() != 14*day || got.Critical.D() != 3*day {
		t.Errorf("pat thresholds = %+v", got)
	}
	if pat.Schedule != "0 */6 * * *" || pat.Remind != "0 8 * * *" {
		t.Errorf("schedule/remind = %q/%q", pat.Schedule, pat.Remind)
	}
	if strings.Join(pat.Tags, ",") != "expiry,pat,runner" {
		t.Errorf("tags = %v", pat.Tags)
	}
	if pat.APIURL != "https://api.github.com" || pat.Owner == nil || !*pat.Owner {
		t.Errorf("github defaults not applied: %q %v", pat.APIURL, pat.Owner)
	}

	tlsCheck, _ := p.FindCheck("omni-platform-tls")
	if tlsCheck.Target != "omni.platform.sthings-vsphere.labul.sva.de:443" {
		t.Errorf("target = %q, want port 443 added", tlsCheck.Target)
	}
	if tlsCheck.Thresholds.Error.D() != 7*day {
		t.Errorf("tls thresholds not from defaults: %+v", tlsCheck.Thresholds)
	}
}

func TestParseDefaults(t *testing.T) {
	p, err := Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  checks:
    - id: a
      type: tls-endpoint
      target: example.com:8443
      thresholds: { error: 10d }
`))
	if err != nil {
		t.Fatal(err)
	}
	c := p.Spec.Checks[0]
	if c.Schedule != DefaultSchedule || c.Remind != DefaultRemind || p.Spec.Defaults.Timezone != DefaultTimezone {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Thresholds.Warning != DefaultThresholds.Warning || c.Thresholds.Error.D() != 10*day || c.Thresholds.Critical != DefaultThresholds.Critical {
		t.Errorf("thresholds = %+v", c.Thresholds)
	}
	if c.Timeout.D() != DefaultTimeout {
		t.Errorf("timeout = %v", c.Timeout)
	}
	if f := p.Spec.Findings; *f.OfficeHours.Start != 8 || *f.OfficeHours.End != 18 || f.AckExpiry.D() != 3*day || f.Retention.D() != 7*day {
		t.Errorf("findings defaults = %+v", f)
	}
	if hb := p.Spec.Heartbeat; !hb.On() || hb.Schedule != DefaultHeartbeatSchedule || hb.StaleAfter.D() != 13*time.Hour {
		t.Errorf("heartbeat defaults = %+v", hb)
	}
	if p.Spec.Defaults.System != DefaultSystem {
		t.Errorf("system = %q", p.Spec.Defaults.System)
	}
}

func TestTypeThresholds(t *testing.T) {
	p, err := Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  defaults:
    thresholds: { critical: 2d }
    assignee: patrick.hermann
  checks:
    - {id: pat, type: github-token-expiry, tokenFrom: {env: X}}
    - {id: pat-own, type: github-token-expiry, tokenFrom: {env: X}, thresholds: {error: 10d}, assignee: someone}
    - {id: tls, type: tls-endpoint, target: 'a:1'}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]time.Duration{
		"pat":     {30 * day, 14 * day, 2 * day}, // type default, critical from spec.defaults
		"pat-own": {30 * day, 10 * day, 2 * day},
		"tls":     {30 * day, 7 * day, 2 * day},
	}
	for id, w := range want {
		c, _ := p.FindCheck(id)
		got := [3]time.Duration{c.Thresholds.Warning.D(), c.Thresholds.Error.D(), c.Thresholds.Critical.D()}
		if got != w {
			t.Errorf("%s: thresholds = %v, want %v", id, got, w)
		}
	}
	if c, _ := p.FindCheck("pat"); c.Assignee != "patrick.hermann" {
		t.Errorf("default assignee = %q", c.Assignee)
	}
	if c, _ := p.FindCheck("pat-own"); c.Assignee != "someone" {
		t.Errorf("own assignee = %q", c.Assignee)
	}
}

func TestReportDefaults(t *testing.T) {
	p, err := Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
metadata: { name: machinery }
spec:
  report: { addr: "https://central/findings", auth: { tokenFrom: { env: T } } }
`))
	if err != nil || p.Spec.Report.Source != "checks-machinery" {
		t.Fatalf("source = %q, %v", p.Spec.Report.Source, err)
	}
	_, err = Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  report: { addr: "https://central/findings" }
`))
	if err == nil || !strings.Contains(err.Error(), "spec.report.source is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	head := "apiVersion: homerun2.sthings.io/v1alpha1\nkind: SchedulePitcherProfile\n"
	tests := []struct {
		name, yaml, want string
	}{
		{"wrong kind", "apiVersion: homerun2.sthings.io/v1alpha1\nkind: Foo\n", "kind must be"},
		{"unknown field", head + "spec:\n  chekcs: []\n", "chekcs"},
		{"format", head + "spec:\n  pitcher: { format: teams }\n", "spec.pitcher.format"},
		{"missing id", head + "spec:\n  checks:\n    - type: tls-endpoint\n      target: a:1\n", "id is required"},
		{"bad id", head + "spec:\n  checks:\n    - id: 'a b'\n      type: tls-endpoint\n      target: a:1\n", "id must match"},
		{"duplicate id", head + "spec:\n  checks:\n    - {id: a, type: tls-endpoint, target: 'a:1'}\n    - {id: a, type: tls-endpoint, target: 'b:1'}\n", "duplicate id"},
		{"unknown type", head + "spec:\n  checks:\n    - {id: a, type: ping}\n", "unknown type"},
		{"no target", head + "spec:\n  checks:\n    - {id: a, type: tls-endpoint}\n", "target is required"},
		{"no token", head + "spec:\n  checks:\n    - {id: a, type: github-token-expiry}\n", "tokenFrom is required"},
		{"two token sources", head + "spec:\n  checks:\n    - {id: a, type: github-token-expiry, tokenFrom: {env: X, file: /x}}\n", "exactly one"},
		{"bad cron", head + "spec:\n  checks:\n    - {id: a, type: tls-endpoint, target: 'a:1', schedule: 'every day'}\n", "schedule"},
		{"threshold order", head + "spec:\n  checks:\n    - {id: a, type: tls-endpoint, target: 'a:1', thresholds: {warning: 1d, error: 7d}}\n", "warning >= error >= critical"},
		{"bad duration", head + "spec:\n  checks:\n    - {id: a, type: tls-endpoint, target: 'a:1', thresholds: {warning: soon}}\n", "invalid duration"},
		{"vault without addr", head + "spec:\n  checks:\n    - {id: a, type: vault-token-ttl, tokenFrom: {env: X}}\n", "addr is required"},
		{"vault without token", head + "spec:\n  checks:\n    - {id: a, type: vault-token-ttl, addr: 'https://v:8200'}\n", "tokenFrom is required"},
		{"bad timezone", head + "spec:\n  defaults: { timezone: Mars/Base }\n", "timezone"},
		{"heartbeat schedule", head + "spec:\n  heartbeat: { schedule: 'daily' }\n", "spec.heartbeat.schedule"},
		{"reminder without due", head + "spec:\n  reminders:\n    - {id: r, title: t}\n", "exactly one of due or recurrence"},
		{"reminder with both", head + "spec:\n  reminders:\n    - {id: r, title: t, due: '2027-01-01', recurrence: '0 9 * * 1'}\n", "exactly one of due or recurrence"},
		{"reminder bad due", head + "spec:\n  reminders:\n    - {id: r, title: t, due: 'next week'}\n", "neither a date"},
		{"reminder bad cron", head + "spec:\n  reminders:\n    - {id: r, title: t, recurrence: 'weekly'}\n", "recurrence"},
		{"reminder no title", head + "spec:\n  reminders:\n    - {id: r, due: '2027-01-01'}\n", "title is required"},
		{"reminder duplicate", head + "spec:\n  reminders:\n    - {id: r, title: t, due: '2027-01-01'}\n    - {id: r, title: t, due: '2027-01-02'}\n", "duplicate id"},
		{"office hours", head + "spec:\n  findings: { officeHours: { start: 18, end: 8 } }\n", "officeHours"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestReminders(t *testing.T) {
	p, err := Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
spec:
  defaults: { timezone: Europe/Berlin }
  reminders:
    - { id: renew-cert, title: Renew the cert, due: "2027-03-28" }
    - { id: review, title: Weekly review, recurrence: "0 9 * * 1", leadTimes: [1d] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Spec.Reminders[0].LeadTimes; len(got) != 3 || got[0].D() != DefaultReminderLead {
		t.Errorf("default lead times = %v", got)
	}
	due, _ := ParseDue(p.Spec.Reminders[0].Due, p.Location())
	if want := time.Date(2027, 3, 28, 0, 0, 0, 0, p.Location()); !due.Equal(want) {
		t.Errorf("due = %s, want %s", due, want)
	}
	if t2, err := ParseDue("2027-03-28T10:00:00Z", p.Location()); err != nil || t2.Hour() != 10 {
		t.Errorf("RFC3339 due = %s, %v", t2, err)
	}
}

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"":      0,
		"30d":   30 * day,
		"2w":    14 * day,
		"1d12h": 36 * time.Hour,
		"90m":   90 * time.Minute,
		"2w1d":  15 * day,
	}
	for in, want := range tests {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"d", "-1d", "xd", "1x"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", bad)
		}
	}
	if s := Duration(30 * day).String(); s != "30d" {
		t.Errorf("String = %q", s)
	}
}

func TestParseScheduleTimezone(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	s, err := ParseSchedule("0 8 * * *", berlin)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	next := s.Next(from)
	if want := time.Date(2026, 10, 5, 8, 0, 0, 0, berlin); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}
