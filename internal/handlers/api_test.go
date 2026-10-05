package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

type fakeScheduler struct {
	statuses []scheduler.CheckStatus
	runErr   error
	ran      string
}

func (f *fakeScheduler) Statuses(context.Context) ([]scheduler.CheckStatus, error) {
	return f.statuses, nil
}
func (f *fakeScheduler) Run(_ context.Context, id string) (state.State, error) {
	f.ran = id
	return state.State{}, f.runErr
}

func newFake() *fakeScheduler {
	exp := time.Date(2027, 3, 28, 0, 0, 0, 0, time.UTC)
	st := state.New("pat")
	st.Band, st.Expiry = status.Warning, exp
	return &fakeScheduler{statuses: []scheduler.CheckStatus{{
		Check: profile.Check{ID: "pat", Type: profile.TypeGitHubTokenExpiry, Thresholds: profile.DefaultThresholds},
		State: st, Band: "warning",
	}}}
}

func TestChecksHandler(t *testing.T) {
	rr := httptest.NewRecorder()
	NewChecksHandler(newFake())(rr, httptest.NewRequest(http.MethodGet, "/api/checks", nil))
	var views []CheckView
	if err := json.NewDecoder(rr.Body).Decode(&views); err != nil || len(views) != 1 {
		t.Fatalf("views = %+v, %v", views, err)
	}
	v := views[0]
	if v.ID != "pat" || v.Band != "warning" || v.Expiry == nil || v.LastRun != nil || v.Thresholds != [3]string{"30d", "7d", "1d"} {
		t.Fatalf("view = %+v", v)
	}
}

func TestRunHandler(t *testing.T) {
	tests := []struct {
		err  error
		code int
	}{
		{nil, http.StatusOK},
		{scheduler.ErrUnknownCheck, http.StatusNotFound},
		{scheduler.ErrBusy, http.StatusConflict},
	}
	for _, tt := range tests {
		f := newFake()
		f.runErr = tt.err
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/checks/{id}/run", NewRunHandler(f))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/checks/pat/run", nil))
		if rr.Code != tt.code || f.ran != "pat" {
			t.Errorf("err %v: code = %d ran = %q", tt.err, rr.Code, f.ran)
		}
	}
}

func TestReadyHandler(t *testing.T) {
	var ready atomic.Bool
	h := NewReadyHandler(&ready)
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rr.Code)
	}
	ready.Store(true)
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
}
