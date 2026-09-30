package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/models"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/pitcher"

	homerun "github.com/stuttgart-things/homerun-library/v3"
)

// NewPitchHandler creates a pitch handler with the given Pitcher backend.
func NewPitchHandler(p pitcher.Pitcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var msg homerun.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid JSON payload")
			return
		}

		if msg.Title == "" {
			respondWithError(w, http.StatusBadRequest, "Title is required")
			return
		}
		if msg.Message == "" {
			respondWithError(w, http.StatusBadRequest, "Message is required")
			return
		}

		if msg.Severity == "" {
			msg.Severity = "info"
		}
		if msg.Author == "" {
			msg.Author = "unknown"
		}
		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().Format(time.RFC3339)
		}
		if msg.System == "" {
			msg.System = "homerun2-schedule-pitcher"
		}

		objectID, streamID, err := p.Pitch(msg)
		if err != nil {
			slog.Error("failed to pitch message", "error", err)
			respondWithError(w, http.StatusServiceUnavailable, "Failed to enqueue message")
			return
		}

		respondWithJSON(w, http.StatusOK, models.PitchResponse{
			ObjectID: objectID,
			StreamID: streamID,
			Status:   "success",
			Message:  "Message successfully enqueued",
		})

		slog.Info("message pitched", "objectID", objectID, "streamID", streamID)
	}
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, models.PitchResponse{
		Status:  "error",
		Message: message,
	})
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("error encoding response", "error", err)
	}
}
