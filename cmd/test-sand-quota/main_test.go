package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
)

func TestIDECompatibilityRequiresDeclaredGzipForCompressedConnectDataFrame(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(nil); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	body := connectFrameForTest(0x01, compressed.Bytes())
	body = append(body, connectFrameForTest(0x02, []byte(`{}`))...)
	result, err := decodeBoxRelayFrames(body)
	if err != nil {
		t.Fatal(err)
	}
	if result.CompressedFrames != 1 || result.DataFrames != 1 || !result.EndFrame {
		t.Fatalf("unexpected result: %+v", result)
	}
	if err := validateIDECompatible(result); err == nil || !strings.Contains(err.Error(), "without connect-content-encoding: gzip") {
		t.Fatalf("IDE compatibility error = %v", err)
	}
	result.ConnectContentEncoding = "gzip"
	if err := validateIDECompatible(result); err != nil {
		t.Fatalf("declared gzip response rejected: %v", err)
	}
}

func TestIDECompatibilityAcceptsIdentityConnectDataFrame(t *testing.T) {
	body := connectFrameForTest(0x00, nil)
	body = append(body, connectFrameForTest(0x02, []byte(`{}`))...)
	result, err := decodeBoxRelayFrames(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateIDECompatible(result); err != nil {
		t.Fatal(err)
	}
}

func TestBuildConnectRequestFrameEmulatesCursorIDECompression(t *testing.T) {
	payload := []byte("cursor-ide-request")
	frame, contentEncoding, err := buildConnectRequestFrame(payload, true)
	if err != nil {
		t.Fatal(err)
	}
	if contentEncoding != "gzip" {
		t.Fatalf("content encoding = %q, want gzip", contentEncoding)
	}
	if len(frame) < 5 || frame[0]&0x01 == 0 {
		t.Fatalf("frame flags = %#x, want compressed bit", frame[0])
	}
	length := int(binary.BigEndian.Uint32(frame[1:5]))
	if length != len(frame)-5 {
		t.Fatalf("frame length = %d, payload bytes = %d", length, len(frame)-5)
	}
	gz, err := gzip.NewReader(bytes.NewReader(frame[5:]))
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(bytes.Buffer)
	if _, err := decoded.ReadFrom(gz); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Bytes(), payload) {
		t.Fatalf("decoded payload = %q, want %q", decoded.Bytes(), payload)
	}
}

func connectFrameForTest(flags byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func TestApplyBoxRelayHeadersUsesOfficialSandIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	acc := &auth.Account{
		AccessToken:     "cursor-access-token",
		ChecksumSession: "ide-checksum",
		ConfigVersion:   "ide-config",
		ClientCommit:    "ide-commit",
		InternalUser:    true,
	}
	config := &boxRelayConfig{Refresh: &boxRelayRefresh{MachineID: "service-machine-id"}}
	if err := applyBoxRelayHeaders(req, acc, config, executor.SandClientVersion, "service"); err != nil {
		t.Fatal(err)
	}

	if got := req.Header.Get("x-cursor-client-type"); got != "sand" {
		t.Fatalf("client type = %q, want sand", got)
	}
	if got := req.Header.Get("x-cursor-client-source"); got != "" {
		t.Fatalf("client source = %q, want empty", got)
	}
	if got := req.Header.Get("x-cursor-client-version"); got != executor.SandClientVersion {
		t.Fatalf("client version = %q, want %q", got, executor.SandClientVersion)
	}
	if got := req.Header.Get("x-sand-box-namespace"); got != "prod" {
		t.Fatalf("box namespace = %q, want prod", got)
	}
	if got := req.Header.Get("x-cursor-checksum"); !strings.HasSuffix(got, "service-machine-id") || got == "ide-checksum" {
		t.Fatalf("checksum does not use relay service machine id")
	}
	if got := req.Header.Get("x-cursor-config-version"); got != "" {
		t.Fatalf("config version = %q, want empty", got)
	}
	if got := req.Header.Get("x-cursor-client-commit"); got != "" {
		t.Fatalf("client commit = %q, want empty", got)
	}
}

func TestApplyBoxRelayHeadersCanUseIDEChecksumForComparison(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	acc := &auth.Account{AccessToken: "cursor-access-token", ChecksumSession: "ide-checksum"}
	if err := applyBoxRelayHeaders(req, acc, &boxRelayConfig{}, executor.SandClientVersion, "ide"); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-cursor-checksum"); got != "ide-checksum" {
		t.Fatalf("checksum = %q, want ide-checksum", got)
	}
}

func TestApplyBoxRelayHeadersRejectsMissingServiceMachineID(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	acc := &auth.Account{AccessToken: "cursor-access-token", ChecksumSession: "ide-checksum"}
	err = applyBoxRelayHeaders(req, acc, &boxRelayConfig{}, executor.SandClientVersion, "service")
	if err == nil || !strings.Contains(err.Error(), "refresh.machineId") {
		t.Fatalf("error = %v, want missing refresh.machineId", err)
	}
}
