package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func setupForwarded(t *testing.T, fa ForwardedAuth) (*http.ServeMux, *fakeFindings) {
	t.Helper()
	ff := &fakeFindings{}
	u := New(&fakeChecks{}, ff, NewSessions("secret"), "platform", "v1", "central", time.UTC)
	u.now = func() time.Time { return now }
	u.Forwarded = fa
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, ff
}

// fwd sends a request as sthings-portal would: with X-Forwarded-User and
// (when not empty) X-Forwarded-Groups.
func fwd(mux *http.ServeMux, method, path string, form url.Values, user, groups, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if user != "" {
		r.Header.Set(HeaderForwardedUser, user)
	}
	if groups != "" {
		r.Header.Set(HeaderForwardedGroups, groups)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

var trusted = ForwardedAuth{Trust: true, LogoutPath: PortalLogoutPath}

func TestForwardedUserIgnoredWhenOff(t *testing.T) {
	mux, _ := setupForwarded(t, ForwardedAuth{})
	rr := fwd(mux, http.MethodGet, "/ui/", nil, "mallory", "portal-admin", "", nil)
	if rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/ui/login") {
		t.Fatalf("spoofed header logged in: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if rr := fwd(mux, http.MethodGet, "/ui/login", nil, "mallory", "", "", nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Token") {
		t.Fatalf("login page: %d", rr.Code)
	}
	if rr := fwd(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"k"}}, "mallory", "", "http://example.com", nil); rr.Code != http.StatusSeeOther ||
		!strings.HasPrefix(rr.Header().Get("Location"), "/ui/login") {
		t.Fatalf("spoofed ack: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	body := fwd(mux, http.MethodGet, "/ui/fragment/findings", nil, "mallory", "", "", nil).Body.String()
	if !strings.Contains(body, "Log in to see open findings") || strings.Contains(body, "finding") && strings.Contains(body, "hr-item") {
		t.Fatalf("spoofed fragment: %s", body)
	}
	// The token login is unchanged.
	c := login(t, mux)
	if rr := fwd(mux, http.MethodGet, "/ui/", nil, "mallory", "", "", c); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "mallory") {
		t.Fatalf("session user: %d", rr.Code)
	}
}

func TestForwardedUserLogsIn(t *testing.T) {
	mux, ff := setupForwarded(t, trusted)
	rr := fwd(mux, http.MethodGet, "/ui/", nil, "patrick@sthings.example", "portal-user", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	body := rr.Body.String()
	if !strings.Contains(body, "patrick@sthings.example") {
		t.Error("header misses the forwarded user")
	}
	if !strings.Contains(body, `action="/auth/logout" target="_top"`) || strings.Contains(body, `action="/ui/logout"`) {
		t.Error("log out must end the portal session, not the local one")
	}
	if v := rr.Header().Values("Vary"); !slices.ContainsFunc(v, func(s string) bool { return strings.Contains(s, HeaderForwardedUser) }) {
		t.Errorf("Vary = %v", v)
	}
	// Acknowledgements record the forwarded name.
	rr = fwd(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"vm/disk:/var"}, "note": {"n"}}, "patrick@sthings.example", "", "http://example.com", nil)
	if ff.acked != "disk|vm/disk:/var|patrick@sthings.example|n" || !strings.Contains(rr.Header().Get("Location"), "flash=") {
		t.Errorf("ack: %q %s", ff.acked, rr.Header().Get("Location"))
	}
	// The login page sends a forwarded user on.
	if rr := fwd(mux, http.MethodGet, "/ui/login?next=/ui/resolved", nil, "p", "", "", nil); rr.Code != http.StatusFound || rr.Header().Get("Location") != "/ui/resolved" {
		t.Errorf("login page: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	// The fragment counts it as a session.
	if body := fwd(mux, http.MethodGet, "/ui/fragment/findings", nil, "p", "", "", nil).Body.String(); !strings.Contains(body, "1 open") {
		t.Errorf("fragment: %s", body)
	}
	// Without LogoutPath the button is hidden.
	mux2, _ := setupForwarded(t, ForwardedAuth{Trust: true})
	if body := fwd(mux2, http.MethodGet, "/ui/", nil, "p", "", "", nil).Body.String(); strings.Contains(body, "Log out") {
		t.Error("log out shown without a portal logout")
	}
}

func TestForwardedUserMissingFallsBackToTokenLogin(t *testing.T) {
	mux, _ := setupForwarded(t, trusted)
	if rr := fwd(mux, http.MethodGet, "/ui/", nil, "", "", "", nil); rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/ui/login") {
		t.Fatalf("no header: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	c := login(t, mux)
	rr := fwd(mux, http.MethodGet, "/ui/", nil, "", "", "", c)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `action="/ui/logout"`) {
		t.Fatalf("token session: %d", rr.Code)
	}
}

func TestForwardedGroups(t *testing.T) {
	fa := trusted
	fa.Groups = []string{"portal-admin", "ops"}
	mux, ff := setupForwarded(t, fa)

	for _, groups := range []string{"portal-admin", "portal-user, ops", "portal-user,portal-admin"} {
		if rr := fwd(mux, http.MethodGet, "/ui/", nil, "p", groups, "", nil); rr.Code != http.StatusOK {
			t.Errorf("groups %q: %d", groups, rr.Code)
		}
	}
	for _, groups := range []string{"", "portal-user", "Portal-Admin", "portal-admins", "kiosk"} {
		rr := fwd(mux, http.MethodGet, "/ui/", nil, "p", groups, "", nil)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "Not allowed") || !strings.Contains(rr.Body.String(), "portal-admin") {
			t.Errorf("groups %q: %d", groups, rr.Code)
		}
	}
	// Denied is final: not the token login page, not a token login, not a
	// still valid session cookie.
	if rr := fwd(mux, http.MethodGet, "/ui/login", nil, "p", "portal-user", "", nil); rr.Code != http.StatusForbidden {
		t.Errorf("login page for a denied user: %d", rr.Code)
	}
	if rr := fwd(mux, http.MethodPost, "/ui/login", url.Values{"name": {"p"}, "token": {"secret"}}, "p", "portal-user", "", nil); rr.Code != http.StatusForbidden || len(rr.Result().Cookies()) != 0 {
		t.Errorf("token login for a denied user: %d", rr.Code)
	}
	c := login(t, mux)
	if rr := fwd(mux, http.MethodPost, "/ui/findings/ack", url.Values{"source": {"disk"}, "key": {"k"}}, "p", "portal-user", "http://example.com", c); rr.Code != http.StatusForbidden || ff.acked != "" {
		t.Errorf("ack by a denied user: %d %q", rr.Code, ff.acked)
	}
	rr := fwd(mux, http.MethodGet, "/ui/fragment/findings", nil, "p", "portal-user", "", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "not allowed") || strings.Contains(rr.Body.String(), "hr-item") {
		t.Errorf("fragment for a denied user: %d %s", rr.Code, rr.Body.String())
	}
}

func TestForwardedUserCSRF(t *testing.T) {
	mux, ff := setupForwarded(t, trusted)
	form := url.Values{"source": {"disk"}, "key": {"k"}}
	if rr := fwd(mux, http.MethodPost, "/ui/findings/ack", form, "p", "", "https://evil.example", nil); rr.Code != http.StatusForbidden {
		t.Errorf("cross-site ack: %d", rr.Code)
	}
	if rr := fwd(mux, http.MethodPost, "/ui/findings/ack", form, "p", "", "", nil); rr.Code != http.StatusForbidden {
		t.Errorf("ack without Origin: %d", rr.Code)
	}
	if rr := fwd(mux, http.MethodPost, "/ui/checks/pat/run", url.Values{}, "p", "", "https://evil.example", nil); rr.Code != http.StatusForbidden {
		t.Errorf("cross-site run: %d", rr.Code)
	}
	if ff.acked != "" {
		t.Errorf("acked %q", ff.acked)
	}
}

func TestForwardedUserInvalidName(t *testing.T) {
	mux, _ := setupForwarded(t, trusted)
	for _, name := range []string{strings.Repeat("a", maxForwardedUser+1), " ", "a\x7fb"} {
		if rr := fwd(mux, http.MethodGet, "/ui/", nil, name, "", "", nil); rr.Code != http.StatusForbidden {
			t.Errorf("name %q: %d", name, rr.Code)
		}
	}
}

func TestParseGroups(t *testing.T) {
	if got := ParseGroups(" a, ,b,,c "); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("ParseGroups = %q", got)
	}
	if got := ParseGroups(""); got != nil {
		t.Errorf("ParseGroups(\"\") = %q", got)
	}
}
