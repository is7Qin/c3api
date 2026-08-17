// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/accountext"
)

// AccountExtRepo persists the account-level credentials used by Codex accounts.
type AccountExtRepo struct{ client *ent.Client }

// UpsertAccountExt replaces one account's extension row. It deliberately uses
// UPDATE-then-INSERT instead of SQL upsert: MySQL's ON DUPLICATE KEY UPDATE
// would treat a duplicate codex_account_id as a conflict on account_id and
// could overwrite a different account's credentials.
func (r *AccountExtRepo) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	normalizeCodexAccountID(e)
	n, err := r.replaceAccountExt(ctx, e)
	if err != nil {
		return nil, mapAccountExtConflict(err, e)
	}
	if n > 0 {
		return r.GetAccountExt(ctx, e.AccountID)
	}

	_, err = r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetNillableCodexAccountID(e.CodexAccountID).
		SetInstallationID(e.InstallationID).
		SetNillableSessionID(e.SessionID).
		SetNillableThreadID(e.ThreadID).
		SetNillableWindowID(e.WindowID).
		SetNillableOauthToken(e.OAuthToken).
		SetNillableOauthRefreshToken(e.OAuthRefreshToken).
		SetNillableOauthExpiresAt(e.OAuthExpiresAt).
		SetNillablePatKey(e.PATKey).
		SetNillableEmail(e.Email).
		Save(ctx)
	if err == nil {
		return r.GetAccountExt(ctx, e.AccountID)
	}
	if !sqlgraph.IsUniqueConstraintError(err) {
		return nil, err
	}

	// account_id can have appeared after the initial UPDATE. Retry the safe
	// targeted UPDATE only in that case; an external-account collision remains
	// a conflict and never updates the row that owns it.
	exists, existsErr := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(e.AccountID)).Exist(ctx)
	if existsErr != nil {
		return nil, existsErr
	}
	if !exists {
		return nil, mapAccountExtConflict(err, e)
	}
	n, err = r.replaceAccountExt(ctx, e)
	if err != nil {
		return nil, mapAccountExtConflict(err, e)
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: account_id=%d missing", ErrNotFound, e.AccountID)
	}
	return r.GetAccountExt(ctx, e.AccountID)
}

func (r *AccountExtRepo) replaceAccountExt(ctx context.Context, e *domain.AccountExt) (int, error) {
	u := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(e.AccountID)).
		SetCredentialType(string(e.CredentialType)).
		SetInstallationID(e.InstallationID)
	if e.CodexAccountID != nil {
		u.SetCodexAccountID(*e.CodexAccountID)
	} else {
		u.ClearCodexAccountID()
	}
	if e.SessionID != nil {
		u.SetSessionID(*e.SessionID)
	} else {
		u.ClearSessionID()
	}
	if e.ThreadID != nil {
		u.SetThreadID(*e.ThreadID)
	} else {
		u.ClearThreadID()
	}
	if e.WindowID != nil {
		u.SetWindowID(*e.WindowID)
	} else {
		u.ClearWindowID()
	}
	if e.OAuthToken != nil {
		u.SetOauthToken(*e.OAuthToken)
	} else {
		u.ClearOauthToken()
	}
	if e.OAuthRefreshToken != nil {
		u.SetOauthRefreshToken(*e.OAuthRefreshToken)
	} else {
		u.ClearOauthRefreshToken()
	}
	if e.OAuthExpiresAt != nil {
		u.SetOauthExpiresAt(*e.OAuthExpiresAt)
	} else {
		u.ClearOauthExpiresAt()
	}
	if e.PATKey != nil {
		u.SetPatKey(*e.PATKey)
	} else {
		u.ClearPatKey()
	}
	if e.Email != nil {
		u.SetEmail(*e.Email)
	} else {
		u.ClearEmail()
	}
	return u.Save(ctx)
}

func mapAccountExtConflict(err error, e *domain.AccountExt) error {
	if sqlgraph.IsUniqueConstraintError(err) {
		return fmt.Errorf("%w: codex_account_id=%q", ErrConflict, derefCodexAccountID(e))
	}
	return err
}

func derefCodexAccountID(e *domain.AccountExt) string {
	if e.CodexAccountID == nil {
		return ""
	}
	return *e.CodexAccountID
}

