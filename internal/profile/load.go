package profile

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	APIVersion = "homerun2.sthings.io/v1alpha1"
	Kind       = "SchedulePitcherProfile"
)

// Defaults used when spec.defaults leaves a field empty.
const (
	DefaultSchedule = "0 */6 * * *"
	DefaultTimezone = "Europe/Berlin"
	DefaultRemind   = "0 8 * * *"
	DefaultSystem   = "homerun2-schedule-pitcher"
	DefaultTimeout  = 10 * time.Second

	DefaultOfficeStart = 8
	DefaultOfficeEnd   = 18
	DefaultAckExpiry   = 3 * 24 * time.Hour
	DefaultRetention   = 7 * 24 * time.Hour

	DefaultDiscoverySelector = "homerun2.sthings.io/watch-expiry=true"
	DefaultDiscoveryInterval = time.Hour
)

var DefaultThresholds = Thresholds{
	Warning:  Duration(30 * 24 * time.Hour),
	Error:    Duration(7 * 24 * time.Hour),
	Critical: Duration(24 * time.Hour),
}

// TypeThresholds are per-type defaults between spec.defaults and
// DefaultThresholds. Tokens need more lead time to rotate than certificates
// that are usually renewed automatically.
var TypeThresholds = map[string]Thresholds{
	TypeGitHubTokenExpiry: {
		Warning:  Duration(30 * 24 * time.Hour),
		Error:    Duration(14 * 24 * time.Hour),
		Critical: Duration(3 * 24 * time.Hour),
	},
}

// sourcePattern matches findings sources (see internal/findings).
var sourcePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

var checkIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// CronParser accepts standard five-field cron expressions and descriptors
// such as @daily or @every 6h.
var CronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Load reads, defaults and validates a profile from a YAML file.
func Load(path string) (*SchedulePitcherProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile %s: %w", path, err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return p, nil
}

// Parse decodes a profile, rejecting unknown fields, then applies defaults
// and validates it. After Parse every check carries its effective settings.
func Parse(data []byte) (*SchedulePitcherProfile, error) {
	var p SchedulePitcherProfile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	applyDefaults(&p)
	if err := validate(&p); err != nil {
		return nil, fmt.Errorf("validating: %w", err)
	}
	return &p, nil
}

