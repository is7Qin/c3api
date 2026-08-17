// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

const maxCodexOAuthImport = 100

// CodexOAuthImportOptions controls account defaults for a batch import.
type CodexOAuthImportOptions struct {
	TemplateID     int64
	GroupIDs       []int64
	NamePrefix     string
	Weight         int
	MaxConcurrency int
}

type CodexOAuthImportItem struct {
	Index          int64  `json:"index"`
	Status         string `json:"status"`
	AccountID      *int64 `json:"account_id,omitempty"`
	CodexAccountID string `json:"codex_account_id,omitempty"`
	Email          string `json:"email,omitempty"`
	Message        string `json:"message,omitempty"`
}

type CodexOAuthImportResult struct {
	Imported int                    `json:"imported"`
	Updated  int                    `json:"updated"`
	Skipped  int                    `json:"skipped"`
	Failed   int                    `json:"failed"`
	Items    []CodexOAuthImportItem `json:"items"`
}

type codexOAuthInput struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
	Email        string
	ExpiresAt    *time.Time
	Error        string
}

type codexOAuthJSON struct {
	AccessToken       string            `json:"access_token"`
	AccessTokenCamel  string            `json:"accessToken"`
	RefreshToken      string            `json:"refresh_token"`
	RefreshTokenCamel string            `json:"refreshToken"`
	Token             string            `json:"token"`
	AccountID         string            `json:"account_id"`
	AccountIDCamel    string            `json:"accountId"`
	ChatGPTAccountID  string            `json:"chatgpt_account_id"`
	ChatGPTAccountIDC string            `json:"chatgptAccountId"`
	Email             string            `json:"email"`
	Expired           json.RawMessage   `json:"expired"`
	ExpiresAt         json.RawMessage   `json:"expires_at"`
	ExpiresAtCamel    json.RawMessage   `json:"expiresAt"`
	ExpiresIn         json.RawMessage   `json:"expires_in"`
	ExpiresInCamel    json.RawMessage   `json:"expiresIn"`
	OAuthToken        string            `json:"oauth_token"`
	OAuthTokenCamel   string            `json:"oauthToken"`
	OAuthRefresh      string            `json:"oauth_refresh_token"`
	OAuthRefreshCamel string            `json:"oauthRefreshToken"`
	Tokens            *codexOAuthTokens `json:"tokens"`
}

type codexOAuthTokens struct {
	AccessToken       string          `json:"access_token"`
	AccessTokenCamel  string          `json:"accessToken"`
	RefreshToken      string          `json:"refresh_token"`
	RefreshTokenCamel string          `json:"refreshToken"`
	Token             string          `json:"token"`
	Expired           json.RawMessage `json:"expired"`
	ExpiresAt         json.RawMessage `json:"expires_at"`
	ExpiresAtCamel    json.RawMessage `json:"expiresAt"`
	ExpiresIn         json.RawMessage `json:"expires_in"`
	ExpiresInCamel    json.RawMessage `json:"expiresIn"`
}

