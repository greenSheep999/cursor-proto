package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestConvertAnthropicToolsMapsNativeWebTools(t *testing.T) {
	tools, webSearch, webFetch, unsupported := convertAnthropicTools([]anthropicTool{
		{Type: "web_search_20250305", Name: "web_search"},
		{Type: "web_fetch_20250910", Name: "web_fetch"},
		{Name: "bash", Description: "run a command"},
	})

	if !webSearch || !webFetch || unsupported != "" {
		t.Fatalf("webSearch=%v webFetch=%v unsupported=%q", webSearch, webFetch, unsupported)
	}
	if len(tools) != 1 || tools[0].Name != "bash" {
		t.Fatalf("custom tools = %+v", tools)
	}
}

func TestConvertAnthropicToolsRejectsUnsupportedServerTool(t *testing.T) {
	_, webSearch, webFetch, unsupported := convertAnthropicTools([]anthropicTool{{Type: "computer_20241022"}})
	if webSearch || webFetch || unsupported != "computer_20241022" {
		t.Fatalf("webSearch=%v webFetch=%v unsupported=%q", webSearch, webFetch, unsupported)
	}
}

func TestNonStreamAnthropicStopsAfterCompletedTurnOnOpenWebToolStream(t *testing.T) {
	events := make(chan executor.ChatEvent, 2)
	events <- assistantBlobEvent("WEBSEARCH_OK")
	events <- turnEndedEvent(10, 2, 0)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		nonStreamAnthropic(recorder, "claude-test", events, simCacheDecision{}, nil, false)
		done <- recorder
	}()

	select {
	case recorder := <-done:
		if !strings.Contains(recorder.Body.String(), "WEBSEARCH_OK") {
			t.Fatalf("body = %s", recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("non-streaming response waited for the upstream SSE channel to close after a completed turn")
	}
}
