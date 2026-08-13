package cpaformat

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

// FromAccount folds a cursor-proto *auth.Account into the CPA on-disk
// shape. Fields that are not persisted (SessionID, ConfigVersion,
// ClientKey, ChecksumSession) are intentionally dropped: the plugin
// regenerates them at load time.
//
// The caller can set the optional operator knobs (Prefix, ProxyURL,
// Priority, Note, Disabled, ExcludedModels, DisableCooling,
// RequestRetry) on the returned AuthFile before writing it to disk.
func FromAccount(a *auth.Account) (*AuthFile, error) {
	if a == nil {
		return nil, fmt.Errorf("nil account")
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("account has empty access_token")
	}

	out := &AuthFile{
		CursorTokenStorage: CursorTokenStorage{
			Type:             ProviderType,
			AccessToken:      a.AccessToken,
			RefreshToken:     a.RefreshToken,
			Email:            a.Email,
			UserID:           a.UserID,
			AuthID:           a.AuthID,
			AuthKind:         a.AuthType,
			MachineID:        a.MachineID,
			MacMachineID:     a.MacMachineID,
			IssuedAt:         FormatTime(a.IssuedAt),
			LastRefresh:      FormatTime(a.IssuedAt),
			Expired:          FormatTime(a.ExpiresAt),
			Refreshable:      a.Refreshable,
			RefreshLeadNanos: int64(a.RefreshLead),
		},
		ProxyURL: a.ProxyURL,
	}
	return out, nil
}

// ToAccount rebuilds a cursor-proto *auth.Account from the on-disk auth
// file. Session-scoped fields (SessionID, ConfigVersion, ClientKey,
// ChecksumSession) are left blank so callers can regenerate them via
// Account.FillSessionDefaults after loading.
func (a *AuthFile) ToAccount() (*auth.Account, error) {
	if a == nil {
		return nil, fmt.Errorf("nil auth file")
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	issued, errIssued := ParseTime(a.IssuedAt)
	if errIssued != nil {
		return nil, fmt.Errorf("parse issued_at: %w", errIssued)
	}
	expires, errExpires := ParseTime(a.Expired)
	if errExpires != nil {
		return nil, fmt.Errorf("parse expired: %w", errExpires)
	}
	acc := &auth.Account{
		Email:        a.Email,
		UserID:       a.UserID,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		AuthID:       a.AuthID,
		AuthType:     a.AuthKind,
		IssuedAt:     issued,
		ExpiresAt:    expires,
		MachineID:    a.MachineID,
		MacMachineID: a.MacMachineID,
		ProxyURL:     a.ProxyURL,
		Refreshable:  a.Refreshable,
		RefreshLead:  time.Duration(a.RefreshLeadNanos),
	}
	return acc, nil
}
