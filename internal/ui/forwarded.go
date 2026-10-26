package ui

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// Forwarded identity (#62): behind sthings-portal the user has already logged
// in (ZITADEL OIDC). The portal drops whatever a client sent as
// X-Forwarded-User / X-Forwarded-Groups and sets both itself after its login,
// so with AUTH_TRUST_FORWARDED_USER the UI takes the name from there instead of
// asking for the token again. The headers only authorize the UI and its
// fragment; /api/* and POST /findings stay bearer-only (middleware), they
// never look at them.
//
// The headers are only trustworthy when every path to the pod goes through the
// portal: the direct HTTPRoute removes them (kcl/service.k), callers inside
// the cluster could still forge them (README, "Login through sthings-portal").

const (
	// HeaderForwardedUser and HeaderForwardedGroups are the names
	// sthings-portal uses (internal/auth/auth.go there); groups are
	// comma-separated.
	HeaderForwardedUser   = "X-Forwarded-User"
	HeaderForwardedGroups = "X-Forwarded-Groups"
	// PortalLogoutPath ends the session at sthings-portal (POST, on the
	// portal's origin, which is the page's origin inside the portal).
	PortalLogoutPath = "/auth/logout"
	// maxForwardedUser bounds the name taken from the header (ZITADEL user
	// names are e-mail addresses).
	maxForwardedUser = 128
)

// ForwardedAuth configures the forwarded identity. The zero value is off.
type ForwardedAuth struct {
	// Trust takes X-Forwarded-User as the logged-in user
	// (AUTH_TRUST_FORWARDED_USER).
	Trust bool
	// Groups, when not empty, lets only users in one of them in
	// (AUTH_FORWARDED_GROUPS); exact, case-sensitive match.
	Groups []string
	// LogoutPath is where "Log out" posts for a forwarded user; empty hides
	// the button.
	LogoutPath string
}

// ParseGroups splits a comma-separated list, dropping blanks.
func ParseGroups(s string) []string {
	var out []string
	for g := range strings.SplitSeq(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// identity is who a request is, and how we know.
type identity struct {
	Name string
	// Forwarded: the name came from X-Forwarded-User.
	Forwarded bool
	// Denied: a forwarded user that AUTH_FORWARDED_GROUPS (or an unusable
	// name) keeps out. Such a request never falls back to the token login.
	Denied bool
}

func (id identity) ok() bool { return id.Name != "" && !id.Denied }

// identify returns the user of a request: the forwarded identity when it is
// trusted and present, otherwise the session cookie.
func (u *UI) identify(r *http.Request) identity {
	if u.Forwarded.Trust {
		if v := r.Header.Get(HeaderForwardedUser); v != "" {
			name := strings.TrimSpace(v)
			if !validForwardedUser(name) {
				return identity{Forwarded: true, Denied: true}
			}
			if !u.Forwarded.groupAllowed(r.Header.Get(HeaderForwardedGroups)) {
				return identity{Name: name, Forwarded: true, Denied: true}
			}
			return identity{Name: name, Forwarded: true}
		}
	}
	if name, ok := u.Sessions.User(r); ok {
		return identity{Name: name}
	}
	return identity{}
}

func (f ForwardedAuth) groupAllowed(header string) bool {
	if len(f.Groups) == 0 {
		return true
	}
	return slices.ContainsFunc(ParseGroups(header), func(g string) bool { return slices.Contains(f.Groups, g) })
}

func validForwardedUser(name string) bool {
	if name == "" || len(name) > maxForwardedUser {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

// forbidden answers a forwarded user who may not use the UI: a 403 page,
// not the token login (the portal decided who they are).
func (u *UI) forbidden(w http.ResponseWriter, r *http.Request, id identity) {
	slog.Warn("ui forwarded user denied", "user", id.Name, "groups", r.Header.Get(HeaderForwardedGroups))
	u.renderStatus(w, r, http.StatusForbidden, "forbidden.html", map[string]any{"Name": id.Name, "Groups": u.Forwarded.Groups})
}
