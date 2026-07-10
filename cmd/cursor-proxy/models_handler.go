// models_handler.go implements the extended /v1/models surface.
//
// The catalog itself lives on executor.Client and is unchanged; this file
// adds the per-id lookup at `/v1/models/{id}`, which matches OpenAI's REST
// convention. It also exposes a small `modelLister` interface so both the
// list and detail handlers can be exercised with a stub in tests.
package main

import (
	"encoding/json"
	"net/http"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// modelLister is the executor subset the models handlers depend on.
type modelLister interface {
	ListModels() (*cursorpb.AiserverV1_AvailableModelsResponse, error)
}

// listModels returns the OpenAI-shaped `{object:"list", data:[{id,object,owned_by},...]}`
// payload used by both /v1/models and /v1/models/{id}. Extracted here so
// the detail handler can filter by id without a second upstream call.
func listModels(c modelLister) ([]map[string]any, error) {
	resp, err := c.ListModels()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resp.Models))
	for _, m := range resp.Models {
		out = append(out, map[string]any{
			"id":       m.GetName(),
			"object":   "model",
			"owned_by": "cursor",
		})
	}
	return out, nil
}

// modelDetailHandler serves `GET /v1/models/{id}`. Returns 404 with an
// OpenAI-shape error body when the id is not in the catalog.
func modelDetailHandler(c modelLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "missing model id", http.StatusBadRequest)
			return
		}
		list, err := listModels(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, m := range list {
			if m["id"] == id {
				w.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(w).Encode(m)
				return
			}
		}
		writeModelNotFound(w, id)
	}
}

// writeModelNotFound returns the same 404 shape OpenAI does so SDKs that
// switch on error.code degrade gracefully.
func writeModelNotFound(w http.ResponseWriter, id string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "The model '" + id + "' does not exist",
			"type":    "invalid_request_error",
			"param":   "id",
			"code":    "model_not_found",
		},
	})
}
