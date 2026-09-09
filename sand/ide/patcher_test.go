package ide

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsUnknownOperation(t *testing.T) {
	_, err := Run(context.Background(), "rewrite-everything", "", false)
	if err == nil || !strings.Contains(err.Error(), "unsupported operation") {
		t.Fatalf("expected unsupported operation error, got %v", err)
	}
}

func TestEmbeddedPatchRulesArePresent(t *testing.T) {
	text := string(patchScript)
	for _, marker := range []string{
		`TOOL_VERSION = "1.5.0-grokbot-box-relay.8"`,
		`SUPPORTED_CURSOR_VERSION = "3.19.13"`,
		`SUPPORTED_CURSOR_VERSIONS = (SUPPORTED_CURSOR_VERSION, "3.19.19")`,
		"SAND_CLIENT_MODE_V1",
		"SAND_MANAGED_LOCAL_ROUTE_V1",
		"SAND_DIRECT_INFERENCE_STREAM_V1",
		"SAND_GROK_BOX_RELAY_AUTH_V1",
		"SAND_AGENT_HOST_MOVE_EXEC_V1",
		"SAND_MAX_TOKENS_V1",
		"SAND_SUBAGENT_RESUME_AGENT_MODE_V1",
		"SAND_SUBAGENT_COMPLETION_WAKE_V1",
		"/sand-stream-relay/aiserver.v1.InferenceService/Stream",
		"def provision_box_relay",
		"def _create_backup",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("embedded patch rules missing %q", marker)
		}
	}
}

