package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

var logHealthChecks = strings.EqualFold(os.Getenv("LOG_HEALTH_CHECKS"), "true")

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// RequestLogging logs every HTTP request with method, path, status, duration, request ID, and client IP.
func RequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set("X-Request-Id", requestID)

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		if isProbe(r.URL.Path) && !logHealthChecks {
			return
		}

		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestID,
			"client_ip", clientIP(r),
		)
	})
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return forwarded
	}
	return r.RemoteAddr
}

func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isProbe reports health, readiness and metrics scrapes, which are not logged
// unless LOG_HEALTH_CHECKS=true.
func isProbe(path string) bool {
	return path == "/health" || path == "/ready" || path == "/metrics"
}