// Location returns the profile's timezone. It is valid after Parse.
func (p *SchedulePitcherProfile) Location() *time.Location {
	loc, err := time.LoadLocation(p.Spec.Defaults.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// FindCheck returns the check with the given id.
func (p *SchedulePitcherProfile) FindCheck(id string) (Check, bool) {
	for _, c := range p.Spec.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

func applyDefaults(p *SchedulePitcherProfile) {
	s := &p.Spec
	if s.Pitcher.Format == "" {
		s.Pitcher.Format = FormatGrafana
	}
	if s.Redis.Addr != "" && s.Redis.Port == "" {
		s.Redis.Port = "6379"
	}
	if s.Report.Source == "" && p.Metadata.Name != "" {
		s.Report.Source = "checks-" + p.Metadata.Name
	}

	fc := &s.Findings
	if fc.OfficeHours.Start == nil {
		v := DefaultOfficeStart
		fc.OfficeHours.Start = &v
	}
	if fc.OfficeHours.End == nil {
		v := DefaultOfficeEnd
		fc.OfficeHours.End = &v
	}
	if fc.AckExpiry == 0 {
		fc.AckExpiry = Duration(DefaultAckExpiry)
	}
	if fc.Retention == 0 {
		fc.Retention = Duration(DefaultRetention)
	}

	d := &s.Defaults
	if d.Schedule == "" {
		d.Schedule = DefaultSchedule
	}
	if d.Timezone == "" {
		d.Timezone = DefaultTimezone
	}
	if d.Remind == "" {
		d.Remind = DefaultRemind
	}
	if d.System == "" {
		d.System = DefaultSystem
	}
	// Precedence per field: check, spec.defaults, type default, DefaultThresholds.
	p.userThresholds = d.Thresholds
	d.Thresholds = mergeThresholds(d.Thresholds, DefaultThresholds)

	dc := &s.Discovery
	if dc.LabelSelector == "" {
		dc.LabelSelector = DefaultDiscoverySelector
	}
	if dc.Interval == 0 {
		dc.Interval = Duration(DefaultDiscoveryInterval)
	}

	for i := range s.Checks {
		s.Checks[i].Origin = OriginProfile
		p.applyCheckDefaults(&s.Checks[i])
	}
}

// applyCheckDefaults fills a check from spec.defaults and the type defaults.
func (p *SchedulePitcherProfile) applyCheckDefaults(c *Check) {
	d := p.Spec.Defaults
	if c.Schedule == "" {
		c.Schedule = d.Schedule
	}
	if c.Remind == "" {
		c.Remind = d.Remind
	}
	c.Thresholds = mergeThresholds(mergeThresholds(c.Thresholds, p.userThresholds), TypeThresholds[c.Type])
	c.Thresholds = mergeThresholds(c.Thresholds, DefaultThresholds)
	c.Tags = mergeTags(d.Tags, c.Tags)
	if c.Assignee == "" {
		c.Assignee = d.Assignee
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(DefaultTimeout)
	}
	switch c.Type {
	case TypeGitHubTokenExpiry:
		if c.APIURL == "" {
			c.APIURL = "https://api.github.com"
		}
		if c.Owner == nil {
			t := true
			c.Owner = &t
		}
	case TypeTLSEndpoint:
		if c.Target != "" {
			if _, _, err := net.SplitHostPort(c.Target); err != nil {
				c.Target = net.JoinHostPort(c.Target, "443")
			}
		}
	}
}

func mergeThresholds(t, fallback Thresholds) Thresholds {
	if t.Warning == 0 {
		t.Warning = fallback.Warning
	}
	if t.Error == 0 {
		t.Error = fallback.Error
	}
	if t.Critical == 0 {
		t.Critical = fallback.Critical
	}
	return t
}

func mergeTags(defaults, own []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append(append([]string{}, defaults...), own...) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func validate(p *SchedulePitcherProfile) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if p.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != Kind {
		add("kind must be %q, got %q", Kind, p.Kind)
	}
	s := p.Spec
	if f := s.Pitcher.Format; f != FormatGrafana && f != FormatGeneric {
		add("spec.pitcher.format must be %q or %q, got %q", FormatGrafana, FormatGeneric, f)
	}
	if err := validateValueFrom(s.Pitcher.Auth.TokenFrom); err != nil {
		add("spec.pitcher.auth.tokenFrom: %v", err)
	}
	if err := validateValueFrom(s.Report.Auth.TokenFrom); err != nil {
		add("spec.report.auth.tokenFrom: %v", err)
	}
	if s.Report.Source != "" && !sourcePattern.MatchString(s.Report.Source) {
		add("spec.report.source must match %s", sourcePattern)
	}
	if s.Report.Addr != "" && s.Report.Source == "" {
		add("spec.report.source is required when metadata.name is empty")
	}
	if err := validateValueFrom(s.Redis.PasswordFrom); err != nil {
		add("spec.redis.passwordFrom: %v", err)
	}
	if _, err := time.LoadLocation(s.Defaults.Timezone); err != nil {
		add("spec.defaults.timezone: %v", err)
	}
	if oh := s.Findings.OfficeHours; *oh.Start < 0 || *oh.End > 23 || *oh.Start+1 >= *oh.End {
		add("spec.findings.officeHours: need 0 <= start < end <= 23 with at least one hour between (got %d-%d)", *oh.Start, *oh.End)
	}

	if s.Discovery.Enabled {
		if _, err := labels.Parse(s.Discovery.LabelSelector); err != nil {
			add("spec.discovery.labelSelector: %v", err)
		}
	}

	seen := map[string]bool{}
	for i, c := range s.Checks {
		where := fmt.Sprintf("spec.checks[%d]", i)
		if c.ID != "" {
			where = fmt.Sprintf("spec.checks[%d] (%s)", i, c.ID)
		}
		if seen[c.ID] && c.ID != "" {
			add("%s: duplicate id", where)
		}
		seen[c.ID] = true
		for _, err := range validateCheck(c) {
			add("%s: %v", where, err)
		}
	}
	return errors.Join(errs...)
}

// CompleteCheck applies the defaults to a check created at runtime (for
// example by discovery) and validates it like a profile check.
func (p *SchedulePitcherProfile) CompleteCheck(c Check) (Check, error) {
	if c.Origin == "" {
		c.Origin = OriginDiscovered
	}
	p.applyCheckDefaults(&c)
	if errs := validateCheck(c); len(errs) > 0 {
		return c, fmt.Errorf("check %s: %w", c.ID, errors.Join(errs...))
	}
	return c, nil
}

func validateCheck(c Check) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case c.ID == "":
		add("id is required")
	case !checkIDPattern.MatchString(c.ID):
		add("id must match %s", checkIDPattern)
	}

	if _, err := CronParser.Parse(c.Schedule); err != nil {
		add("schedule %q: %v", c.Schedule, err)
	}
	if _, err := CronParser.Parse(c.Remind); err != nil {
		add("remind %q: %v", c.Remind, err)
	}
	t := c.Thresholds
	if t.Warning < t.Error || t.Error < t.Critical {
		add("thresholds must satisfy warning >= error >= critical (got %s/%s/%s)", t.Warning, t.Error, t.Critical)
	}

	switch c.Type {
	case TypeGitHubTokenExpiry:
		if c.TokenFrom == nil {
			add("tokenFrom is required for %s", c.Type)
		} else if err := validateValueFrom(c.TokenFrom); err != nil {
			add("tokenFrom: %v", err)
		}
	case TypeTLSEndpoint:
		if c.Target == "" {
			add("target is required for %s", c.Type)
		}
	case "":
		add("type is required")
	default:
		add("unknown type %q", c.Type)
	}
	return errs
}

func validateValueFrom(v *ValueFrom) error {
	if v == nil {
		return nil
	}
	n := 0
	if v.SecretKeyRef != nil {
		n++
		if v.SecretKeyRef.Name == "" || v.SecretKeyRef.Key == "" {
			return errors.New("secretKeyRef needs name and key")
		}
	}
	if v.Env != "" {
		n++
	}
	if v.File != "" {
		n++
	}
	if n != 1 {
		return errors.New("exactly one of secretKeyRef, env or file must be set")
	}
	return nil
}

// ParseSchedule parses a cron expression in the given timezone. A CRON_TZ=
// prefix in the expression wins over loc.
func ParseSchedule(spec string, loc *time.Location) (cron.Schedule, error) {
	s, err := CronParser.Parse(spec)
	if err != nil {
		return nil, err
	}
	if ss, ok := s.(*cron.SpecSchedule); ok && ss.Location == time.Local {
		ss.Location = loc
	}
	return s, nil
}
