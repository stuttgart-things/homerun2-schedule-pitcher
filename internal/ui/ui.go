// Package ui serves the small web UI (design #1, section 3.3): checks and
// findings, run now and acknowledge. Server-rendered, no JavaScript needed.
package ui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed favicon.png
var favicon []byte

// Checks is what the UI needs from the scheduler.
type Checks interface {
	Statuses(ctx context.Context) ([]scheduler.CheckStatus, error)
	Run(ctx context.Context, id string) (state.State, error)
	History(ctx context.Context, id string, limit int) ([]store.HistoryEntry, error)
}

// Findings is what the UI needs from the findings service. It is nil on an
// agent.
type Findings interface {
	List(ctx context.Context, status, source string) ([]findings.Finding, error)
	Acknowledge(ctx context.Context, source, key, by, note string) (findings.Finding, error)
}

// UI serves the pages under /ui/.
type UI struct {
	Checks   Checks
	Findings Findings
	Sessions *Sessions
	// Title names the instance, e.g. the profile name.
	Title   string
	Version string
	Mode    string // central or agent
	// Loc is the timezone times are shown in (the profile's).
	Loc *time.Location
	// TokenSecret and TokenNamespace name the Secret holding AUTH_TOKEN, for
	// the hint on the login page; empty TokenSecret hides the hint.
	TokenSecret    string
	TokenNamespace string
	now            func() time.Time
	tmpl           *template.Template
}

func New(checks Checks, f Findings, sessions *Sessions, title, version, mode string, loc *time.Location) *UI {
	if loc == nil {
		loc = time.UTC
	}
	u := &UI{Checks: checks, Findings: f, Sessions: sessions, Title: title, Version: version, Mode: mode, Loc: loc, now: time.Now}
	u.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"age":       func(t time.Time) string { return ageOf(u.now(), t) },
		"until":     func(t time.Time) string { return untilOf(u.now(), t) },
		"when":      func(t time.Time) string { return whenOf(t, u.Loc) },
		"span":      func(from, to time.Time) string { return findings.Age(to.Sub(from)) },
		"list":      func(s ...string) []string { return s },
		"date":      func(t time.Time) string { return t.Format("2006-01-02") },
		"isZero":    func(t time.Time) bool { return t.IsZero() },
		"bandClass": func(s string) string { return "severity-" + s },
		"deref": func(v *float64) string {
			if v == nil {
				return ""
			}
			return fmt.Sprintf("%g", *v)
		},
		"pathEscape": url.PathEscape,
	}).ParseFS(templateFS, "templates/*.html"))
	return u
}

// Register adds the UI routes.
func (u *UI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("GET /ui/static/favicon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(favicon)
	})
	mux.HandleFunc("GET /ui/login", u.loginPage)
	mux.HandleFunc("POST /ui/login", u.sameOrigin(u.login))
	mux.HandleFunc("POST /ui/logout", u.sameOrigin(u.logout))
	mux.HandleFunc("GET /ui/{$}", u.auth(u.dashboard))
	mux.HandleFunc("GET /ui/checks/{id}", u.auth(u.checkPage))
	mux.HandleFunc("POST /ui/checks/{id}/run", u.sameOrigin(u.auth(u.runCheck)))
	mux.HandleFunc("GET /ui/resolved", u.auth(u.resolvedPage))
	mux.HandleFunc("POST /ui/findings/ack", u.sameOrigin(u.auth(u.ack)))
}

type ctxKey struct{}

func user(r *http.Request) string {
	name, _ := r.Context().Value(ctxKey{}).(string)
	return name
}

func (u *UI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := u.Sessions.User(r)
		if !ok {
			http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, name)))
	}
}

