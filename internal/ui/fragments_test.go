package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
)

// manyFindings lists n open findings, the i-th opened i hours ago, plus one
// acknowledged.
type manyFindings struct {
	fakeFindings
	n int
}

func (m *manyFindings) List(_ context.Context, st, _ string) ([]findings.Finding, error) {
	if st != "open" {
		return nil, nil
	}
	list := []findings.Finding{{Source: "disk", Key: "ack", Title: "acked", Severity: "error", Status: findings.StatusAcknowledged, FirstSeen: now}}
	for i := range m.n {
		list = append(list, findings.Finding{Source: "disk", Key: fmt.Sprint(i), Title: fmt.Sprintf("finding %d", i),
			Severity: "info", Status: findings.StatusOpen, FirstSeen: now.Add(-time.Duration(i+1) * time.Hour)})
	}
	return list, nil
}

func setupFragments(t *testing.T, f Findings) *http.ServeMux {
	t.Helper()
	u := New(&fakeChecks{}, f, NewSessions("secret"), "platform", "v1", "central", time.UTC)
	u.now = func() time.Time { return now }
	u.SetBasePath(prefix)
	mux := http.NewServeMux()
	u.Register(mux)
	return mux
}

func loginBase(t *testing.T, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"p"}, "token": {"secret"}}, nil, "")
	for _, c := range rr.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestManifestIsPublicAndListsTheFindingsFragment(t *testing.T) {
	rr := do(setupFragments(t, &fakeFindings{}), http.MethodGet, "/ui/manifest.json", nil, nil, "")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("manifest: %d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
	var m Manifest
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Title != "Schedule Pitcher" || m.Version != "v1" || m.BasePath != prefix || len(m.Fragments) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	if f := m.Fragments[0]; f.ID != "findings" || f.Title != "Open findings" || f.URL != prefix+"/ui/fragment/findings" || f.RefreshSeconds < 2 {
		t.Errorf("fragment = %+v", f)
	}
}

func TestManifestOfAnAgentOffersNoFragment(t *testing.T) {
	mux := setupFragments(t, nil)
	var m Manifest
	if err := json.NewDecoder(do(mux, http.MethodGet, "/ui/manifest.json", nil, nil, "").Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Fragments == nil || len(m.Fragments) != 0 {
		t.Errorf("agent fragments = %#v", m.Fragments)
	}
	if rr := do(mux, http.MethodGet, "/ui/fragment/findings", nil, loginBase(t, mux), ""); rr.Code != http.StatusNotFound {
		t.Errorf("agent fragment: %d", rr.Code)
	}
}

func checkFragment(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "<html") || strings.Contains(body, "<head") || strings.Contains(body, "<script") {
		t.Fatalf("a fragment is bare HTML:\n%s", body)
	}
	for _, m := range regexp.MustCompile(`(?:href|src|action|hx-get)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], prefix+"/") {
			t.Errorf("URL without the prefix: %s", m[1])
		}
	}
}

func TestFindingsFragmentWithoutSessionShowsNoFindings(t *testing.T) {
	mux := setupFragments(t, &fakeFindings{})
	for name, c := range map[string]*http.Cookie{
		"none":   nil,
		"forged": {Name: CookieName, Value: "cA.9999999999.forged"},
	} {
		rr := do(mux, http.MethodGet, "/ui/fragment/findings", nil, c, "")
		body := rr.Body.String()
		if rr.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200 (a redirect would swap the login page into the card)", name, rr.Code)
		}
		checkFragment(t, body)
		if !strings.Contains(body, "Log in to see open findings") || !strings.Contains(body, `href="/schedule/ui/login"`) {
			t.Errorf("%s: no login hint:\n%s", name, body)
		}
		if strings.Contains(body, "hr-item") || strings.Contains(body, "/var") || strings.Contains(body, "disk") || strings.Contains(body, "acknowledged") {
			t.Errorf("%s: findings leak without a session:\n%s", name, body)
		}
	}
}

func TestFindingsFragment(t *testing.T) {
	mux := setupFragments(t, &fakeFindings{})
	rr := do(mux, http.MethodGet, "/ui/fragment/findings", nil, loginBase(t, mux), "")
	body := rr.Body.String()
	checkFragment(t, body)
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("a per-user fragment must not be cached")
	}
	for _, want := range []string{
		`<span class="hr-muted">1 open, 1 acknowledged</span>`,
		`<span class="hr-time">2 hours</span><span class="hr-sev hr-sev-warning">WARNING</span><span class="hr-system">disk</span>`,
		`/var at 74% &lt;script&gt;`, // titles are escaped
		`<a class="hr-frag-more" href="/schedule/ui/">Open schedule pitcher →</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/ at 91%") {
		t.Error("acknowledged findings are listed as open")
	}
}

func TestFindingsFragmentCapsNewestFirst(t *testing.T) {
	mux := setupFragments(t, &manyFindings{n: findingsFragmentLimit + 3})
	body := do(mux, http.MethodGet, "/ui/fragment/findings", nil, loginBase(t, mux), "").Body.String()
	if n := strings.Count(body, `<li class="hr-item">`); n != findingsFragmentLimit {
		t.Errorf("%d items, want %d", n, findingsFragmentLimit)
	}
	if !strings.Contains(body, "+3 more") || !strings.Contains(body, fmt.Sprintf("%d open, 1 acknowledged", findingsFragmentLimit+3)) {
		t.Errorf("no count or +N more:\n%s", body)
	}
	if i := strings.Index(body, "finding 0"); i < 0 || i > strings.Index(body, "finding 1") {
		t.Error("the newest finding is not first")
	}
}

func TestFindingsFragmentEmpty(t *testing.T) {
	mux := setupFragments(t, &manyFindings{})
	body := do(mux, http.MethodGet, "/ui/fragment/findings", nil, loginBase(t, mux), "").Body.String()
	checkFragment(t, body)
	if !strings.Contains(body, `<span class="hr-muted">No open findings (1 acknowledged).</span>`) || strings.Contains(body, "hr-list") {
		t.Errorf("empty fragment:\n%s", body)
	}
}
