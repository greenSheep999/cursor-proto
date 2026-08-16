// cursor-to-cpa converts a cursor-proto account JSON (as written by
// cmd/cursor-login) into the JSON shape CLIProxyAPI (CPA) expects
// under its auths/ directory.
//
// Usage:
//
//	cursor-to-cpa --in ./account.json
//	cursor-to-cpa --in ./account.json --out /path/to/cpa/cursor-foo.json
//	cursor-to-cpa --in ./account.json --dir ~/.cli-proxy-api
//	cursor-to-cpa --dir-in ~/.cursor-pool/ --dir ~/.cpa-pool/   # batch mode
//
// Behaviour:
//   - When --out is set, the output is written verbatim to that path.
//   - When --dir is set, the file is written to <dir>/cursor-<sanitized_email>.json.
//   - When --dir-in is set, every *.json under it is converted into
//     <dir>/cursor-<sanitized_email>.json. Extra flags apply to every
//     converted file.
//   - When none of --out/--dir/--dir-in is set, the file is written to
//     ~/.cli-proxy-api/cursor-<sanitized_email>.json (CPA's default
//     auth directory).
//   - Extra operator knobs (prefix, priority, note, proxy_url,
//     excluded_models, disable_cooling, request_retry, disabled) may be
//     supplied via matching flags and are recorded in the output file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// commaList is a repeatable string-slice flag whose value is a
// comma-separated list on the command line.
type commaList []string

func (c *commaList) String() string { return strings.Join(*c, ",") }
func (c *commaList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*c = append(*c, p)
		}
	}
	return nil
}

func main() {
	inPath := flag.String("in", "", "path to a single cursor-proto account JSON")
	inDir := flag.String("dir-in", "", "batch mode: directory of cursor-proto account JSON files")
	outPath := flag.String("out", "", "explicit output file path (single-file mode only)")
	outDir := flag.String("dir", "", "CPA auths directory (default: $CLIPROXY_AUTH_DIR or ~/.cli-proxy-api)")
	prefix := flag.String("prefix", "", "model routing prefix (applied to every converted account)")
	proxyURL := flag.String("proxy-url", "", "per-account proxy URL (applied to every converted account)")
	priority := flag.Int("priority", 0, "scheduler priority hint (higher wins)")
	note := flag.String("note", "", "human-readable note for the operator")
	disabled := flag.Bool("disabled", false, "mark the account(s) disabled")
	disableCooling := flag.Bool("disable-cooling", false, "opt out of provider-wide cooldowns")
	requestRetry := flag.Int("request-retry", 0, "per-request retry override")
	var excluded commaList
	flag.Var(&excluded, "excluded-models", "comma-separated list of models to block")
	dryRun := flag.Bool("dry-run", false, "print resulting JSON to stdout instead of writing to disk (single-file mode)")
	stdout := flag.Bool("stdout", false, "print resulting JSON to stdout AFTER writing to disk (single-file mode)")
	flag.Parse()

	overrides := knobs{
		prefix:         strings.TrimSpace(*prefix),
		proxyURL:       strings.TrimSpace(*proxyURL),
		priority:       *priority,
		note:           strings.TrimSpace(*note),
		disabled:       *disabled,
		disableCooling: *disableCooling,
		requestRetry:   *requestRetry,
		excluded:       excluded,
	}

	if strings.TrimSpace(*inDir) != "" {
		if strings.TrimSpace(*inPath) != "" || *dryRun || *stdout {
			fatalf("--dir-in cannot be combined with --in / --dry-run / --stdout")
		}
		runBatch(*inDir, *outDir, overrides)
		return
	}

	if strings.TrimSpace(*inPath) == "" {
		fmt.Fprintln(os.Stderr, "error: --in or --dir-in is required")
		flag.Usage()
		os.Exit(2)
	}

	acc, err := auth.LoadAccount(*inPath)
	if err != nil {
		fatalf("load account: %v", err)
	}

	out, err := cpaformat.FromAccount(acc)
	if err != nil {
		fatalf("convert account: %v", err)
	}
	overrides.apply(out)

	buf, err := out.Marshal()
	if err != nil {
		fatalf("marshal: %v", err)
	}

	if *dryRun {
		writeStdout(buf)
		return
	}

	path, err := resolveOutputPath(*outPath, *outDir, out)
	if err != nil {
		fatalf("resolve output path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fatalf("create dir: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		fatalf("write file: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, len(buf))
	if *stdout {
		writeStdout(buf)
	}
}

// knobs bundles operator-visible CPA fields set by CLI flags.
type knobs struct {
	prefix         string
	proxyURL       string
	priority       int
	note           string
	disabled       bool
	disableCooling bool
	requestRetry   int
	excluded       commaList
}

func (k knobs) apply(out *cpaformat.AuthFile) {
	out.Prefix = k.prefix
	if k.proxyURL != "" {
		out.ProxyURL = k.proxyURL
	}
	out.Priority = k.priority
	out.Note = k.note
	out.Disabled = k.disabled
	out.DisableCooling = k.disableCooling
	out.RequestRetry = k.requestRetry
	if len(k.excluded) > 0 {
		out.ExcludedModels = append(out.ExcludedModels, k.excluded...)
	}
}

// runBatch iterates every *.json in inDir, converts it, and writes the
// result under outDir. Failures are logged and do not abort the run.
func runBatch(inDir, outDir string, ov knobs) {
	entries, err := os.ReadDir(inDir)
	if err != nil {
		fatalf("read dir: %v", err)
	}
	dir, err := resolveOutDir(outDir)
	if err != nil {
		fatalf("resolve out dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatalf("create out dir: %v", err)
	}

	var (
		saved  []string
		failed []string
	)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		in := filepath.Join(inDir, e.Name())
		acc, err := auth.LoadAccount(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", in, err)
			failed = append(failed, e.Name())
			continue
		}
		out, err := cpaformat.FromAccount(acc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", in, err)
			failed = append(failed, e.Name())
			continue
		}
		ov.apply(out)
		path, err := out.WriteToDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", in, err)
			failed = append(failed, e.Name())
			continue
		}
		fmt.Fprintf(os.Stderr, "ok:   %s -> %s\n", in, path)
		saved = append(saved, e.Name())
	}
	fmt.Fprintf(os.Stderr, "\n=== summary ===\n  %d converted, %d failed\n",
		len(saved), len(failed))
	if len(failed) > 0 {
		os.Exit(1)
	}
}

func resolveOutDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir != "" {
		return dir, nil
	}
	if v := os.Getenv(cpaformat.DefaultAuthDirEnv); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home dir: %w", err)
	}
	return filepath.Join(home, ".cli-proxy-api"), nil
}

func resolveOutputPath(outPath, outDir string, af *cpaformat.AuthFile) (string, error) {
	outPath = strings.TrimSpace(outPath)
	if outPath != "" {
		return outPath, nil
	}
	dir, err := resolveOutDir(outDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, af.FileName()), nil
}

func writeStdout(buf []byte) {
	// Emit the JSON followed by a newline so the tool composes well
	// with pipelines. We use encoding/json here so an invalid buffer
	// would surface as an error rather than silently propagating.
	var probe json.RawMessage
	if err := json.Unmarshal(buf, &probe); err != nil {
		fatalf("internal: produced invalid JSON: %v", err)
	}
	if _, err := io.Copy(os.Stdout, strings.NewReader(string(buf))); err != nil {
		fatalf("write stdout: %v", err)
	}
	fmt.Println()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cursor-to-cpa: "+format+"\n", args...)
	os.Exit(1)
}