// sameOrigin rejects cross-site form posts. The SameSite=Strict cookie
// already keeps the session out of them; this also covers the login form.
func (u *UI) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin != "" {
			o, err := url.Parse(origin)
			if err != nil || o.Host != r.Host {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

type page struct {
	Title, Version, Mode, User, Flash, Error string
	HasFindings                              bool
	Data                                     any
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	p := page{
		Title: u.Title, Version: u.Version, Mode: u.Mode, User: user(r),
		Flash: r.URL.Query().Get("flash"), Error: r.URL.Query().Get("error"),
		HasFindings: u.Findings != nil, Data: data,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := u.tmpl.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("rendering page failed", "page", name, "error", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, path, key, msg string) {
	http.Redirect(w, r, path+"?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.Sessions.User(r); ok {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	hint := ""
	if u.TokenSecret != "" {
		ns := u.TokenNamespace
		if ns == "" {
			ns = "<namespace>"
		}
		hint = fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.data.auth-token}' | base64 -d", ns, u.TokenSecret)
	}
	u.render(w, r, "login.html", map[string]any{
		"Enabled": u.Sessions.Enabled(),
		"Next":    safeNext(r.URL.Query().Get("next")),
		"Secret":  u.TokenSecret,
		"Hint":    hint,
	})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	name := strings.TrimSpace(r.FormValue("name"))
	if !u.Sessions.CheckToken(r.FormValue("token")) || name == "" || len(name) > 64 {
		time.Sleep(500 * time.Millisecond)
		redirect(w, r, "/ui/login", "error", "Name and a valid token are required.")
		return
	}
	u.Sessions.Issue(w, r, name)
	slog.Info("ui login", "user", name)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// safeNext only allows local UI paths as redirect targets.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/ui/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return "/ui/"
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	u.Sessions.Clear(w)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

type dashboard struct {
	Open, Acknowledged []findings.Finding
	Counts             map[string]int
	Checks             []scheduler.CheckStatus
	ChecksBad          int
	FindingsErr        string
}

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	d := dashboard{Counts: map[string]int{}}
	sts, err := u.Checks.Statuses(r.Context())
	if err != nil {
		http.Error(w, "reading checks: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Worst first, then by id.
	slices.SortStableFunc(sts, func(a, b scheduler.CheckStatus) int { return int(b.State.Band - a.State.Band) })
	d.Checks = sts
	for _, cs := range sts {
		if cs.State.Band > 0 || cs.State.Failing {
			d.ChecksBad++
		}
	}
	if u.Findings != nil {
		open, err := u.Findings.List(r.Context(), "open", "")
		if err != nil {
			d.FindingsErr = err.Error()
		}
		for _, f := range open {
			d.Counts[f.Severity]++
			if f.Status == findings.StatusAcknowledged {
				d.Acknowledged = append(d.Acknowledged, f)
			} else {
				d.Open = append(d.Open, f)
			}
		}
	}
	u.render(w, r, "dashboard.html", d)
}

type checkDetail struct {
	Status  scheduler.CheckStatus
	History []store.HistoryEntry
}

func (u *UI) checkPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sts, err := u.Checks.Statuses(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, cs := range sts {
		if cs.Check.ID != id {
			continue
		}
		h, err := u.Checks.History(r.Context(), id, 30)
		if err != nil {
			slog.Warn("reading history failed", "check", id, "error", err)
		}
		u.render(w, r, "check.html", checkDetail{Status: cs, History: h})
		return
	}
	http.NotFound(w, r)
}

func (u *UI) runCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := "/ui/checks/" + url.PathEscape(id)
	if r.FormValue("back") == "dashboard" {
		back = "/ui/"
	}
	_, err := u.Checks.Run(r.Context(), id)
	switch {
	case errors.Is(err, scheduler.ErrUnknownCheck):
		http.NotFound(w, r)
	case errors.Is(err, scheduler.ErrBusy):
		redirect(w, r, back, "error", "The check is already running.")
	case err != nil:
		redirect(w, r, back, "error", "Ran, but: "+err.Error())
	default:
		slog.Info("ui run now", "check", id, "user", user(r))
		redirect(w, r, back, "flash", "Check "+id+" ran.")
	}
}

func (u *UI) ack(w http.ResponseWriter, r *http.Request) {
	if u.Findings == nil {
		http.NotFound(w, r)
		return
	}
	source, key := r.FormValue("source"), r.FormValue("key")
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > 500 {
		note = note[:500]
	}
	_, err := u.Findings.Acknowledge(r.Context(), source, key, user(r), note)
	if err != nil {
		redirect(w, r, "/ui/", "error", "Acknowledge failed: "+err.Error())
		return
	}
	slog.Info("ui acknowledge", "source", source, "key", key, "user", user(r))
	redirect(w, r, "/ui/", "flash", "Acknowledged: "+key)
}

func (u *UI) resolvedPage(w http.ResponseWriter, r *http.Request) {
	if u.Findings == nil {
		http.NotFound(w, r)
		return
	}
	list, err := u.Findings.List(r.Context(), findings.StatusResolved, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	slices.SortFunc(list, func(a, b findings.Finding) int { return b.ResolvedAt.Compare(a.ResolvedAt) })
	u.render(w, r, "resolved.html", list)
}

func ageOf(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return findings.Age(now.Sub(t))
}

func untilOf(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Before(now) {
		return "expired " + findings.Age(now.Sub(t)) + " ago"
	}
	return "in " + findings.Age(t.Sub(now))
}

func whenOf(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "–"
	}
	return t.In(loc).Format("2006-01-02 15:04")
}
