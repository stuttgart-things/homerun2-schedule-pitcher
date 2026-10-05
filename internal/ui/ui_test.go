package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

var now = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

type fakeChecks struct{ ran string }

func (f *fakeChecks) Statuses(context.Context) ([]scheduler.CheckStatus, error) {
	st := state.New("pat")
	st.Band, st.LastRun, st.Expiry, st.Summary = status.Warning, now, now.Add(20*24*time.Hour), "token expires"
	return []scheduler.CheckStatus{
		{Check: profile.Check{ID: "pat", Type: profile.TypeGitHubTokenExpiry, Origin: profile.OriginDiscovered,
			TokenFrom: &profile.ValueFrom{SecretKeyRef: &profile.SecretKeyRef{Namespace: "flux-system", Name: "git", Key: "password"}}},
			State: st, Band: "warning"},
	}, nil
}

func (f *fakeChecks) Run(_ context.Context, id string) (state.State, error) {
	f.ran = id
	if id == "busy" {
		return state.State{}, scheduler.ErrBusy
	}
	return state.State{}, nil
}

func (f *fakeChecks) History(context.Context, string, int) ([]store.HistoryEntry, error) {
	return []store.HistoryEntry{{At: now, Band: status.Warning, Summary: "token expires", Pitched: []string{"firing"}}}, nil
}

type fakeFindings struct{ acked string }

func (f *fakeFindings) List(_ context.Context, st, _ string) ([]findings.Finding, error) {
	if st == findings.StatusResolved {
		return []findings.Finding{{Source: "s", Key: "r", Title: "gone <b>", Severity: "info", Status: findings.StatusResolved,
			FirstSeen: now.Add(-3 * 24 * time.Hour), ResolvedAt: now}}, nil
	}
	return []findings.Finding{
		{Source: "disk", Key: "vm/disk:/var", Title: "/var at 74% <script>", Severity: "warning", Status: findings.StatusOpen, Host: "vm", FirstSeen: now.Add(-2 * time.Hour)},
		{Source: "disk", Key: "vm/disk:/", Title: "/ at 91%", Severity: "error", Status: findings.StatusAcknowledged, AckBy: "patrick", AckNote: "on it", AckAt: now, FirstSeen: now},
	}, nil
}

func (f *fakeFindings) Acknowledge(_ context.Context, source, key, by, note string) (findings.Finding, error) {
	f.acked = source + "|" + key + "|" + by + "|" + note
	if key == "nope" {
		return findings.Finding{}, findings.ErrNotFound
	}
	return findings.Finding{}, nil
}

func setup(t *testing.T, withFindings bool) (*http.ServeMux, *fakeChecks, *fakeFindings) {
	t.Helper()
	fc, ff := &fakeChecks{}, &fakeFindings{}
	var f Findings
	if withFindings {
		f = ff
	}
	u := New(fc, f, NewSessions("secret"), "platform", "v1", "central", time.UTC)
	u.now = func() time.Time { return now }
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, fc, ff
}