func TestPatchCurrentDirectStreamRoundTrip(t *testing.T) {
	result := runPythonProbe(t, `
fixture = module.DIRECT_STREAM_ANCHOR + "const afterAnchor=1;"
patched, first = module.apply_patch_to_content(fixture)
repatched, second = module.apply_patch_to_content(patched)
restored, removed = module.remove_patch_from_content(repatched)
print(json.dumps({
    "first": first.direct_stream,
    "second": second.direct_stream,
    "markers": repatched.count(module.SAND_DIRECT_STREAM_MARKER),
    "before_tail": repatched.index(module.SAND_DIRECT_STREAM_MARKER) < repatched.index("afterAnchor"),
    "idempotent": repatched == patched,
    "restored": restored == fixture,
    "removed": removed.direct_stream,
}))
`)
	var got struct {
		First      int  `json:"first"`
		Second     int  `json:"second"`
		Markers    int  `json:"markers"`
		BeforeTail bool `json:"before_tail"`
		Idempotent bool `json:"idempotent"`
		Restored   bool `json:"restored"`
		Removed    int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.First != 1 || got.Second != 0 || got.Markers != 1 || !got.BeforeTail || !got.Idempotent {
		t.Fatalf("direct Stream injection result = %+v", got)
	}
	if !got.Restored || got.Removed != 1 {
		t.Fatalf("direct Stream uninstall did not restore fixture: %+v", got)
	}
}

func TestPatchCurrentBoxRelayAuthAtBothTransports(t *testing.T) {
	result := runPythonProbe(t, `
fixture = "|".join(module.GROK_RUNTIME_AUTH_ORIGINALS_V3113)
patched, first = module.apply_patch_to_content(fixture)
repatched, second = module.apply_patch_to_content(patched)
restored, removed = module.remove_patch_from_content(repatched)
print(json.dumps({
    "first": first.grok_runtime_auth,
    "second": second.grok_runtime_auth,
    "markers": repatched.count(module.SAND_GROK_RUNTIME_AUTH_MARKER),
    "relay_paths": repatched.count(module.BOX_RELAY_PATH),
    "refresh": "EnsureSandBox" in repatched,
    "v046": repatched.count("0.46.0"),
    "v044": repatched.count("0.44.0"),
    "client_source_set": 'set("x-cursor-client-source"' in repatched,
    "client_source_delete": 'delete("x-cursor-client-source")' in repatched,
    "connect_accept_delete": 'delete("connect-accept-encoding")' in repatched,
    "accept_delete": 'delete("accept-encoding")' in repatched,
    "idempotent": repatched == patched,
    "restored": restored == fixture,
    "removed": removed.grok_runtime_auth,
}))
`)
	var got struct {
		First               int  `json:"first"`
		Second              int  `json:"second"`
		Markers             int  `json:"markers"`
		RelayPaths          int  `json:"relay_paths"`
		Refresh             bool `json:"refresh"`
		V046                int  `json:"v046"`
		V044                int  `json:"v044"`
		ClientSourceSet     bool `json:"client_source_set"`
		ClientSourceDelete  bool `json:"client_source_delete"`
		ConnectAcceptDelete bool `json:"connect_accept_delete"`
		AcceptDelete        bool `json:"accept_delete"`
		Idempotent          bool `json:"idempotent"`
		Restored            bool `json:"restored"`
		Removed             int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.First != 2 || got.Second != 0 || got.Markers != 2 || got.RelayPaths != 2 || !got.Refresh || got.V046 != 4 || got.V044 != 0 || got.ClientSourceSet || !got.ClientSourceDelete || got.ConnectAcceptDelete || got.AcceptDelete || !got.Idempotent {
		t.Fatalf("Box relay auth injection result = %+v", got)
	}
	if !got.Restored || got.Removed != 2 {
		t.Fatalf("Box relay auth uninstall did not restore fixture: %+v", got)
	}
}

func TestConnectGzipFallbackRoundTrip(t *testing.T) {
	result := runPythonProbe(t, `
fixture = module.SAND_GROK_RUNTIME_AUTH_MARKER + "|".join(
    [module.CONNECT_GZIP_ENVELOPE_ORIGINAL]
)
patched, first = module.apply_patch_to_content(fixture)
repatched, second = module.apply_patch_to_content(patched)
unscoped_fixture = "|".join(
    [module.CONNECT_GZIP_ENVELOPE_ORIGINAL]
)
unscoped, unscoped_stats = module.apply_patch_to_content(unscoped_fixture)
restored, removed = module.remove_patch_from_content(repatched)
print(json.dumps({
    "first": first.connect_gzip_fallback,
    "second": second.connect_gzip_fallback,
    "markers": repatched.count(module.SAND_CONNECT_GZIP_FALLBACK_MARKER),
    "uses_node_gzip": 'require("node:zlib").gunzipSync' in repatched,
    "idempotent": repatched == patched,
    "bounded": 'maxOutputLength:n' in repatched,
    "unscoped_unchanged": unscoped == unscoped_fixture,
    "unscoped_count": unscoped_stats.connect_gzip_fallback,
    "restored": restored == fixture,
    "removed": removed.connect_gzip_fallback,
}))
`)
	var got struct {
		First             int  `json:"first"`
		Second            int  `json:"second"`
		Markers           int  `json:"markers"`
		UsesNodeGzip      bool `json:"uses_node_gzip"`
		Bounded           bool `json:"bounded"`
		Idempotent        bool `json:"idempotent"`
		UnscopedUnchanged bool `json:"unscoped_unchanged"`
		UnscopedCount     int  `json:"unscoped_count"`
		Restored          bool `json:"restored"`
		Removed           int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.First != 1 || got.Second != 0 || got.Markers != 1 || !got.UsesNodeGzip || !got.Bounded || !got.Idempotent || !got.UnscopedUnchanged || got.UnscopedCount != 0 {
		t.Fatalf("Connect gzip fallback result = %+v", got)
	}
	if !got.Restored || got.Removed != 1 {
		t.Fatalf("Connect gzip fallback uninstall result = %+v", got)
	}
}

func TestGrokRuntimeAuthReadyCountDeduplicatesAliases(t *testing.T) {
	result := runPythonProbe(t, `
fixture = "|".join(module.GROK_RUNTIME_AUTH_PATCHES_V3113)
print(json.dumps({
    "count": module._grok_runtime_auth_ready_count([fixture]),
    "expected": module.EXPECTED_GROK_RUNTIME_AUTH_SITES,
    "current_equals_v145": (
        module.GROK_RUNTIME_AUTH_PATCHES_V3113
        == module.GROK_RUNTIME_AUTH_PATCHES_V145
    ),
}))
`)
	var got struct {
		Count             int  `json:"count"`
		Expected          int  `json:"expected"`
		CurrentEqualsV145 bool `json:"current_equals_v145"`
	}
	decodeProbe(t, result, &got)
	if !got.CurrentEqualsV145 || got.Count != got.Expected || got.Count != 2 {
		t.Fatalf("Grok runtime auth ready count = %+v", got)
	}
}

func TestConnectGzipFallbackMigratesPreReleaseBytes(t *testing.T) {
	result := runPythonProbe(t, `
fixture = (
    module.SAND_GROK_RUNTIME_AUTH_MARKER
    + module.CONNECT_GZIP_FALLBACK_PATCHED
    + module.CONNECT_GZIP_METHOD_FALLBACK_PATCHED
    + module.CONNECT_GZIP_ENVELOPE_ORIGINAL
)
patched, stats = module.apply_patch_to_content(fixture)
restored, removed = module.remove_patch_from_content(patched)
expected = (
    module.SAND_GROK_RUNTIME_AUTH_MARKER
    + module.CONNECT_GZIP_FALLBACK_ORIGINAL
    + module.CONNECT_GZIP_METHOD_FALLBACK_ORIGINAL
    + module.CONNECT_GZIP_ENVELOPE_ORIGINAL
)
print(json.dumps({
    "migrated": stats.migrated_connect_gzip_fallback,
    "added": stats.connect_gzip_fallback,
    "markers": patched.count(module.SAND_CONNECT_GZIP_FALLBACK_MARKER),
    "old_gone": (
        module.CONNECT_GZIP_FALLBACK_PATCHED not in patched
        and module.CONNECT_GZIP_METHOD_FALLBACK_PATCHED not in patched
    ),
    "restored": restored == expected,
    "removed": removed.connect_gzip_fallback,
}))
`)
	var got struct {
		Migrated int  `json:"migrated"`
		Added    int  `json:"added"`
		Markers  int  `json:"markers"`
		OldGone  bool `json:"old_gone"`
		Restored bool `json:"restored"`
		Removed  int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.Migrated != 2 || got.Added != 1 || got.Markers != 1 || !got.OldGone {
		t.Fatalf("pre-release gzip fallback migration = %+v", got)
	}
	if !got.Restored || got.Removed != 1 {
		t.Fatalf("migrated gzip fallback uninstall = %+v", got)
	}
}

func TestPatchMigratesV147RelayBytes(t *testing.T) {
	result := runPythonProbe(t, `
fixture = "|".join(module.GROK_RUNTIME_AUTH_PATCHES_V147)
patched, changed = module.apply_patch_to_content(fixture)
restored, removed = module.remove_patch_from_content(patched)
expected_original = "|".join(module.GROK_RUNTIME_AUTH_ORIGINALS_V3113)
print(json.dumps({
    "changed": changed.grok_runtime_auth,
    "connect_accept_delete": patched.count('delete("connect-accept-encoding")'),
    "accept_delete": patched.count('delete("accept-encoding")'),
    "restored": restored == expected_original,
    "removed": removed.grok_runtime_auth,
}))
`)
	var got struct {
		Changed             int  `json:"changed"`
		ConnectAcceptDelete int  `json:"connect_accept_delete"`
		AcceptDelete        int  `json:"accept_delete"`
		Restored            bool `json:"restored"`
		Removed             int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.Changed != 2 || got.ConnectAcceptDelete != 0 || got.AcceptDelete != 0 {
		t.Fatalf("V147 migration result = %+v", got)
	}
	if !got.Restored || got.Removed != 2 {
		t.Fatalf("V147 uninstall result = %+v", got)
	}
}

func TestPatchMigratesV144RelayBytes(t *testing.T) {
	result := runPythonProbe(t, `
fixture = "|".join(module.GROK_RUNTIME_AUTH_PATCHES_V144)
patched, changed = module.apply_patch_to_content(fixture)
restored, removed = module.remove_patch_from_content(patched)
expected_original = "|".join(module.GROK_RUNTIME_AUTH_ORIGINALS_V3113)
print(json.dumps({
    "changed": changed.grok_runtime_auth,
    "v046": patched.count("0.46.0"),
    "v044": patched.count("0.44.0"),
    "client_source_set": 'set("x-cursor-client-source"' in patched,
    "client_source_delete": 'delete("x-cursor-client-source")' in patched,
    "restored": restored == expected_original,
    "removed": removed.grok_runtime_auth,
}))
`)
	var got struct {
		Changed            int  `json:"changed"`
		V046               int  `json:"v046"`
		V044               int  `json:"v044"`
		ClientSourceSet    bool `json:"client_source_set"`
		ClientSourceDelete bool `json:"client_source_delete"`
		Restored           bool `json:"restored"`
		Removed            int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.Changed != 2 || got.V046 != 4 || got.V044 != 0 || got.ClientSourceSet || !got.ClientSourceDelete {
		t.Fatalf("V144 migration result = %+v", got)
	}
	if !got.Restored || got.Removed != 2 {
		t.Fatalf("V144 uninstall result = %+v", got)
	}
}

func TestProbeBoxRelayRequiresV8GzipInBothDirections(t *testing.T) {
	result := runPythonProbe(t, `
def frame(payload, flags=2):
    return bytes([flags]) + len(payload).to_bytes(4, "big") + payload

valid_status = json.dumps({
    "routeVersion": "v8",
    "upstreamClientVersion": "0.46.0",
    "configVersionMode": "strip",
    "requestCompression": "gzip-forwarded",
    "responseCompression": "gzip-declared",
}).encode()

def run(status_body, stream_body, stream_headers=None):
    observed = {}
    def fake(config, path, **kwargs):
        if path == module.BOX_RELAY_STATUS_PATH:
            return 200, {"Content-Type": "application/json"}, status_body
        observed.update(kwargs)
        headers = {"Content-Type": "application/connect+proto"}
        headers.update(stream_headers or {})
        return 200, headers, stream_body
    module._box_http_request = fake
    result = module._probe_box_relay({})
    request_body = observed.get("data", b"")
    request_headers = observed.get("headers", {})
    request_ok = (
        len(request_body) >= 5
        and request_body[0] & 1 == 1
        and module.gzip.decompress(request_body[5:]) == b""
        and request_headers.get("Connect-Content-Encoding") == "gzip"
    )
    return [*result, request_ok]

print(json.dumps({
    "valid": run(valid_status, frame(b"{}"), {"Connect-Content-Encoding": "gzip"}),
    "missing": run(valid_status, frame(b"{}")),
    "undeclared": run(valid_status, frame(b"gzip", 1) + frame(b"{}")),
    "stale": run(valid_status.replace(b'"v8"', b'"v7"'), frame(b"{}")),
    "outdated": run(valid_status, frame(b'{"error":"ERROR_OUTDATED_CLIENT","actionRequired":"config"}')),
    "dropped_request_encoding": run(valid_status, frame(b'{"error":{"code":"internal","message":"received compressed envelope, but do not know how to decompress"}}'), {"Connect-Content-Encoding": "gzip"}),
}))
`)
	var got struct {
		Valid                  []any `json:"valid"`
		Missing                []any `json:"missing"`
		Undeclared             []any `json:"undeclared"`
		Stale                  []any `json:"stale"`
		Outdated               []any `json:"outdated"`
		DroppedRequestEncoding []any `json:"dropped_request_encoding"`
	}
	decodeProbe(t, result, &got)
	if len(got.Valid) != 5 || got.Valid[2] != true || got.Valid[4] != true {
		t.Fatalf("valid relay probe = %#v", got.Valid)
	}
	if len(got.Missing) != 5 || got.Missing[2] != false || !strings.Contains(got.Missing[3].(string), "missing connect-content-encoding: gzip") {
		t.Fatalf("missing gzip header relay probe = %#v", got.Missing)
	}
	if len(got.Undeclared) != 5 || got.Undeclared[2] != false || !strings.Contains(got.Undeclared[3].(string), "missing connect-content-encoding: gzip") {
		t.Fatalf("undeclared compressed relay probe = %#v", got.Undeclared)
	}
	if len(got.Stale) != 5 || got.Stale[2] != false || !strings.Contains(got.Stale[3].(string), "stale relay contract") {
		t.Fatalf("stale relay probe = %#v", got.Stale)
	}
	if len(got.Outdated) != 5 || got.Outdated[2] != false || !strings.Contains(got.Outdated[3].(string), "outdated/config") {
		t.Fatalf("outdated relay probe = %#v", got.Outdated)
	}
	if len(got.DroppedRequestEncoding) != 5 || got.DroppedRequestEncoding[2] != false || !strings.Contains(got.DroppedRequestEncoding[3].(string), "dropped the request") || got.DroppedRequestEncoding[4] != true {
		t.Fatalf("dropped request encoding probe = %#v", got.DroppedRequestEncoding)
	}
}

func TestV8ProvisionPromptForwardsRequestGzipAndDeclaresResponseGzip(t *testing.T) {
	text := string(patchScript)
	for _, required := range []string{
		`BOX_RELAY_ROUTE_VERSION = "v8"`,
		`requestCompression=gzip-forwarded`,
		`responseCompression=gzip-declared`,
		`connect-content-encoding: gzip`,
		`connect-accept-encoding`,
		`Connect-Content-Encoding: gzip`,
		`zlib.gunzipSync`,
		`flags & 0x01`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("v8 gzip relay contract missing %q", required)
		}
	}
}

func TestPatchCurrentManagedLocalRouteIsIdempotent(t *testing.T) {
	result := runPythonProbe(t, `
fixture = module.MANAGED_LOCAL_ROUTE_ORIGINAL
patched, first = module.apply_patch_to_content(fixture)
repatched, second = module.apply_patch_to_content(patched)
restored, removed = module.remove_patch_from_content(repatched)
print(json.dumps({
    "first": first.managed_local_route,
    "second": second.managed_local_route,
    "markers": repatched.count(module.SAND_MANAGED_LOCAL_ROUTE_MARKER),
    "idempotent": repatched == patched,
    "restored": restored == fixture,
    "removed": removed.managed_local_route,
}))
`)
	var got struct {
		First      int  `json:"first"`
		Second     int  `json:"second"`
		Markers    int  `json:"markers"`
		Idempotent bool `json:"idempotent"`
		Restored   bool `json:"restored"`
		Removed    int  `json:"removed"`
	}
	decodeProbe(t, result, &got)
	if got.First != 1 || got.Second != 0 || got.Markers != 1 || !got.Idempotent {
		t.Fatalf("managed-local route patch is not idempotent: %+v", got)
	}
	if !got.Restored || got.Removed != 1 {
		t.Fatalf("managed-local route uninstall did not restore fixture: %+v", got)
	}
}

func TestParserExposesOperatorCommands(t *testing.T) {
	result := runPythonProbe(t, `
commands = ["status", "plan", "provision-box", "install", "setup-all", "uninstall"]
parsed = [module.build_parser().parse_args([command]).command for command in commands]
print(json.dumps(parsed))
`)
	var got []string
	decodeProbe(t, result, &got)
	want := []string{"status", "plan", "provision-box", "install", "setup-all", "uninstall"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestSupportedCursorVersionsAreExplicit(t *testing.T) {
	result := runPythonProbe(t, `
print(json.dumps({
    "v31913": module._cursor_version_supported("3.19.13"),
    "v31919": module._cursor_version_supported("3.19.19"),
    "v3197": module._cursor_version_supported("3.19.7"),
    "future": module._cursor_version_supported("3.19.20"),
}))
`)
	var got struct {
		V31913 bool `json:"v31913"`
		V31919 bool `json:"v31919"`
		V3197  bool `json:"v3197"`
		Future bool `json:"future"`
	}
	decodeProbe(t, result, &got)
	if !got.V31913 || !got.V31919 || got.V3197 || got.Future {
		t.Fatalf("supported versions = %+v", got)
	}
}

func TestExplicitCursorPathOverridesSavedConfig(t *testing.T) {
	result := runPythonProbe(t, `
import os
import tempfile
from pathlib import Path

explicit = Path(tempfile.mkdtemp()) / "Cursor.app"
saved = Path(tempfile.mkdtemp()) / "Cursor.app"
os.environ["SAND_CURSOR_INSTALL_DIR"] = str(explicit)
module._load_config = lambda: {"cursorInstallRoot": str(saved)}
module.layout_from_path = lambda value: str(value)
print(json.dumps({"resolved": module.resolve_cursor_layout(), "explicit": str(explicit)}))
`)
	var got struct {
		Resolved string `json:"resolved"`
		Explicit string `json:"explicit"`
	}
	decodeProbe(t, result, &got)
	if got.Resolved != got.Explicit {
		t.Fatalf("resolved path = %q, want explicit %q", got.Resolved, got.Explicit)
	}
}

func TestMacBundleDiscoveryAcceptsRenamedCursorApp(t *testing.T) {
	result := runPythonProbe(t, `
from pathlib import Path
root = Path("/private/tmp/Cursor Sand Test.app")
print(json.dumps({
    "bundle": str(module._find_app_bundle(root / "Contents" / "Resources" / "app")),
}))
`)
	var got struct {
		Bundle string `json:"bundle"`
	}
	decodeProbe(t, result, &got)
	if got.Bundle != "/private/tmp/Cursor Sand Test.app" {
		t.Fatalf("bundle = %q", got.Bundle)
	}
}

func TestAtomicJSONWriteUsesOwnerOnlyPermissions(t *testing.T) {
	result := runPythonProbe(t, `
import os
import tempfile
from pathlib import Path

path = Path(tempfile.mkdtemp()) / "relay.json"
module._write_json_atomic(path, {"token": "test-secret"})
print(json.dumps({
    "mode": oct(path.stat().st_mode & 0o777),
    "value": json.loads(path.read_text(encoding="utf-8")),
}))
`)
	var got struct {
		Mode  string         `json:"mode"`
		Value map[string]any `json:"value"`
	}
	decodeProbe(t, result, &got)
	if got.Mode != "0o600" {
		t.Fatalf("relay config mode = %s, want 0o600", got.Mode)
	}
	if got.Value["token"] != "test-secret" {
		t.Fatalf("relay config value = %#v", got.Value)
	}
}

func TestCommittedInstallBackupRestoresExactBytes(t *testing.T) {
	result := runPythonProbe(t, `
import tempfile
from pathlib import Path

root = Path(tempfile.mkdtemp()) / "Cursor.app"
target = root / "Contents" / "Resources" / "app" / "bundle.js"
target.parent.mkdir(parents=True)
original = b"original bundle bytes\n"
patched = b"patched bundle bytes\n"
target.write_bytes(patched)
backup = Path(tempfile.mkdtemp())
backup_file = backup / "files" / target.relative_to(root)
backup_file.parent.mkdir(parents=True)
backup_file.write_bytes(original)
manifest = {
    "files": [{
        "path": target.relative_to(root).as_posix(),
        "originalSha256": module._sha256(original),
        "nextSha256": module._sha256(patched),
        "mode": 0o644,
    }],
}
layout = module.CursorLayout(root, root, root / "product.json", root / "Cursor", (target,), None, module.SUPPORTED_CURSOR_VERSION)
plan = module._restore_committed_install_plan(layout, (backup, manifest))
restored = plan[target].next_bytes == original
target.write_bytes(b"externally changed")
rejected_external_change = False
try:
    module._restore_committed_install_plan(layout, (backup, manifest))
except module.SandToolError:
    rejected_external_change = True
print(json.dumps({
    "restored": restored,
    "rejected_external_change": rejected_external_change,
}))
`)
	var got struct {
		Restored               bool `json:"restored"`
		RejectedExternalChange bool `json:"rejected_external_change"`
	}
	decodeProbe(t, result, &got)
	if !got.Restored || !got.RejectedExternalChange {
		t.Fatalf("backup restore result = %+v", got)
	}
}

func TestRestartOutcomeMatchesSkipRestartMode(t *testing.T) {
	result := runPythonProbe(t, `
import os

os.environ["SAND_PATCH_SKIP_RESTART"] = "1"
skipped = module._restart_outcome("done")
os.environ["SAND_PATCH_SKIP_RESTART"] = ""
restarted = module._restart_outcome("done")
print(json.dumps({"skipped": skipped, "restarted": restarted}))
`)
	var got struct {
		Skipped   string `json:"skipped"`
		Restarted string `json:"restarted"`
	}
	decodeProbe(t, result, &got)
	if !strings.Contains(got.Skipped, "未自动重启") || !strings.Contains(got.Restarted, "已重新启动") {
		t.Fatalf("restart messages = %+v", got)
	}
}

func runPythonProbe(t *testing.T, body string) []byte {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	scriptPath := filepath.Join(t.TempDir(), "sand_patch.py")
	if err := os.WriteFile(scriptPath, patchScript, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := `
import importlib.util
import json
import sys

spec = importlib.util.spec_from_file_location("sand_patch", sys.argv[1])
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
` + body
	cmd := exec.Command(python, "-c", probe, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe embedded patcher: %v\n%s", err, out)
	}
	return out
}

func decodeProbe(t *testing.T, output []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(output, target); err != nil {
		t.Fatalf("decode probe output: %v\n%s", err, output)
	}
}
