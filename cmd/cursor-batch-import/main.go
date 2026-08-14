// cursor-batch-import ingests a CSV or JSON file of existing Cursor
// tokens and writes one cursor-proto Account file per row.
//
// Usage:
//
//	cursor-batch-import --csv tokens.csv --out ~/.cursor-pool/
//	cursor-batch-import --csv tokens.json --out ~/.cursor-pool/  # JSON works too
//	cursor-batch-import --csv tokens.csv --skip-validate         # trust rows blindly
//
// CSV shape (header row required, columns can be in any order):
//
//	email,access_token,refresh_token
//	alice@icloud.com,eyJ...,eyJ...
//	bob@gmail.com,eyJ...,
//
// JSON shape: array of objects with the same field names.
//
// Behaviour:
//   - Rows without an access_token are logged and skipped.
//   - Website session JWTs (type=web) are rejected before validation; they
//     must be converted through cursor-login's cookie-backed PKCE flow.
//   - Each row is validated against Cursor's DashboardService.GetMe. On
//     failure with a refresh_token present, one retry is attempted; if
//     that still fails the row is logged and skipped.
//   - Rows without a refresh_token are saved with refreshable=false and
//     bypass the validation step (since there's no way to recover on
//     expiry).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/batch"
	"github.com/router-for-me/cursor-proto/usage"
)

func main() {
	inputPath := flag.String("csv", "", "path to input file (.csv or .json)")
	inputPathAlt := flag.String("input", "", "alias for --csv (accepts .csv or .json)")
	outDir := flag.String("out", defaultOutDir(), "directory to write account JSON files into")
	force := flag.Bool("force", false, "overwrite existing account files")
	skipValidate := flag.Bool("skip-validate", false, "do not call GetMe to validate each token")
	timeout := flag.Duration("timeout", 20*time.Second, "per-account validation timeout")
	flag.Parse()

	src := strings.TrimSpace(*inputPath)
	if src == "" {
		src = strings.TrimSpace(*inputPathAlt)
	}
	if src == "" {
		fatalf("--csv (or --input) is required; supports .csv and .json")
	}
	rows, err := batch.ReadRows(src)
	if err != nil {
		fatalf("read %s: %v", src, err)
	}
	if len(rows) == 0 {
		fatalf("no rows in %s", src)
	}

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		fatalf("create out dir: %v", err)
	}

	// Pin machine IDs once — the imported batch is authored on this host.
	machineID, _ := auth.GetMachineID()
	macMachineID, _ := auth.GetMacMachineID()

	var (
		saved   []string
		skipped []string
		failed  []string
	)

	for i, row := range rows {
		email := strings.TrimSpace(row.Email)
		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s\n", i+1, len(rows), displayEmail(email, i))
		if email == "" {
			fmt.Fprintln(os.Stderr, "  skip: email is empty")
			skipped = append(skipped, fmt.Sprintf("row_%d(no_email)", i+1))
			continue
		}
		if strings.TrimSpace(row.AccessToken) == "" {
			fmt.Fprintln(os.Stderr, "  skip: access_token is empty")
			skipped = append(skipped, email+"(no_access)")
			continue
		}
		outPath := auth.AccountFilePath(*outDir, email)
		if !*force {
			if _, err := os.Stat(outPath); err == nil {
				fmt.Fprintf(os.Stderr, "  skip: %s already exists (use --force to overwrite)\n", outPath)
				skipped = append(skipped, email+"(exists)")
				continue
			}
		}

		acc, err := batch.AccountFromRow(row, machineID, macMachineID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  fail: %v\n", err)
			failed = append(failed, email)
			continue
		}
		acc.FillSessionDefaults(time.Now())

		if !*skipValidate && acc.Refreshable {
			if err := validateWithRetry(context.Background(), acc, *timeout); err != nil {
				fmt.Fprintf(os.Stderr, "  fail: validation: %v\n", err)
				failed = append(failed, email)
				continue
			}
			fmt.Fprintln(os.Stderr, "  validate: ok")
		} else if !acc.Refreshable {
			fmt.Fprintln(os.Stderr, "  note: no refresh_token; saving with refreshable=false")
		}

		path, err := auth.SaveAccount(*outDir, acc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  fail: save: %v\n", err)
			failed = append(failed, email)
			continue
		}
		fmt.Fprintf(os.Stderr, "  ok: %s\n", path)
		saved = append(saved, email)
	}

	summarise(len(rows), saved, skipped, failed)
	if len(failed) > 0 {
		os.Exit(1)
	}
}

// validateWithRetry hits DashboardService.GetMe once; on failure it
// retries a single time. A refresh endpoint is not yet wired up in
// cursor-proto (plugin/cursor's auth.refresh handler is a passthrough),
// so the retry is best-effort transport recovery rather than a token
// swap. When a real refresh flow lands, this function is the seam to
// call it.
func validateWithRetry(ctx context.Context, acc *auth.Account, timeout time.Duration) error {
	if err := validateOnce(ctx, acc, timeout); err == nil {
		return nil
	} else if acc.RefreshToken == "" {
		return err
	} else {
		// One retry.
		if err2 := validateOnce(ctx, acc, timeout); err2 == nil {
			return nil
		} else {
			return fmt.Errorf("first: %v; retry: %v", err, err2)
		}
	}
}

func validateOnce(ctx context.Context, acc *auth.Account, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := usage.New(executor.NewClient(acc))
	snap, err := client.Fetch(cctx)
	if err != nil {
		return err
	}
	// Fetch returns a non-nil snap with per-RPC errors mapped; we consider
	// validation successful if at least the "me" or "current period" RPC
	// worked. Both would fail together for an invalid bearer token.
	if snap == nil {
		return fmt.Errorf("nil snapshot")
	}
	if snap.Fetched.Me || snap.Fetched.CurrentPeriodUsage {
		if snap.Email != "" && !strings.EqualFold(snap.Email, acc.Email) {
			fmt.Fprintf(os.Stderr, "  note: token belongs to %s (input said %s)\n", snap.Email, acc.Email)
		}
		return nil
	}
	if reason, ok := snap.Errors["me"]; ok {
		return fmt.Errorf("GetMe failed: %s", reason)
	}
	if reason, ok := snap.Errors["current_period_usage"]; ok {
		return fmt.Errorf("GetCurrentPeriodUsage failed: %s", reason)
	}
	return fmt.Errorf("no dashboard RPC succeeded")
}

func summarise(total int, saved, skipped, failed []string) {
	fmt.Fprintf(os.Stderr, "\n=== summary ===\n")
	fmt.Fprintf(os.Stderr, "  %d/%d saved, %d skipped, %d failed\n",
		len(saved), total, len(skipped), len(failed))
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "  failed: %s\n", strings.Join(failed, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "  skipped: %s\n", strings.Join(skipped, ", "))
	}
}

func displayEmail(e string, i int) string {
	if e == "" {
		return fmt.Sprintf("row_%d", i+1)
	}
	return e
}

func defaultOutDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cursor-pool")
	}
	return "."
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cursor-batch-import: "+format+"\n", args...)
	os.Exit(2)
}