// ImportCodexOAuth accepts the OAuth JSON formats emitted by Plus and Codex
// clients. Existing external account ids are updated in place; new ids create
// accounts under the selected OAuth template.
func (s *Service) ImportCodexOAuth(ctx context.Context, raw []byte, opts CodexOAuthImportOptions) (*CodexOAuthImportResult, error) {
	if opts.TemplateID <= 0 {
		return nil, ErrInvalidInput
	}
	tpl, err := s.store.GetTemplate(ctx, opts.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != credential.TypeCodexOAuth {
		return nil, ErrInvalidInput
	}
	if len(opts.GroupIDs) > 0 {
		if err := s.checkGroupsExist(ctx, opts.GroupIDs); err != nil {
			return nil, err
		}
	}
	inputs, err := parseCodexOAuthJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(inputs) == 0 || len(inputs) > maxCodexOAuthImport {
		return nil, fmt.Errorf("%w: credentials must contain 1-%d items", ErrInvalidInput, maxCodexOAuthImport)
	}
	if opts.NamePrefix == "" {
		opts.NamePrefix = "codex"
	}
	if opts.Weight < 1 {
		opts.Weight = 100
	}
	if opts.MaxConcurrency < 1 {
		opts.MaxConcurrency = 8
	}

	result := &CodexOAuthImportResult{Items: make([]CodexOAuthImportItem, 0, len(inputs))}
	seen := make(map[string]struct{}, len(inputs))
	for i, input := range inputs {
		item := CodexOAuthImportItem{Index: int64(i), CodexAccountID: input.AccountID, Email: input.Email}
		if input.Error != "" {
			item.Status, item.Message = "failed", input.Error
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		if _, ok := seen[input.AccountID]; ok {
			item.Status, item.Message = "skipped", "duplicate account_id in import"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		seen[input.AccountID] = struct{}{}

		existingExt, findErr := s.store.GetAccountExtByCodexAccountID(ctx, input.AccountID)
		if findErr == nil {
			item = s.updateImportedCodex(ctx, item, input, existingExt, result)
			result.Items = append(result.Items, item)
			continue
		}
		if !errors.Is(findErr, repository.ErrNotFound) {
			item.Status, item.Message = "failed", "lookup failed"
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}

		item = s.createImportedCodex(ctx, item, input, opts, result)
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *Service) updateImportedCodex(ctx context.Context, item CodexOAuthImportItem, input codexOAuthInput, existing *domain.AccountExt, result *CodexOAuthImportResult) CodexOAuthImportItem {
	acc, err := s.store.GetAccount(ctx, existing.AccountID)
	if err != nil {
		item.Status, item.Message = "failed", "account lookup failed"
		result.Failed++
		return item
	}
	if acc.DeletedAt != nil {
		item.Status, item.Message = "failed", "account is deleted"
		result.Failed++
		return item
	}
	at, rt := input.AccessToken, input.RefreshToken
	if input.Email == "" {
		item.Email = derefString(existing.Email)
	}
	updated := &domain.AccountExt{
		AccountID:         existing.AccountID,
		CredentialType:    credential.TypeCodexOAuth,
		CodexAccountID:    &input.AccountID,
		InstallationID:    existing.InstallationID,
		SessionID:         existing.SessionID,
		ThreadID:          existing.ThreadID,
		WindowID:          existing.WindowID,
		OAuthToken:        &at,
		OAuthRefreshToken: &rt,
		OAuthExpiresAt:    input.ExpiresAt,
		Email:             nullableString(input.Email, existing.Email),
	}
	if _, err := s.UpsertAccountExt(ctx, updated); err != nil {
		item.Status, item.Message = "failed", "credential update failed"
		result.Failed++
		return item
	}
	item.Status = "updated"
	result.Updated++
	return item
}

func (s *Service) createImportedCodex(ctx context.Context, item CodexOAuthImportItem, input codexOAuthInput, opts CodexOAuthImportOptions, result *CodexOAuthImportResult) CodexOAuthImportItem {
	groupIDs := append([]int64(nil), opts.GroupIDs...)
	account := &domain.Account{
		Name:           importedCodexName(opts.NamePrefix, input.AccountID),
		TemplateID:     opts.TemplateID,
		Status:         domain.StatusActive,
		Weight:         opts.Weight,
		MaxConcurrency: opts.MaxConcurrency,
		GroupIDs:       &groupIDs,
	}
	created, err := s.CreateAccount(ctx, account)
	if err != nil {
		item.Status, item.Message = "failed", "account creation failed"
		result.Failed++
		return item
	}
	item.AccountID = &created.ID
	at, rt := input.AccessToken, input.RefreshToken
	ext := &domain.AccountExt{
		AccountID:         created.ID,
		CredentialType:    credential.TypeCodexOAuth,
		CodexAccountID:    &input.AccountID,
		OAuthToken:        &at,
		OAuthRefreshToken: &rt,
		OAuthExpiresAt:    input.ExpiresAt,
		Email:             nullableString(input.Email, nil),
	}
	if _, err := s.UpsertAccountExt(ctx, ext); err != nil {
		if cleanupErr := s.DeleteAccount(ctx, created.ID); cleanupErr != nil {
			item.Status, item.Message = "failed", "credential creation failed; account cleanup failed"
			result.Failed++
			return item
		}
		if errors.Is(err, ErrConflict) {
			winner, lookupErr := s.store.GetAccountExtByCodexAccountID(ctx, input.AccountID)
			if lookupErr == nil {
				item.AccountID = &winner.AccountID
				item.Status, item.Message = "skipped", "account was imported concurrently"
				result.Skipped++
				return item
			}
		}
		item.Status, item.Message = "failed", "credential creation failed"
		result.Failed++
		return item
	}
	item.Status = "imported"
	result.Imported++
	return item
}

func importedCodexName(prefix, accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return strings.TrimSpace(prefix) + "-" + hex.EncodeToString(sum[:4])
}

func nullableString(value string, fallback *string) *string {
	if value != "" {
		return &value
	}
	return fallback
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func parseCodexOAuthJSON(raw []byte) ([]codexOAuthInput, error) {
	var value json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid JSON")
	}
	return parseCodexOAuthValue(value)
}

func parseCodexOAuthValue(raw json.RawMessage) ([]codexOAuthInput, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("invalid credentials array")
		}
		out := make([]codexOAuthInput, 0, len(values))
		for _, value := range values {
			items, err := parseCodexOAuthValue(value)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("credential must be an object")
	}
	for _, key := range []string{"credentials", "accounts", "items", "data"} {
		if nested, ok := object[key]; ok {
			items, err := parseCodexOAuthValue(nested)
			if err != nil {
				return nil, err
			}
			if len(items) > 0 {
				return items, nil
			}
		}
	}
	var item codexOAuthJSON
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("invalid credential object")
	}
	var nestedAccessToken, nestedAccessTokenCamel, nestedToken string
	var nestedRefreshToken, nestedRefreshTokenCamel string
	var nestedExpired, nestedExpiresAt, nestedExpiresAtCamel json.RawMessage
	var nestedExpiresIn, nestedExpiresInCamel json.RawMessage
	if item.Tokens != nil {
		nestedAccessToken = item.Tokens.AccessToken
		nestedAccessTokenCamel = item.Tokens.AccessTokenCamel
		nestedToken = item.Tokens.Token
		nestedRefreshToken = item.Tokens.RefreshToken
		nestedRefreshTokenCamel = item.Tokens.RefreshTokenCamel
		nestedExpired = item.Tokens.Expired
		nestedExpiresAt = item.Tokens.ExpiresAt
		nestedExpiresAtCamel = item.Tokens.ExpiresAtCamel
		nestedExpiresIn = item.Tokens.ExpiresIn
		nestedExpiresInCamel = item.Tokens.ExpiresInCamel
	}
	item.AccessToken = firstNonEmptyCodexOAuthToken(
		nestedAccessToken,
		nestedAccessTokenCamel,
		nestedToken,
		item.AccessToken,
		item.AccessTokenCamel,
		item.OAuthToken,
		item.OAuthTokenCamel,
		item.Token,
	)
	item.RefreshToken = firstNonEmptyCodexOAuthToken(
		nestedRefreshToken,
		nestedRefreshTokenCamel,
		item.RefreshToken,
		item.RefreshTokenCamel,
		item.OAuthRefresh,
		item.OAuthRefreshCamel,
	)
	item.AccountID = firstNonEmptyCodexOAuthToken(
		item.ChatGPTAccountID,
		item.ChatGPTAccountIDC,
		item.AccountID,
		item.AccountIDCamel,
	)
	item.Email = strings.TrimSpace(item.Email)
	if item.AccountID == "" {
		item.AccountID = jwtCodexAccountID(item.AccessToken)
	}
	if item.Email == "" {
		item.Email = jwtEmail(item.AccessToken)
	}
	input := codexOAuthInput{AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, AccountID: item.AccountID, Email: item.Email}
	input.ExpiresAt = parseOAuthExpiry(
		firstCodexOAuthRaw(item.Expired, nestedExpired),
		firstCodexOAuthRaw(item.ExpiresAt, item.ExpiresAtCamel, nestedExpiresAt, nestedExpiresAtCamel),
		firstCodexOAuthRaw(item.ExpiresIn, item.ExpiresInCamel, nestedExpiresIn, nestedExpiresInCamel),
	)
	switch {
	case input.AccessToken == "":
		input.Error = "access_token is required"
	case input.RefreshToken == "":
		input.Error = "refresh_token is required"
	case input.AccountID == "":
		input.Error = "account_id is required"
	}
	return []codexOAuthInput{input}, nil
}

func parseOAuthExpiry(expired, expiresAt, expiresIn json.RawMessage) *time.Time {
	for _, raw := range []json.RawMessage{expired, expiresAt} {
		if t := parseOAuthTime(raw); t != nil {
			return t
		}
	}
	if seconds := parseOAuthNumber(expiresIn); seconds > 0 {
		if maxSeconds := float64(codexMaxExpiresIn / time.Second); seconds > maxSeconds {
			seconds = maxSeconds
		}
		t := time.Now().Add(time.Duration(seconds) * time.Second)
		return &t
	}
	return nil
}

func firstNonEmptyCodexOAuthToken(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstCodexOAuthRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

func parseOAuthTime(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
		text = strings.TrimSpace(text)
		if t, err := time.Parse(time.RFC3339, text); err == nil {
			return &t
		}
		if seconds := parseOAuthNumber(json.RawMessage(text)); seconds > 0 {
			return unixOAuthTime(seconds)
		}
	}
	if seconds := parseOAuthNumber(raw); seconds > 0 {
		return unixOAuthTime(seconds)
	}
	return nil
}

func unixOAuthTime(seconds float64) *time.Time {
	if seconds > 1e12 {
		seconds /= 1000
	}
	if seconds <= 0 || seconds > float64(^uint64(0)>>1) {
		return nil
	}
	t := time.Unix(int64(seconds), 0).UTC()
	return &t
}

func parseOAuthNumber(raw json.RawMessage) float64 {
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		number, _ = strconv.ParseFloat(strings.TrimSpace(text), 64)
		return number
	}
	return 0
}

func jwtCodexAccountID(token string) string {
	claims := jwtPayload(token)
	if value, ok := claims["https://api.openai.com/auth.chatgpt_account_id"].(string); ok {
		return strings.TrimSpace(value)
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if value, ok := nested["chatgpt_account_id"].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jwtEmail(token string) string {
	claims := jwtPayload(token)
	for _, key := range []string{"email", "https://api.openai.com/auth.email"} {
		if value, ok := claims[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}
