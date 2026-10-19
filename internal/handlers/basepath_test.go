package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMount(t *testing.T) {
	const (
		get, post = http.MethodGet, http.MethodPost
		sched     = "/schedule"
	)
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("app " + r.Method + " " + r.URL.Path))
	})

	cases := []struct {
		basePath, method, path string
		code                   int
		body, location         string
	}{
		{"", get, "/ui/", 200, "app GET /ui/", ""},
		{"", post, "/findings", 200, "app POST /findings", ""},
		{sched, get, sched + "/ui/", 200, "app GET /ui/", ""},
		{sched, post, sched + "/ui/login", 200, "app POST /ui/login", ""},
		{sched, get, sched + "/", 200, "app GET /", ""},
		{sched, get, sched, 307, "", sched + "/"},
		// Root endpoints for agents, scripts, probes and Prometheus.
		{sched, post, "/findings", 200, "app POST /findings", ""},
		{sched, get, "/api/checks", 200, "app GET /api/checks", ""},
		{sched, get, "/health", 200, "app GET /health", ""},
		{sched, get, "/ready", 200, "app GET /ready", ""},
		{sched, get, "/metrics", 200, "app GET /metrics", ""},
		{sched, get, "/", 200, "app GET /", ""},
		// The UI's old root paths lead under the prefix.
		{sched, get, "/ui/checks/pat?flash=x", 307, "", sched + "/ui/checks/pat?flash=x"},
		{"/ui", get, "/ui/", 200, "app GET /", ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		Mount(tc.basePath, app).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, http.NoBody))
		if rec.Code != tc.code {
			t.Errorf("base %q %s %s: code %d, want %d", tc.basePath, tc.method, tc.path, rec.Code, tc.code)
			continue
		}
		if tc.body != "" && rec.Body.String() != tc.body {
			t.Errorf("base %q %s %s: body %q, want %q", tc.basePath, tc.method, tc.path, rec.Body.String(), tc.body)
		}
		if loc := rec.Header().Get("Location"); tc.location != "" && loc != tc.location {
			t.Errorf("base %q %s %s: Location %q, want %q", tc.basePath, tc.method, tc.path, loc, tc.location)
		}
	}
}
