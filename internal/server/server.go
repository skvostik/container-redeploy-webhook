// Package server exposes the redeploy service over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/skvostik/container-redeploy-webhook/internal/auth"
	"github.com/skvostik/container-redeploy-webhook/internal/redeploy"
)

// Redeployer is implemented by *redeploy.Service.
type Redeployer interface {
	Redeploy(ctx context.Context, id string, p *auth.Principal, force bool) (*redeploy.Result, error)
}

// Server handles webhook requests.
type Server struct {
	Auth    *auth.Authorizer
	Service Redeployer
	Logger  *slog.Logger
	Timeout time.Duration // overall redeploy timeout
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /redeploy/{id}", s.handleRedeploy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

type errorBody struct {
	Error  string           `json:"error"`
	Result *redeploy.Result `json:"result,omitempty"`
}

func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid id"})
		return
	}
	force := isTrue(r.URL.Query().Get("force"))

	p, err := s.Auth.Authenticate(r.Context(), bearerToken(r))
	if err != nil {
		s.Logger.Warn("rejected: authentication failed", "id", id, "remote", r.RemoteAddr, "err", err)
		unauthorized(w)
		return
	}

	// Don't abort half-way through a recreate when the client disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.Timeout)
	defer cancel()

	res, err := s.Service.Redeploy(ctx, id, p, force)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, redeploy.ErrNotFound), errors.Is(err, redeploy.ErrUnauthorized):
		// Same response for both, so ids cannot be enumerated.
		unauthorized(w)
	case errors.Is(err, redeploy.ErrConflict):
		s.Logger.Warn("rejected", "id", id, "err", err)
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	default:
		s.Logger.Error("redeploy failed", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error(), Result: res})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer`)
	writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized"})
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
