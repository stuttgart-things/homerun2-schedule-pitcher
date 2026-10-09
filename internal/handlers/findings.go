package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
)

// FindingsService is what the findings endpoints need.
type FindingsService interface {
	Ingest(ctx context.Context, r findings.Report) (findings.IngestResult, error)
	Acknowledge(ctx context.Context, source, key, by, note string) (findings.Finding, error)
	List(ctx context.Context, status, source string) ([]findings.Finding, error)
}

// RemindersService is what the reminder handlers need.
type RemindersService interface {
	Reminders(ctx context.Context) ([]findings.ReminderStatus, error)
	ReminderDone(ctx context.Context, id, by string) (findings.ReminderStatus, error)
}

// NewRemindersHandler serves GET /api/reminders.
func NewRemindersHandler(s RemindersService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Reminders(r.Context())
		if err != nil {
			errorJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, list)
	}
}

// NewReminderDoneHandler serves POST /api/reminders/{id}/done with an
// optional body {"by": "..."}.
func NewReminderDoneHandler(s RemindersService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req struct {
			By string `json:"by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			errorJSON(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if strings.TrimSpace(req.By) == "" {
			req.By = "api"
		}
		st, err := s.ReminderDone(r.Context(), r.PathValue("id"), req.By)
		switch {
		case errors.Is(err, findings.ErrUnknownReminder):
			errorJSON(w, http.StatusNotFound, err.Error())
		case err != nil:
			errorJSON(w, http.StatusServiceUnavailable, err.Error())
		default:
			respondWithJSON(w, http.StatusOK, st)
		}
	}
}

func errorJSON(w http.ResponseWriter, code int, msg string) {
	respondWithJSON(w, code, map[string]string{"status": "error", "message": msg})
}

// NewIngestHandler serves POST /findings: the complete current set of
// findings of one source.
func NewIngestHandler(s FindingsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, findings.MaxBodyBytes)
		var rep findings.Report
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rep); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				errorJSON(w, http.StatusRequestEntityTooLarge, "body too large")
				return
			}
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := rep.Validate(); err != nil {
			errorJSON(w, http.StatusBadRequest, strings.ReplaceAll(err.Error(), "\n", "; "))
			return
		}
		res, err := s.Ingest(r.Context(), rep)
		switch {
		case errors.Is(err, findings.ErrBusy):
			errorJSON(w, http.StatusConflict, err.Error())
		case err != nil:
			errorJSON(w, http.StatusServiceUnavailable, err.Error())
		default:
			respondWithJSON(w, http.StatusOK, res)
		}
	}
}

// NewFindingsHandler serves GET /api/findings?status=open|acknowledged|resolved&source=.
func NewFindingsHandler(s FindingsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		status := q.Get("status")
		switch status {
		case "", "open", findings.StatusAcknowledged, findings.StatusResolved:
		default:
			errorJSON(w, http.StatusBadRequest, "status must be open, acknowledged or resolved")
			return
		}
		list, err := s.List(r.Context(), status, q.Get("source"))
		if err != nil {
			errorJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if list == nil {
			list = []findings.Finding{}
		}
		respondWithJSON(w, http.StatusOK, list)
	}
}

type ackRequest struct {
	Source string `json:"source"`
	Key    string `json:"key"`
	By     string `json:"by"`
	Note   string `json:"note"`
}

// NewAckHandler serves POST /api/findings/ack with {source, key, by, note}.
// The key is in the body because keys contain slashes.
func NewAckHandler(s FindingsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req ackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.Source == "" || req.Key == "" || strings.TrimSpace(req.By) == "" {
			errorJSON(w, http.StatusBadRequest, "source, key and by are required")
			return
		}
		f, err := s.Acknowledge(r.Context(), req.Source, req.Key, req.By, req.Note)
		switch {
		case errors.Is(err, findings.ErrNotFound):
			errorJSON(w, http.StatusNotFound, err.Error())
		case errors.Is(err, findings.ErrBusy):
			errorJSON(w, http.StatusConflict, err.Error())
		case err != nil:
			errorJSON(w, http.StatusConflict, err.Error())
		default:
			respondWithJSON(w, http.StatusOK, f)
		}
	}
}
