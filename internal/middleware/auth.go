package middleware

import (
	"fmt"
	"net/http"
	"strings"

	homerun "github.com/stuttgart-things/homerun-library/v3"
)

// TokenAuthMiddleware validates bearer token authentication
func TokenAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			respondWithAuthError(w, http.StatusUnauthorized, "Missing authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			respondWithAuthError(w, http.StatusUnauthorized, "Invalid authorization header format")
			return
		}

		token := parts[1]
		expectedToken := homerun.GetEnv("AUTH_TOKEN", "")

		if expectedToken == "" {
			respondWithAuthError(w, http.StatusInternalServerError, "Server authentication not configured")
			return
		}

		if token != expectedToken {
			respondWithAuthError(w, http.StatusUnauthorized, "Invalid token")
			return
		}

		next.ServeHTTP(w, r)
	}
}

func respondWithAuthError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"status":"error","message":"%s"}`, message)
}
