// Package pitcher renders notifications and delivers them to omni-pitcher.
package pitcher

import (
	"fmt"
	"strings"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
)

// Author is the author of every message this service pitches.
const Author = "homerun2-schedule-pitcher"

// Message is a rendered notification, independent of the wire format.
type Message struct {
	// AlertName is stable for one condition of one check, so a resolution
	// matches the alert it resolves.
	AlertName string
	Title     string
	Text      string
	Severity  string
	// Resolved marks a resolution (grafana status "resolved").
	Resolved bool
	CheckID  string
	Type     string
	System   string
	Tags     []string
	URL      string
	Assignee string
	At       time.Time
}

// Render turns a notification about check c into a message.
func Render(n state.Notification, c profile.Check, system string) Message {
	m := Message{
		Severity: n.Severity(),
		CheckID:  c.ID,
		Type:     c.Type,
		System:   system,
		Tags:     c.Tags,
		URL:      c.URL,
		Assignee: c.Assignee,
		At:       n.At,
	}
	what := subject(c, n)

	switch n.Kind {
	case state.CheckFailing, state.CheckRecovered:
		m.AlertName = c.ID + ": could not check"
	default:
		m.AlertName = c.ID + ": " + noun(c.Type)
	}

	var lines []string
	switch n.Kind {
	case state.Firing:
		switch {
		case n.Problem != "":
			m.Title = fmt.Sprintf("%s: %s", what, n.Problem)
		case n.Expiry.IsZero():
			m.Title = fmt.Sprintf("%s: %s", what, n.Summary)
		default:
			m.Title = fmt.Sprintf("%s %s", what, remaining(n.Expiry, n.At))
		}
		lines = append(lines, n.Summary)
	case state.Resolved:
		m.Resolved = true
		m.Title = fmt.Sprintf("%s is ok again", what)
		lines = append(lines, n.Summary)
	case state.Info:
		m.Title = fmt.Sprintf("%s never expires", what)
		lines = append(lines, n.Summary)
	case state.CheckFailing:
		m.Title = fmt.Sprintf("Could not check %s", what)
		lines = append(lines, "The check could not complete; this says nothing about the "+noun(c.Type)+" itself.", "Error: "+n.Error)
	case state.CheckRecovered:
		m.Resolved = true
		m.Title = fmt.Sprintf("Check of %s works again", what)
		lines = append(lines, n.Summary)
	}
	if c.Description != "" {
		lines = append(lines, c.Description)
	}
	if n.Subject != "" {
		lines = append(lines, "Subject: "+n.Subject)
	}
	m.Text = strings.Join(nonEmpty(lines), "\n")
	return m
}

// HomerunMessage maps a message to the generic omni-pitcher /pitch body.
func (m Message) HomerunMessage() homerun.Message {
	tags := append([]string{"check=" + m.CheckID, "type=" + m.Type}, m.Tags...)
	return homerun.Message{
		Title:        m.Title,
		Message:      m.Text,
		Severity:     m.Severity,
		Author:       Author,
		Timestamp:    m.At.Format(time.RFC3339),
		System:       m.System,
		Tags:         strings.Join(tags, ","),
		AssigneeName: m.Assignee,
		URL:          m.URL,
	}
}

func subject(c profile.Check, n state.Notification) string {
	switch c.Type {
	case profile.TypeGitHubTokenExpiry:
		if n.Subject != "" {
			return fmt.Sprintf("GitHub token %s (%s)", c.ID, n.Subject)
		}
		return "GitHub token " + c.ID
	case profile.TypeTLSEndpoint:
		return fmt.Sprintf("TLS certificate of %s", c.Target)
	case profile.TypeVaultTokenTTL:
		if n.Subject != "" {
			return fmt.Sprintf("Vault token %s (%s)", c.ID, n.Subject)
		}
		return "Vault token " + c.ID
	default:
		return c.ID
	}
}

func noun(checkType string) string {
	switch checkType {
	case profile.TypeGitHubTokenExpiry, profile.TypeVaultTokenTTL:
		return "token"
	case profile.TypeTLSEndpoint:
		return "certificate"
	default:
		return "check"
	}
}

// remaining renders the time left, e.g. "expires in 12 days (2027-03-28)".
func remaining(expiry, now time.Time) string {
	date := expiry.Format("2006-01-02")
	left := expiry.Sub(now)
	switch {
	case left < 0:
		ago := -left
		if ago < 24*time.Hour {
			return fmt.Sprintf("expired %s ago (%s)", hours(ago), date)
		}
		return fmt.Sprintf("expired %s ago (%s)", days(ago), date)
	case left < 24*time.Hour:
		return fmt.Sprintf("expires in %s (%s)", hours(left), date)
	default:
		return fmt.Sprintf("expires in %s (%s)", days(left), date)
	}
}

func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

func hours(d time.Duration) string {
	n := int(d / time.Hour)
	if n == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", n)
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
