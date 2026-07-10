package executor

import (
	"strings"
	"testing"
)

// Real trailer captured from /tmp/claude-raw.bin when Cursor's server refused
// the request for a region-gated model on the CN Pro account. Reduced to a
// literal so unit tests can lock the parser without hitting the network.
const regionGateTrailer = "grpc-message: Error\r\n" +
	"grpc-status: 8\r\n" +
	"grpc-status-details-bin: CAgSBUVycm9yGt0BCix0eXBlLmdvb2dsZWFwaXMuY29tL2Fpc2VydmVyLnYxLkVycm9yRGV0YWlscxKsAQhAEqUBChNNb2RlbCBub3QgYXZhaWxhYmxlEnhUaGlzIG1vZGVsIHByb3ZpZGVyIGlzIG5vdCBzdXBwb3J0ZWQgaW4geW91ciByZWdpb24uIFZpc2l0IGh0dHBzOi8vY3Vyc29yLmNvbS9kb2NzL2FjY291bnQvcmVnaW9ucyBmb3IgbW9yZSBpbmZvcm1hdGlvbi4YASAAKABSDgoMY2hhbmdlX21vZGVsGAE=\r\n"

func TestParseTrailer_RegionGate(t *testing.T) {
	got := ParseTrailer([]byte(regionGateTrailer))
	if got == nil {
		t.Fatal("ParseTrailer returned nil")
	}
	if got.Code != 8 {
		t.Fatalf("Code = %d, want 8", got.Code)
	}
	if got.Message != "Error" {
		t.Fatalf("Message = %q, want %q", got.Message, "Error")
	}
	if got.Detail == nil {
		t.Fatal("Detail = nil; expected decoded aiserver.v1.ErrorDetails")
	}
	inner := got.Detail.GetDetails()
	if inner == nil {
		t.Fatal("Detail.Details = nil")
	}
	if inner.GetTitle() != "Model not available" {
		t.Fatalf("Title = %q, want %q", inner.GetTitle(), "Model not available")
	}
	if !strings.Contains(inner.GetDetail(), "not supported in your region") {
		t.Fatalf("Detail = %q, expected region-gate text", inner.GetDetail())
	}
	if err := got.Err(); err == nil || !strings.Contains(err.Error(), "Model not available") {
		t.Fatalf("Err = %v, want non-nil containing 'Model not available'", err)
	}
}

func TestParseTrailer_EmptyIsNil(t *testing.T) {
	if got := ParseTrailer(nil); got != nil {
		t.Fatalf("ParseTrailer(nil) = %#v, want nil", got)
	}
}

func TestParseTrailer_OK(t *testing.T) {
	got := ParseTrailer([]byte("grpc-status: 0\r\n"))
	if got == nil {
		t.Fatal("got nil")
	}
	if !got.OK() {
		t.Fatalf("OK() = false for code 0")
	}
	if got.Err() != nil {
		t.Fatalf("Err() = %v, want nil for OK", got.Err())
	}
}
