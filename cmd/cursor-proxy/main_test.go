package main

import (
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor/transport"
)

func TestNewWireClientPreservesAccountProxyWithoutOverride(t *testing.T) {
	account := &auth.Account{AccessToken: "test-token", ProxyURL: "http://account-proxy.example:8080"}
	client := newWireClient(account, transport.Http1_1, "")
	if client.ProxyURL != account.ProxyURL {
		t.Fatalf("ProxyURL = %q, want account proxy %q", client.ProxyURL, account.ProxyURL)
	}
}

func TestNewWireClientUsesExplicitProxyOverride(t *testing.T) {
	account := &auth.Account{AccessToken: "test-token", ProxyURL: "http://account-proxy.example:8080"}
	client := newWireClient(account, transport.Http1_1, " http://override-proxy.example:8080 ")
	if client.ProxyURL != "http://override-proxy.example:8080" {
		t.Fatalf("ProxyURL = %q, want explicit override", client.ProxyURL)
	}
}
