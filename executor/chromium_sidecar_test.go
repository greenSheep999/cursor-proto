package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
)

func TestChromiumSidecarOption(t *testing.T) {
	option, err := ChromiumSidecarOption("http://127.0.0.1:18901/base/", "secret")
	if err != nil {
		t.Fatalf("option: %v", err)
	}
	client := NewClient(&auth.Account{ProxyURL: "http://proxy.example:8080"}, option)
	if client.API2 != "http://127.0.0.1:18901/base/api2" {
		t.Fatalf("API2 = %q", client.API2)
	}
	if client.API3 != client.API2 {
		t.Fatalf("API3 = %q, want current api2 route %q", client.API3, client.API2)
	}
	if client.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, sidecar loopback hop must bypass it", client.ProxyURL)
	}
	req, _ := http.NewRequest(http.MethodPost, client.API2+"/test", nil)
	client.applySidecarToken(req)
	if got := req.Header.Get(sidecarTokenHeader); got != "secret" {
		t.Fatalf("sidecar token = %q", got)
	}
}

func TestChromiumSidecarOptionRejectsNonLoopback(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1:18901",
		"http://sidecar.example:18901",
		"http://0.0.0.0:18901",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ChromiumSidecarOption(raw, ""); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
