package cpaformat

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

// FromAccount folds a cursor-proto *auth.Account into the CPA on-disk
// shape. ChecksumSession is intentionally omitted because it is derived from
// the persisted checksum identity and session start time.
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
			Type:              ProviderType,
			AccessToken:       a.AccessToken,
			RefreshToken:      a.RefreshToken,
			Email:             a.Email,
			UserID:            a.UserID,
			AuthID:            a.AuthID,
			AuthKind:          a.AuthType,
			TeamID:            a.TeamID,
			PrivacyMode:       a.PrivacyMode,
			InternalUser:      a.InternalUser,
			MachineID:         a.MachineID,
			MacMachineID:      a.MacMachineID,
			ChecksumMachineID: a.ChecksumMachineID,
			SessionID:         a.SessionID,
			ConfigVersion:     a.ConfigVersion,
			ClientKey:         a.ClientKey,
			ClientOS:          a.ClientOS,
			ClientOSVersion:   a.ClientOSVersion,
			ClientArch:        a.ClientArch,
			ClientType:        a.ClientType,
			ClientLayout:      a.ClientLayout,
			ClientShell:       a.ClientShell,
			WorkspacePath:     a.WorkspacePath,
			IssuedAt:          FormatTime(a.IssuedAt),
			LastRefresh:       FormatTime(a.IssuedAt),
			Expired:           FormatTime(a.ExpiresAt),
			Refreshable:       a.Refreshable,
			RefreshLeadNanos:  int64(a.RefreshLead),
		},
		ProxyURL: a.ProxyURL,
	}
	return out, nil
}

// ToAccount rebuilds a cursor-proto *auth.Account from the on-disk auth
// file. ChecksumSession remains blank so Account.FillSessionDefaults can
// derive it from the persisted checksum identity.
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
		Email:             a.Email,
		UserID:            a.UserID,
		AccessToken:       a.AccessToken,
		RefreshToken:      a.RefreshToken,
		AuthID:            a.AuthID,
		AuthType:          a.AuthKind,
		TeamID:            a.TeamID,
		PrivacyMode:       a.PrivacyMode,
		InternalUser:      a.InternalUser,
		IssuedAt:          issued,
		ExpiresAt:         expires,
		MachineID:         a.MachineID,
		MacMachineID:      a.MacMachineID,
		ChecksumMachineID: a.ChecksumMachineID,
		SessionID:         a.SessionID,
		ConfigVersion:     a.ConfigVersion,
		ClientKey:         a.ClientKey,
		ClientOS:          a.ClientOS,
		ClientOSVersion:   a.ClientOSVersion,
		ClientArch:        a.ClientArch,
		ClientType:        a.ClientType,
		ClientLayout:      a.ClientLayout,
		ClientShell:       a.ClientShell,
		WorkspacePath:     a.WorkspacePath,
		ProxyURL:          a.ProxyURL,
		Refreshable:       a.Refreshable,
		RefreshLead:       time.Duration(a.RefreshLeadNanos),
	}
	return acc, nil
}
