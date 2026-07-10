package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// fakeModelLister returns a canned AvailableModelsResponse.
type fakeModelLister struct {
	names []string
	err   error
}

func (f *fakeModelLister) ListModels() (*cursorpb.AiserverV1_AvailableModelsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	models := make([]*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel, 0, len(f.names))
	for _, n := range f.names {
		models = append(models, &cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{Name: n})
	}
	return &cursorpb.AiserverV1_AvailableModelsResponse{Models: models}, nil
}

func TestModelDetailHandler_Found(t *testing.T) {
	lister := &fakeModelLister{names: []string{"composer-2.5", "claude-4.5-sonnet"}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models/{id}", modelDetailHandler(lister))

	req := httptest.NewRequest(http.MethodGet, "/v1/models/composer-2.5", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if got["id"].(string) != "composer-2.5" {
		t.Fatalf("id = %v, want composer-2.5", got["id"])
	}
	if got["object"].(string) != "model" {
		t.Fatalf("object = %v, want model", got["object"])
	}
	if got["owned_by"].(string) != "cursor" {
		t.Fatalf("owned_by = %v, want cursor", got["owned_by"])
	}
}

func TestModelDetailHandler_NotFound(t *testing.T) {
	lister := &fakeModelLister{names: []string{"composer-2.5"}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models/{id}", modelDetailHandler(lister))

	req := httptest.NewRequest(http.MethodGet, "/v1/models/does-not-exist", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var got struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil {
		t.Fatalf("expected error body, got: %s", rec.Body.String())
	}
	if got.Error["code"].(string) != "model_not_found" {
		t.Fatalf("error.code = %v, want model_not_found", got.Error["code"])
	}
	if !strings.Contains(got.Error["message"].(string), "does-not-exist") {
		t.Fatalf("error.message should mention id, got %q", got.Error["message"])
	}
}

func TestModelDetailHandler_UpstreamError(t *testing.T) {
	lister := &fakeModelLister{err: errors.New("boom")}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models/{id}", modelDetailHandler(lister))

	req := httptest.NewRequest(http.MethodGet, "/v1/models/x", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
