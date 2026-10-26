package ui

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
)

// Micro-frontend fragments for homerun2-portal. The portal reads
// <base>/ui/manifest.json to learn which fragments a service offers and loads
// each <base>/ui/fragment/<id> with htmx into its overview. A fragment is bare
// HTML styled only by the portal's hr-* classes (static/homerun2.css in
// homerun2-portal); every URL in it carries the base path, because it is
// served on the portal's origin.

const (
	findingsFragmentLimit   = 6
	findingsFragmentRefresh = 15
)

// Fragment is one entry of the manifest.
type Fragment struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	RefreshSeconds int    `json:"refreshSeconds"`
}

// Manifest lists the fragments the service offers to the portal.
type Manifest struct {
	Name      string     `json:"name"`
	Title     string     `json:"title"`
	Version   string     `json:"version"`
	BasePath  string     `json:"basePath"`
	Fragments []Fragment `json:"fragments"`
}

// manifest serves GET /ui/manifest.json. It is public: the portal's server
// reads it without a session, and it names no findings. An agent has no
// findings, so it offers no fragment.
func (u *UI) manifest(w http.ResponseWriter, r *http.Request) {
	m := Manifest{
		Name: "homerun2-schedule-pitcher", Title: "Schedule Pitcher",
		Version: u.Version, BasePath: u.basePath, Fragments: []Fragment{},
	}
	if u.Findings != nil {
		m.Fragments = append(m.Fragments, Fragment{
			ID: "findings", Title: "Open findings",
			URL: u.path("/ui/fragment/findings"), RefreshSeconds: findingsFragmentRefresh,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// html/template escapes every field: titles and sources come from findings
// reported by agents and checks.
var findingsFragment = template.Must(template.New("findings").Funcs(template.FuncMap{
	"hrSeverity": hrSeverity,
	"upper":      strings.ToUpper,
}).Parse(`<div class="hr-frag">
{{- if .Denied}}<span class="hr-muted">You are not allowed to see the findings.</span>
{{- else if not .LoggedIn}}<span class="hr-muted">Log in to see open findings.</span><a class="hr-frag-more" href="{{.Base}}/ui/login">Log in →</a>
{{- else if .Err}}<span class="hr-muted">Findings unavailable: {{.Err}}</span><a class="hr-frag-more" href="{{.Base}}/ui/">Open schedule pitcher →</a>
{{- else}}
{{- if .Open}}<span class="hr-muted">{{len .Open}} open{{if .Acknowledged}}, {{.Acknowledged}} acknowledged{{end}}</span>
<ul class="hr-list">
{{- range .Shown}}<li class="hr-item"><span class="hr-time">{{.Age}}</span><span class="hr-sev hr-sev-{{hrSeverity .Severity}}">{{upper .Severity}}</span><span class="hr-system">{{.Source}}</span><span class="hr-title">{{.Title}}</span></li>
{{- end}}
{{- if .More}}<li class="hr-item hr-muted">+{{.More}} more</li>{{end -}}
</ul>
{{- else}}<span class="hr-muted">No open findings{{if .Acknowledged}} ({{.Acknowledged}} acknowledged){{end}}.</span>{{end -}}
<a class="hr-frag-more" href="{{.Base}}/ui/">Open schedule pitcher →</a>
{{- end}}</div>`))

type fragmentItem struct {
	Age, Severity, Source, Title string
}

// findingsFrag serves GET /ui/fragment/findings: the open (not acknowledged)
// findings, newest first. The portal loads it with htmx from the viewer's
// browser, so it is behind the same login as the UI; without a session it
// answers 200 with a login hint instead of a redirect, which htmx would
// swap into the card. With AUTH_TRUST_FORWARDED_USER the portal's
// X-Forwarded-User counts as a session too; a forwarded user outside
// AUTH_FORWARDED_GROUPS gets a "not allowed" line, also with 200.
func (u *UI) findingsFrag(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	if u.Forwarded.Trust {
		w.Header().Add("Vary", HeaderForwardedUser+", "+HeaderForwardedGroups)
	}
	data := struct {
		LoggedIn     bool
		Denied       bool
		Err          string
		Open         []findings.Finding
		Acknowledged int
		Shown        []fragmentItem
		More         int
		Base         string
	}{Base: u.basePath}
	id := u.identify(r)
	data.Denied = id.Denied
	if id.ok() {
		data.LoggedIn = true
		list, err := u.Findings.List(r.Context(), "open", "")
		if err != nil {
			data.Err = err.Error()
		}
		for _, f := range list {
			if f.Status == findings.StatusAcknowledged {
				data.Acknowledged++
			} else {
				data.Open = append(data.Open, f)
			}
		}
		slices.SortStableFunc(data.Open, func(a, b findings.Finding) int { return openedAt(b).Compare(openedAt(a)) })
		for i, f := range data.Open {
			if i == findingsFragmentLimit {
				data.More = len(data.Open) - i
				break
			}
			data.Shown = append(data.Shown, fragmentItem{
				Age: ageOf(u.now(), openedAt(f)), Severity: f.Severity, Source: f.Source, Title: f.Title,
			})
		}
	}
	if err := findingsFragment.Execute(w, data); err != nil {
		slog.Error("rendering fragment failed", "fragment", "findings", "error", err)
	}
}

// openedAt is when a finding last became open.
func openedAt(f findings.Finding) time.Time {
	if f.ReopenedAt.After(f.FirstSeen) {
		return f.ReopenedAt
	}
	return f.FirstSeen
}

// hrSeverity maps a severity to the portal's hr-sev-* classes.
func hrSeverity(severity string) string {
	switch severity {
	case "error", "critical":
		return "error"
	case "warning":
		return "warning"
	default:
		return "info"
	}
}
