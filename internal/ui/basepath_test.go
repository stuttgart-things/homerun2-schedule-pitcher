package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const prefix = "/schedule"

// setupBase is setup with the UI under BASE_PATH=/schedule. The mux still
// routes at the root, as behind handlers.Mount, which strips the prefix.
func setupBase(t *testing.T) *http.ServeMux {
	t.Helper()
	u := New(&fakeChecks{}, &fakeFindings{}, NewSessions("secret"), "platform", "v1", "central", time.UTC)
	u.now = func() time.Time { return now }
	u.SetBasePath(prefix)
	mux := http.NewServeMux()
	u.Register(mux)
	return mux
}

func TestBasePathLoginAndRedirects(t *testing.T) {
	mux := setupBase(t)
	if rr := do(mux, http.MethodGet, "/", nil, nil, ""); rr.Header().Get("Location") != prefix+"/ui/" {
		t.Errorf("root: %s", rr.Header().Get("Location"))
	}
	want := prefix + "/ui/login?next=" + url.QueryEscape(prefix+"/ui/checks/pat")
	if rr := do(mux, http.MethodGet, "/ui/checks/pat", nil, nil, ""); rr.Header().Get("Location") != want {
		t.Errorf("unauthenticated: %s, want %s", rr.Header().Get("Location"), want)
	}
	// A next without the prefix is not a page of this UI any more.
	if rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"x"}, "token": {"secret"}, "next": {"/ui/checks/pat"}}, nil, ""); rr.Header().Get("Location") != prefix+"/ui/" {
		t.Errorf("unprefixed next: %s", rr.Header().Get("Location"))
	}
	rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"p"}, "token": {"secret"}, "next": {prefix + "/ui/checks/pat"}}, nil, "")
	if rr.Header().Get("Location") != prefix+"/ui/checks/pat" {
		t.Fatalf("login: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	var c *http.Cookie
	for _, k := range rr.Result().Cookies() {
		if k.Name == CookieName {
			c = k
		}
	}
	if c == nil || c.Path != prefix+"/" {
		t.Fatalf("session cookie must be scoped to the prefix: %+v", c)
	}
	if rr := do(mux, http.MethodPost, "/ui/checks/pat/run", url.Values{"back": {"dashboard"}}, c, ""); !strings.HasPrefix(rr.Header().Get("Location"), prefix+"/ui/?flash=") {
		t.Errorf("run now: %s", rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"k"}}, c, ""); !strings.HasPrefix(rr.Header().Get("Location"), prefix+"/ui/?") {
		t.Errorf("ack: %s", rr.Header().Get("Location"))
	}
	if rr := do(mux, http.MethodPost, "/ui/logout", nil, c, ""); rr.Header().Get("Location") != prefix+"/ui/login" {
		t.Errorf("logout: %s", rr.Header().Get("Location"))
	}
}

// Behind homerun2-portal every URL a page hands out must carry the prefix:
// the portal forwards only /schedule/... to this service.
func TestBasePathPagesCarryPrefix(t *testing.T) {
	mux := setupBase(t)
	pages := map[string]string{"/ui/login": do(mux, http.MethodGet, "/ui/login", nil, nil, "").Body.String()}
	rr := do(mux, http.MethodPost, "/ui/login", url.Values{"name": {"p"}, "token": {"secret"}}, nil, "")
	c := rr.Result().Cookies()[0]
	for _, p := range []string{"/ui/", "/ui/checks/pat", "/ui/resolved"} {
		pages[p] = do(mux, http.MethodGet, p, nil, c, "").Body.String()
	}
	local := regexp.MustCompile(`(?:href|src|action)="(/[^"]*)"`)
	for p, body := range pages {
		found := local.FindAllStringSubmatch(body, -1)
		if len(found) == 0 {
			t.Errorf("%s: no local URLs, page not rendered?", p)
		}
		for _, m := range found {
			if !strings.HasPrefix(m[1], prefix+"/") {
				t.Errorf("%s: local URL without the base path: %s", p, m[0])
			}
		}
	}
	if !strings.Contains(pages["/ui/login"], `name="next" value="`+prefix+`/ui/"`) {
		t.Error("login form's next lacks the prefix")
	}
}

func TestFramingAndForwardedOrigin(t *testing.T) {
	mux := setupBase(t)
	rr := do(mux, http.MethodGet, "/ui/login", nil, nil, "")
	if rr.Header().Get("X-Frame-Options") != "SAMEORIGIN" || rr.Header().Get("Content-Security-Policy") != "frame-ancestors 'self'" {
		t.Errorf("framing headers: %v", rr.Header())
	}
	// The portal proxies with the service's Host and the browser's host in
	// X-Forwarded-Host; a form posted from the portal's page is same-origin.
	login := func(forwarded string) int {
		r := httpRequest(http.MethodPost, "/ui/login", url.Values{"name": {"p"}, "token": {"secret"}})
		r.Host = "homerun2-schedule-pitcher"
		r.Header.Set("Origin", "https://portal.example.com")
		if forwarded != "" {
			r.Header.Set("X-Forwarded-Host", forwarded)
		}
		return serve(mux, r).Code
	}
	if code := login("portal.example.com"); code != http.StatusSeeOther {
		t.Errorf("login through the portal: %d", code)
	}
	if code := login(""); code != http.StatusForbidden {
		t.Errorf("cross-origin login without X-Forwarded-Host: %d", code)
	}
	if code := login("evil.example"); code != http.StatusForbidden {
		t.Errorf("mismatching X-Forwarded-Host: %d", code)
	}
}

func httpRequest(method, path string, form url.Values) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func serve(mux *http.ServeMux, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}
