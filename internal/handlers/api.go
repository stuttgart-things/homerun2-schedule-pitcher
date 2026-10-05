package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/scheduler"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/store"
)

// Scheduler is what the API needs from the scheduler.
type Scheduler interface {
	Statuses(ctx context.Context) ([]scheduler.CheckStatus, error)
	Run(ctx context.Context, id string) (state.State, error)
	History(ctx context.Context, id string, limit int) ([]store.HistoryEntry, error)
}

// CheckView is the API representation of a check.
type CheckView struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Description string     `json:"description,omitempty"`
	Target      string     `json:"target,omitempty"`
	Schedule    string     `json:"schedule"`
	Remind      string     `json:"remind"`
	Thresholds  [3]string  `json:"thresholds"`
	Tags        []string   `json:"tags,omitempty"`
	URL         string     `json:"url,omitempty"`
	Assignee    string     `json:"assignee,omitempty"`
	Paused      bool       `json:"paused"`
	Source      string     `json:"source"`
	Band        string     `json:"band"`
	Failing     bool       `json:"failing"`
	LastError   string     `json:"lastError,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	Subject     string     `json:"subject,omitempty"`
	Problem     string     `json:"problem,omitempty"`
	Expiry      *time.Time `json:"expiry,omitempty"`
	LastRun     *time.Time `json:"lastRun,omitempty"`
	LastOK      *time.Time `json:"lastOk,omitempty"`
	NextRun     *time.Time `json:"nextRun,omitempty"`
}

func viewOf(cs scheduler.CheckStatus) CheckView {
	c, s := cs.Check, cs.State
	t := c.Thresholds
	return CheckView{
		ID: c.ID, Type: c.Type, Description: c.Description, Target: c.Target,
		Schedule: c.Schedule, Remind: c.Remind,
		Thresholds: [3]string{t.Warning.String(), t.Error.String(), t.Critical.String()},
		Tags:       c.Tags, URL: c.URL, Assignee: c.Assignee, Paused: c.Paused,
		Source: c.Origin, Band: cs.Band, Failing: s.Failing, LastError: s.LastError,
		Summary: s.Summary, Subject: s.Subject, Problem: s.Problem,
		Expiry: timePtr(s.Expiry), LastRun: timePtr(s.LastRun), LastOK: timePtr(s.LastOK),
		NextRun: cs.NextRun,
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// NewChecksHandler serves GET /api/checks.
func NewChecksHandler(s Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		statuses, err := s.Statuses(r.Context())
		if err != nil {
			respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "message": err.Error()})
			return
		}
		views := make([]CheckView, 0, len(statuses))
		for _, cs := range statuses {
			views = append(views, viewOf(cs))
		}
		respondWithJSON(w, http.StatusOK, views)
	}
}

// NewRunHandler serves POST /api/checks/{id}/run.
func NewRunHandler(s Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_, err := s.Run(r.Context(), id)
		switch {
		case errors.Is(err, scheduler.ErrUnknownCheck):
			respondWithJSON(w, http.StatusNotFound, map[string]string{"status": "error", "message": "unknown check " + id})
			return
		case errors.Is(err, scheduler.ErrBusy):
			respondWithJSON(w, http.StatusConflict, map[string]string{"status": "error", "message": err.Error()})
			return
		case err != nil:
			slog.Error("run now failed", "check", id, "error", err)
			respondWithJSON(w, http.StatusBadGateway, map[string]string{"status": "error", "message": err.Error()})
			return
		}
		statuses, err := s.Statuses(r.Context())
		if err == nil {
			for _, cs := range statuses {
				if cs.Check.ID == id {
					respondWithJSON(w, http.StatusOK, viewOf(cs))
					return
				}
			}
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "success"})
	}
}

// NewHistoryHandler serves GET /api/checks/{id}/history?limit=N (default 20).
func NewHistoryHandler(s Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > store.HistoryLimit {
				respondWithJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": "limit must be 1.." + strconv.Itoa(store.HistoryLimit)})
				return
			}
			limit = n
		}
		h, err := s.History(r.Context(), id, limit)
		switch {
		case errors.Is(err, scheduler.ErrUnknownCheck):
			respondWithJSON(w, http.StatusNotFound, map[string]string{"status": "error", "message": "unknown check " + id})
		case err != nil:
			respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "message": err.Error()})
		default:
			respondWithJSON(w, http.StatusOK, h)
		}
	}
}

// NewReadyHandler answers 200 once ready is set and ping (if any) succeeds.
func NewReadyHandler(ready *atomic.Bool, ping func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		if ping != nil {
			if err := ping(r.Context()); err != nil {
				respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "message": "store: " + err.Error()})
				return
			}
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("error encoding response", "error", err)
	}
}