func do(mux *http.ServeMux, method, path string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

func login(t *testing.T, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"patrick.hermann"}, "token": {"secret"}, "next": {"/ui/checks/pat"}}, nil, "")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/ui/checks/pat" {
		t.Fatalf("login: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == CookieName {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie flags: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestLoginFlow(t *testing.T) {
	mux, _, _ := setup(t, true)
	if rr := do(mux, http.MethodGet, "/ui/", nil, nil, ""); rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/ui/login") {
		t.Fatalf("unauthenticated: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"x"}, "token": {"wrong"}}, nil, ""); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("wrong token: %s", rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"x"}, "token": {"secret"}}, nil, "https://evil.example"); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", rr.Code)
	}
	if rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"x"}, "token": {"secret"}, "next": {"//evil.example/"}}, nil, ""); rr.Header().Get("Location") != "/ui/" {
		t.Fatalf("open redirect: %s", rr.Header().Get("Location"))
	}
	c := login(t, mux)
	if rr := do(mux, http.MethodGet, "/ui/", nil, c, ""); rr.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rr.Code)
	}
	tampered := *c
	tampered.Value = strings.Replace(c.Value, c.Value[:4], "eHh4", 1)
	if rr := do(mux, http.MethodGet, "/ui/", nil, &tampered, ""); rr.Code != http.StatusSeeOther {
		t.Fatalf("tampered cookie accepted: %d", rr.Code)
	}
	if rr := do(mux, http.MethodGet, "/", nil, nil, ""); rr.Header().Get("Location") != "/ui/" {
		t.Fatalf("root: %s", rr.Header().Get("Location"))
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessions("secret")
	s.now = func() time.Time { return now }
	rr := httptest.NewRecorder()
	s.Issue(rr, httptest.NewRequest(http.MethodGet, "/", nil), "p")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(rr.Result().Cookies()[0])
	if name, ok := s.User(r); !ok || name != "p" {
		t.Fatalf("user = %q %v", name, ok)
	}
	s.now = func() time.Time { return now.Add(13 * time.Hour) }
	if _, ok := s.User(r); ok {
		t.Fatal("expired session accepted")
	}
	if _, ok := NewSessions("rotated").User(r); ok {
		t.Fatal("session survived token rotation")
	}
	if NewSessions("").Enabled() || NewSessions("").CheckToken("") {
		t.Fatal("empty token must disable the UI")
	}
}

func TestPages(t *testing.T) {
	mux, fc, ff := setup(t, true)
	c := login(t, mux)

	body := do(mux, http.MethodGet, "/ui/", nil, c, "").Body.String()
	for _, want := range []string{"/var at 74% &lt;script&gt;", "on it", "patrick.hermann", "Run now", "discovered", "in 20 days", ">Resolved<"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard misses %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("title not escaped")
	}

	detail := do(mux, http.MethodGet, "/ui/checks/pat", nil, c, "").Body.String()
	for _, want := range []string{"Secret flux-system/git, key password", "discovered Secret", "firing"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail misses %q", want)
		}
	}
	if rr := do(mux, http.MethodGet, "/ui/checks/nope", nil, c, ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown check: %d", rr.Code)
	}
	if body := do(mux, http.MethodGet, "/ui/resolved", nil, c, "").Body.String(); !strings.Contains(body, "gone &lt;b&gt;") || !strings.Contains(body, "3 days") {
		t.Errorf("resolved page: %s", body)
	}

	rr := do(mux, http.MethodPost, "/ui/checks/pat/run", url.Values{"back": {"dashboard"}}, c, "http://example.com")
	if fc.ran != "pat" || !strings.HasPrefix(rr.Header().Get("Location"), "/ui/?flash=") {
		t.Errorf("run now: ran %q, %s", fc.ran, rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/checks/busy/run", url.Values{}, c, ""); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Errorf("busy: %s", rr.Header().Get("Location"))
	}
	rr = do(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"vm/disk:/var"}, "note": {"cleanup"}}, c, "")
	if ff.acked != "disk|vm/disk:/var|patrick.hermann|cleanup" || !strings.Contains(rr.Header().Get("Location"), "flash=") {
		t.Errorf("ack: %q %s", ff.acked, rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"nope"}}, c, ""); !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Errorf("ack unknown: %s", rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"k"}}, c, "https://evil.example"); rr.Code != http.StatusForbidden {
		t.Errorf("cross-site ack: %d", rr.Code)
	}
}

func TestAgentHasNoFindings(t *testing.T) {
	mux, _, _ := setup(t, false)
	c := login(t, mux)
	body := do(mux, http.MethodGet, "/ui/", nil, c, "").Body.String()
	if strings.Contains(body, "Open findings") || !strings.Contains(body, "Checks") {
		t.Error("agent dashboard shows findings")
	}
	if rr := do(mux, http.MethodGet, "/ui/resolved", nil, c, ""); rr.Code != http.StatusNotFound {
		t.Errorf("resolved on agent: %d", rr.Code)
	}
}
