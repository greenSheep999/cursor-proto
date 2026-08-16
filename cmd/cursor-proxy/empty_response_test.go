package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestNonStreamOpenAIRejectsEmptyZeroUsageUpstream(t *testing.T) {
	events := make(chan executor.ChatEvent, 1)
	events <- turnEndedEvent(0, 0, 0)
	close(events)

	rec := httptest.NewRecorder()
	nonStreamOpenAI(rec, "claude-test", events, simCacheDecision{}, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty response") {
		t.Fatalf("body = %q, want empty-response diagnostic", rec.Body.String())
	}
}

func TestNonStreamAnthropicRejectsEmptyZeroUsageUpstream(t *testing.T) {
	events := make(chan executor.ChatEvent, 1)
	events <- turnEndedEvent(0, 0, 0)
	close(events)

	rec := httptest.NewRecorder()
	nonStreamAnthropic(rec, "claude-test", events, simCacheDecision{}, nil, false)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty response") {
		t.Fatalf("body = %q, want empty-response diagnostic", rec.Body.String())
	}
}

func TestAllProtocolSurfacesRejectEmptyZeroUsageUpstream(t *testing.T) {
	tests := []struct {
		name string
		run  func(http.ResponseWriter, <-chan executor.ChatEvent)
	}{
		{
			name: "openai-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				streamOpenAI(w, "claude-test", events, true, simCacheDecision{}, nil)
			},
		},
		{
			name: "anthropic-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				streamAnthropic(w, "claude-test", events, simCacheDecision{}, nil, false)
			},
		},
		{
			name: "gemini-non-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				nonStreamGemini(w, "gemini-test", events, simCacheDecision{}, nil)
			},
		},
		{
			name: "gemini-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				streamGemini(w, "gemini-test", events, simCacheDecision{}, nil)
			},
		},
		{
			name: "responses-non-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				nonStreamResponses(w, "gpt-test", events, simCacheDecision{}, nil)
			},
		},
		{
			name: "responses-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				streamResponses(w, "gpt-test", events, simCacheDecision{}, nil)
			},
		},
		{
			name: "legacy-non-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				nonStreamLegacyCompletions(w, "gpt-test", events, simCacheDecision{})
			},
		},
		{
			name: "legacy-stream",
			run: func(w http.ResponseWriter, events <-chan executor.ChatEvent) {
				streamLegacyCompletions(w, "gpt-test", events, simCacheDecision{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := make(chan executor.ChatEvent, 1)
			events <- turnEndedEvent(0, 0, 0)
			close(events)

			rec := httptest.NewRecorder()
			tt.run(rec, events)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "empty response") {
				t.Fatalf("body = %q, want empty-response diagnostic", rec.Body.String())
			}
		})
	}
}

func TestEmptyContentWithEffectiveUsageIsNotClassifiedAsTransportFailure(t *testing.T) {
	events := make(chan executor.ChatEvent, 1)
	events <- turnEndedEvent(12, 0, 0)
	close(events)

	rec := httptest.NewRecorder()
	nonStreamOpenAI(rec, "claude-test", events, simCacheDecision{}, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a completed response with upstream usage (body=%s)", rec.Code, rec.Body.String())
	}
}
