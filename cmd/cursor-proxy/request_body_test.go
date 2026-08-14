package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRequestRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"value":"`+strings.Repeat("x", int(maxJSONRequestBody))+`"}`))
	rec := httptest.NewRecorder()
	var dst map[string]any
	err := decodeJSONRequest(rec, req, &dst, false)
	if err == nil {
		t.Fatal("expected oversized body error")
	}
	if got := jsonRequestErrorStatus(err); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (err=%v)", got, err)
	}
}

func TestDecodeJSONRequestRejectsTrailingValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{} {}`))
	rec := httptest.NewRecorder()
	var dst map[string]any
	if err := decodeJSONRequest(rec, req, &dst, false); err == nil {
		t.Fatal("expected trailing JSON value error")
	}
}
