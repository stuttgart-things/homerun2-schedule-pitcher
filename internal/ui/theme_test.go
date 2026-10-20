package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const portalTokensLink = `<link rel="stylesheet" href="/static/homerun2-tokens.css">`

func TestThePortalTokensAreLinkedOnlyBehindThePortal(t *testing.T) {
	mux := setupBase(t)

	if rr := do(mux, http.MethodGet, "/ui/login", nil, nil, ""); strings.Contains(rr.Body.String(), portalTokensLink) {
		t.Error("the page links the portal's tokens without the portal")
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/login", http.NoBody)
	req.Header.Set("X-Forwarded-Prefix", prefix)
	behind := httptest.NewRecorder()
	mux.ServeHTTP(behind, req)
	body := behind.Body.String()
	if i := strings.Index(body, portalTokensLink); i < 0 || i < strings.Index(body, "</style>") {
		t.Error("behind the portal the page does not link its tokens after its own copy")
	}
}

func TestThePageSwitchesToTheCompactHeaderInsideThePortal(t *testing.T) {
	mux, _, _ := setup(t, true)
	rr := do(mux, http.MethodGet, "/ui/login", nil, nil, "")
	head, _, _ := strings.Cut(rr.Body.String(), "</head>")
	script := strings.Index(head, "classList.add('hr-embedded')")
	if script < 0 || script > strings.Index(head, "<style>") {
		t.Error("the hr-embedded script is missing or runs after the styles")
	}
	if !strings.Contains(head, ".hr-embedded .header-bar {") {
		t.Error("no compact header styles")
	}
}
