package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/skvostik/container-redeploy-webhook/internal/auth"
	"github.com/skvostik/container-redeploy-webhook/internal/redeploy"
)

const token = "server-test-token-123"

type stubService struct {
	err    error
	called bool
	force  bool
}

func (s *stubService) Redeploy(_ context.Context, id string, _ *auth.Principal, force bool) (*redeploy.Result, error) {
	s.called, s.force = true, force
	if s.err != nil {
		return nil, s.err
	}
	return &redeploy.Result{ID: id, Containers: []redeploy.ContainerResult{{Name: "app", Status: redeploy.StatusUpdated}}}, nil
}

func do(t *testing.T, svc *stubService, method, path, authz string) *httptest.ResponseRecorder {
	t.Helper()
	a, err := auth.New(token, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: a, Service: svc, Logger: slog.New(slog.DiscardHandler), Timeout: time.Minute}
	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRedeployOK(t *testing.T) {
	svc := &stubService{}
	rec := do(t, svc, http.MethodPost, "/redeploy/my-app?force=1", "Bearer "+token)
	if rec.Code != http.StatusOK || !svc.force {
		t.Fatalf("code=%d force=%v body=%s", rec.Code, svc.force, rec.Body)
	}
	var res redeploy.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.ID != "my-app" {
		t.Fatalf("body=%s err=%v", rec.Body, err)
	}
}

func TestStatusCodes(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		authz  string
		err    error
		code   int
		called bool
	}{
		{"no auth", "POST", "/redeploy/app", "", nil, 401, false},
		{"wrong token", "POST", "/redeploy/app", "Bearer nope", nil, 401, false},
		{"lowercase bearer", "POST", "/redeploy/app", "bearer " + token, nil, 200, true},
		{"not found looks like unauthorized", "POST", "/redeploy/app", "Bearer " + token, redeploy.ErrNotFound, 401, true},
		{"unauthorized", "POST", "/redeploy/app", "Bearer " + token, redeploy.ErrUnauthorized, 401, true},
		{"conflict", "POST", "/redeploy/app", "Bearer " + token, fmt.Errorf("%w: stopped", redeploy.ErrConflict), 409, true},
		{"failure", "POST", "/redeploy/app", "Bearer " + token, fmt.Errorf("%w: pull", redeploy.ErrFailed), 500, true},
		{"invalid id", "POST", "/redeploy/..bad", "Bearer " + token, nil, 400, false},
		{"GET not allowed", "GET", "/redeploy/app", "Bearer " + token, nil, 405, false},
		{"healthz", "GET", "/healthz", "", nil, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &stubService{err: tt.err}
			rec := do(t, svc, tt.method, tt.path, tt.authz)
			if rec.Code != tt.code || svc.called != tt.called {
				t.Fatalf("code=%d called=%v body=%s", rec.Code, svc.called, rec.Body)
			}
		})
	}
}