// TryInsertAccountExt inserts only when account_id has no extension row. A
// duplicate external Codex account id remains a conflict instead of becoming
// a false "already exists" result for a different internal account.
func (r *AccountExtRepo) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	normalizeCodexAccountID(e)
	_, err := r.client.AccountExt.Create().
		SetAccountID(e.AccountID).
		SetCredentialType(string(e.CredentialType)).
		SetNillableCodexAccountID(e.CodexAccountID).
		SetInstallationID(e.InstallationID).
		SetNillableSessionID(e.SessionID).
		SetNillableThreadID(e.ThreadID).
		SetNillableWindowID(e.WindowID).
		SetNillableOauthToken(e.OAuthToken).
		SetNillableOauthRefreshToken(e.OAuthRefreshToken).
		SetNillableOauthExpiresAt(e.OAuthExpiresAt).
		SetNillablePatKey(e.PATKey).
		SetNillableEmail(e.Email).
		Save(ctx)
	if err == nil {
		return true, nil
	}
	if !sqlgraph.IsUniqueConstraintError(err) {
		return false, err
	}
	exists, existsErr := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(e.AccountID)).Exist(ctx)
	if existsErr != nil {
		return false, existsErr
	}
	if exists {
		return false, nil
	}
	return false, mapAccountExtConflict(err, e)
}

func normalizeCodexAccountID(e *domain.AccountExt) {
	if e.CodexAccountID == nil {
		return
	}
	accountID := strings.TrimSpace(*e.CodexAccountID)
	if accountID == "" {
		e.CodexAccountID = nil
		return
	}
	e.CodexAccountID = &accountID
}

// WriteOAuthRotation updates only OAuth fields so an SDK refresh never clears
// the account identity, external account id, or administrative metadata.
func (r *AccountExtRepo) WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error {
	current, err := r.GetAccountExt(ctx, accountID)
	if err != nil {
		return err
	}
	if current.OAuthRefreshToken == nil || strings.TrimSpace(*current.OAuthRefreshToken) == "" {
		return fmt.Errorf("%w: account_id=%d refresh token missing", ErrNotFound, accountID)
	}
	updated, err := r.WriteOAuthRotationIfCurrent(ctx, accountID, *current.OAuthRefreshToken, at, rt, expiresAt)
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("%w: account_id=%d OAuth rotation superseded", ErrConflict, accountID)
	}
	return nil
}

// WriteOAuthRotationIfCurrent atomically persists a refresh response only
// while the stored refresh token remains the token that initiated it.
func (r *AccountExtRepo) WriteOAuthRotationIfCurrent(ctx context.Context, accountID int64, expectedRefreshToken, at, rt string, expiresAt *time.Time) (bool, error) {
	if accountID <= 0 || strings.TrimSpace(expectedRefreshToken) == "" || strings.TrimSpace(at) == "" || strings.TrimSpace(rt) == "" {
		return false, errors.New("repository: invalid OAuth rotation values")
	}
	u := r.client.AccountExt.Update().
		Where(accountext.AccountIDEQ(accountID), accountext.OauthRefreshTokenEQ(expectedRefreshToken)).
		SetOauthToken(at).
		SetOauthRefreshToken(rt)
	if expiresAt != nil {
		u.SetOauthExpiresAt(*expiresAt)
	} else {
		u.ClearOauthExpiresAt()
	}
	n, err := u.Save(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	exists, err := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Exist(ctx)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("%w: account_id=%d ext row missing (codex account must have account_ext)", ErrNotFound, accountID)
	}
	return false, nil
}

// GetAccountExt finds an extension by internal account id.
func (r *AccountExtRepo) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	row, err := r.client.AccountExt.Query().Where(accountext.AccountIDEQ(accountID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: account_id=%d missing", ErrNotFound, accountID)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}

// GetAccountExtByCodexAccountID finds the internal account owning an external
// Plus/Codex account id. NULL and blank ids never match.
func (r *AccountExtRepo) GetAccountExtByCodexAccountID(ctx context.Context, codexAccountID string) (*domain.AccountExt, error) {
	codexAccountID = strings.TrimSpace(codexAccountID)
	if codexAccountID == "" {
		return nil, fmt.Errorf("%w: codex account id is empty", ErrNotFound)
	}
	row, err := r.client.AccountExt.Query().Where(accountext.CodexAccountIDEQ(codexAccountID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: codex_account_id=%s missing", ErrNotFound, codexAccountID)
		}
		return nil, err
	}
	return toDomainAccountExt(row), nil
}
