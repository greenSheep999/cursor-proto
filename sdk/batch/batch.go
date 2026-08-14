// Package batch provides shared helpers for the batch account-management
// CLIs (cursor-login-batch, cursor-batch-import, cursor-pool). These
// helpers are intentionally standalone — no importer of executor/ or
// usage/ so callers can pull in only what they need.
package batch

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// InputRow is the neutral view of one row from a CSV / JSON import file.
// All fields are optional except Email + AccessToken.
type InputRow struct {
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
}

// ReadRows dispatches to CSV or JSON parsing based on the file extension.
// Empty rows are dropped; rows without an email or access token are kept
// so the caller can surface the error for that specific row.
func ReadRows(path string) ([]InputRow, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return readCSV(path)
	case ".json":
		return readJSON(path)
	default:
		return nil, fmt.Errorf("unsupported file extension %q (want .csv or .json)", filepath.Ext(path))
	}
}

func readJSON(path string) ([]InputRow, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []InputRow
	if err := json.Unmarshal(buf, &rows); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	return rows, nil
}

func readCSV(path string) ([]InputRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(bufio.NewReader(f))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1

	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(records) < 1 {
		return nil, nil
	}
	header := records[0]
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	col := func(rec []string, keys ...string) string {
		for _, k := range keys {
			if i, ok := idx[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
		}
		return ""
	}

	var out []InputRow
	for i := 1; i < len(records); i++ {
		rec := records[i]
		if allEmpty(rec) {
			continue
		}
		row := InputRow{
			Email:        col(rec, "email", "e-mail"),
			AccessToken:  col(rec, "access_token", "accesstoken", "token"),
			RefreshToken: col(rec, "refresh_token", "refreshtoken"),
			UserID:       col(rec, "user_id", "userid"),
			AuthID:       col(rec, "auth_id", "authid"),
			AuthType:     col(rec, "auth_type", "authtype", "auth_kind"),
		}
		out = append(out, row)
	}
	return out, nil
}

func allEmpty(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// ReadEmailsFile reads a plain-text file with one email per line. Lines
// starting with "#" and blank lines are ignored. Whitespace around each
// email is trimmed.
func ReadEmailsFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var out []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AccountFromRow turns an InputRow into a cursor-proto Account, filling
// in machine identifiers from the local machine and deriving issued_at
// from the JWT payload when available.
func AccountFromRow(row InputRow, machineID, macMachineID string) (*auth.Account, error) {
	if strings.TrimSpace(row.Email) == "" {
		return nil, fmt.Errorf("email is required")
	}
	if strings.TrimSpace(row.AccessToken) == "" {
		return nil, fmt.Errorf("access_token is required for %s", row.Email)
	}
	if tokenType := auth.TokenType(row.AccessToken); tokenType == "web" {
		return nil, fmt.Errorf("access_token for %s is a Cursor website token (type=web), not an IDE session token; use cursor-login -credential-file with a normal browser cookie environment", row.Email)
	}
	issued := auth.IssuedAtFromJWT(row.AccessToken)
	if issued.IsZero() {
		issued = nowUTC()
	}
	expires := auth.ExpiresAtFromJWT(row.AccessToken)

	userID := strings.TrimSpace(row.UserID)
	if userID == "" && row.AuthID != "" {
		if i := strings.Index(row.AuthID, "|"); i >= 0 {
			userID = row.AuthID[i+1:]
		}
	}

	return &auth.Account{
		Email:        strings.TrimSpace(row.Email),
		UserID:       userID,
		AccessToken:  strings.TrimSpace(row.AccessToken),
		RefreshToken: strings.TrimSpace(row.RefreshToken),
		AuthID:       strings.TrimSpace(row.AuthID),
		AuthType:     strings.TrimSpace(row.AuthType),
		IssuedAt:     issued,
		ExpiresAt:    expires,
		MachineID:    machineID,
		MacMachineID: macMachineID,
		Refreshable:  strings.TrimSpace(row.RefreshToken) != "",
		RefreshLead:  defaultRefreshLead,
	}, nil
}

// SanitizedEmail returns the lower-case, filesystem-safe form of an email,
// matching the same rules used by auth.AccountFilePath and cpaformat.
func SanitizedEmail(email string) string {
	return cpaformat.SanitizeEmail(email)
}
