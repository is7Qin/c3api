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

const (
	maxCodexOAuthImport = 100
	codexMaxExpiresIn   = 365 * 24 * time.Hour
)

// CodexOAuthImportOptions controls account defaults for a batch import.
type CodexOAuthImportOptions struct {
	TemplateID     int64
	GroupIDs       []int64
	NamePrefix     string
	Weight         int
	MaxConcurrency int
}

type CodexOAuthImportItem struct {
	Index     int64
	Status    string
	AccountID *int64
	SpaceID   string
	Email     string
	Message   string
}

type CodexOAuthImportResult struct {
	Imported int
	Updated  int
	Skipped  int
	Failed   int
	Items    []CodexOAuthImportItem
}

type codexOAuthInput struct {
	AccessToken  string
	RefreshToken string
	SpaceID      string
	Email        string
	ExpiresAt    *time.Time
	Error        string
}

type codexOAuthJSON struct {
	AccessToken       string            `json:"access_token"`
	AccessTokenCamel  string            `json:"accessToken"`
	IDToken           string            `json:"id_token"`
	IDTokenCamel      string            `json:"idToken"`
	RefreshToken      string            `json:"refresh_token"`
	RefreshTokenCamel string            `json:"refreshToken"`
	Token             string            `json:"token"`
	SpaceID           string            `json:"space_id"`
	SpaceIDCamel      string            `json:"spaceId"`
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
	IDToken           string          `json:"id_token"`
	IDTokenCamel      string          `json:"idToken"`
	RefreshToken      string          `json:"refresh_token"`
	RefreshTokenCamel string          `json:"refreshToken"`
	Token             string          `json:"token"`
	SpaceID           string          `json:"space_id"`
	SpaceIDCamel      string          `json:"spaceId"`
	AccountID         string          `json:"account_id"`
	AccountIDCamel    string          `json:"accountId"`
	ChatGPTAccountID  string          `json:"chatgpt_account_id"`
	ChatGPTAccountIDC string          `json:"chatgptAccountId"`
	Email             string          `json:"email"`
	Expired           json.RawMessage `json:"expired"`
	ExpiresAt         json.RawMessage `json:"expires_at"`
	ExpiresAtCamel    json.RawMessage `json:"expiresAt"`
	ExpiresIn         json.RawMessage `json:"expires_in"`
	ExpiresInCamel    json.RawMessage `json:"expiresIn"`
}

// ImportCodexOAuth imports OAuth JSON emitted by Codex clients. Existing
// email + space identities are updated in place; new pairs create accounts
// under the selected OAuth template.
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
	opts.NamePrefix = strings.TrimSpace(opts.NamePrefix)
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
	type identityKey struct {
		email   string
		spaceID string
	}
	seen := make(map[identityKey]struct{}, len(inputs))
	for i, input := range inputs {
		item := CodexOAuthImportItem{Index: int64(i), SpaceID: input.SpaceID, Email: input.Email}
		if input.Error != "" {
			item.Status, item.Message = "failed", input.Error
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}

		key := identityKey{email: input.Email, spaceID: input.SpaceID}
		if _, ok := seen[key]; ok {
			item.Status, item.Message = "skipped", "duplicate email + space_id in import"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		seen[key] = struct{}{}

		existing, findErr := s.store.GetAccountExtByCodexEmailAndAccountID(ctx, input.Email, input.SpaceID)
		if findErr == nil {
			item = s.updateImportedCodex(ctx, item, input, existing, result)
			result.Items = append(result.Items, item)
			continue
		}
		if !errors.Is(findErr, repository.ErrNotFound) {
			item.Status, item.Message = "failed", "identity lookup failed"
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
		item.Status, item.Message = "failed", "matching account is deleted"
		result.Failed++
		return item
	}
	if existing.CredentialType != credential.TypeCodexOAuth {
		item.Status, item.Message = "failed", "matching account is not Codex OAuth"
		result.Failed++
		return item
	}

	identity := existing.CodexIdentity
	if identity == nil {
		fresh := NewCodexIdentity()
		identity = &fresh
	}
	accessToken, refreshToken := input.AccessToken, input.RefreshToken
	email, spaceID := input.Email, input.SpaceID
	updated := &domain.AccountExt{
		AccountID:              existing.AccountID,
		CredentialType:         credential.TypeCodexOAuth,
		CodexIdentity:          identity,
		CodexOAuthToken:        &accessToken,
		CodexOAuthRefreshToken: &refreshToken,
		CodexOAuthExpiresAt:    input.ExpiresAt,
		CodexEmail:             &email,
		CodexAccountID:         &spaceID,
	}
	if _, err := s.UpsertAccountExt(ctx, updated); err != nil {
		item.Status, item.Message = "failed", "credential update failed"
		result.Failed++
		return item
	}
	item.AccountID = &existing.AccountID
	item.Status = "updated"
	result.Updated++
	return item
}

func (s *Service) createImportedCodex(ctx context.Context, item CodexOAuthImportItem, input codexOAuthInput, opts CodexOAuthImportOptions, result *CodexOAuthImportResult) CodexOAuthImportItem {
	groupIDs := append([]int64(nil), opts.GroupIDs...)
	account := &domain.Account{
		Name:           importedCodexName(opts.NamePrefix, input.Email, input.SpaceID),
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

	accessToken, refreshToken := input.AccessToken, input.RefreshToken
	email, spaceID := input.Email, input.SpaceID
	identity := NewCodexIdentity()
	ext := &domain.AccountExt{
		AccountID:              created.ID,
		CredentialType:         credential.TypeCodexOAuth,
		CodexIdentity:          &identity,
		CodexOAuthToken:        &accessToken,
		CodexOAuthRefreshToken: &refreshToken,
		CodexOAuthExpiresAt:    input.ExpiresAt,
		CodexEmail:             &email,
		CodexAccountID:         &spaceID,
	}
	if _, err := s.UpsertAccountExt(ctx, ext); err != nil {
		if cleanupErr := s.DeleteAccount(ctx, created.ID); cleanupErr != nil {
			item.Status, item.Message = "failed", "credential creation failed; account cleanup failed"
			result.Failed++
			return item
		}
		if errors.Is(err, ErrConflict) {
			winner, lookupErr := s.store.GetAccountExtByCodexEmailAndAccountID(ctx, input.Email, input.SpaceID)
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

func importedCodexName(prefix, email, spaceID string) string {
	sum := sha256.Sum256([]byte(email + "\x00" + spaceID))
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

func parseCodexOAuthJSON(raw []byte) ([]codexOAuthInput, error) {
	var value json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("invalid JSON")
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
			return nil, errors.New("invalid credentials array")
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
		return nil, errors.New("credential must be an object")
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
		return nil, errors.New("invalid credential object")
	}
	var nestedAccessToken, nestedAccessTokenCamel, nestedIDToken, nestedIDTokenCamel, nestedToken string
	var nestedRefreshToken, nestedRefreshTokenCamel string
	var nestedSpaceID, nestedSpaceIDCamel, nestedAccountID, nestedAccountIDCamel string
	var nestedChatGPTAccountID, nestedChatGPTAccountIDC, nestedEmail string
	var nestedExpired, nestedExpiresAt, nestedExpiresAtCamel json.RawMessage
	var nestedExpiresIn, nestedExpiresInCamel json.RawMessage
	if item.Tokens != nil {
		nestedAccessToken = item.Tokens.AccessToken
		nestedAccessTokenCamel = item.Tokens.AccessTokenCamel
		nestedIDToken = item.Tokens.IDToken
		nestedIDTokenCamel = item.Tokens.IDTokenCamel
		nestedToken = item.Tokens.Token
		nestedRefreshToken = item.Tokens.RefreshToken
		nestedRefreshTokenCamel = item.Tokens.RefreshTokenCamel
		nestedSpaceID = item.Tokens.SpaceID
		nestedSpaceIDCamel = item.Tokens.SpaceIDCamel
		nestedAccountID = item.Tokens.AccountID
		nestedAccountIDCamel = item.Tokens.AccountIDCamel
		nestedChatGPTAccountID = item.Tokens.ChatGPTAccountID
		nestedChatGPTAccountIDC = item.Tokens.ChatGPTAccountIDC
		nestedEmail = item.Tokens.Email
		nestedExpired = item.Tokens.Expired
		nestedExpiresAt = item.Tokens.ExpiresAt
		nestedExpiresAtCamel = item.Tokens.ExpiresAtCamel
		nestedExpiresIn = item.Tokens.ExpiresIn
		nestedExpiresInCamel = item.Tokens.ExpiresInCamel
	}

	item.AccessToken = firstNonEmptyCodexOAuthToken(
		nestedAccessToken, nestedAccessTokenCamel, nestedToken,
		item.AccessToken, item.AccessTokenCamel, item.OAuthToken, item.OAuthTokenCamel, item.Token,
	)
	item.RefreshToken = firstNonEmptyCodexOAuthToken(
		nestedRefreshToken, nestedRefreshTokenCamel,
		item.RefreshToken, item.RefreshTokenCamel, item.OAuthRefresh, item.OAuthRefreshCamel,
	)
	item.SpaceID = firstNonEmptyCodexOAuthToken(
		nestedSpaceID, nestedSpaceIDCamel, nestedChatGPTAccountID, nestedChatGPTAccountIDC,
		nestedAccountID, nestedAccountIDCamel,
		item.SpaceID, item.SpaceIDCamel, item.ChatGPTAccountID, item.ChatGPTAccountIDC,
		item.AccountID, item.AccountIDCamel,
	)
	item.Email = firstNonEmptyCodexOAuthToken(nestedEmail, item.Email)
	for _, identityToken := range []string{nestedIDToken, nestedIDTokenCamel, item.IDToken, item.IDTokenCamel, item.AccessToken} {
		if item.SpaceID == "" {
			item.SpaceID = jwtCodexSpaceID(identityToken)
		}
		if item.Email == "" {
			item.Email = jwtEmail(identityToken)
		}
		if item.SpaceID != "" && item.Email != "" {
			break
		}
	}

	input := codexOAuthInput{
		AccessToken:  strings.TrimSpace(item.AccessToken),
		RefreshToken: strings.TrimSpace(item.RefreshToken),
		SpaceID:      strings.TrimSpace(item.SpaceID),
		Email:        strings.ToLower(strings.TrimSpace(item.Email)),
		ExpiresAt: parseOAuthExpiry(
			firstCodexOAuthRaw(item.Expired, nestedExpired),
			firstCodexOAuthRaw(item.ExpiresAt, item.ExpiresAtCamel, nestedExpiresAt, nestedExpiresAtCamel),
			firstCodexOAuthRaw(item.ExpiresIn, item.ExpiresInCamel, nestedExpiresIn, nestedExpiresInCamel),
		),
	}
	switch {
	case input.AccessToken == "":
		input.Error = "access_token is required"
	case input.RefreshToken == "":
		input.Error = "refresh_token is required"
	case input.Email == "":
		input.Error = "email is required (include it in the OAuth JSON or token claims)"
	case input.SpaceID == "":
		input.Error = "space_id is required (account_id is accepted as a legacy alias)"
	}
	return []codexOAuthInput{input}, nil
}

func parseOAuthExpiry(expired, expiresAt, expiresIn json.RawMessage) *time.Time {
	for _, raw := range []json.RawMessage{expired, expiresAt} {
		if parsed := parseOAuthTime(raw); parsed != nil {
			return parsed
		}
	}
	if seconds := parseOAuthNumber(expiresIn); seconds > 0 {
		if maxSeconds := float64(codexMaxExpiresIn / time.Second); seconds > maxSeconds {
			seconds = maxSeconds
		}
		parsed := time.Now().Add(time.Duration(seconds) * time.Second)
		return &parsed
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
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return &parsed
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
	parsed := time.Unix(int64(seconds), 0).UTC()
	return &parsed
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

func jwtCodexSpaceID(token string) string {
	claims := jwtPayload(token)
	for _, key := range []string{
		"https://api.openai.com/auth.chatgpt_account_id",
		"https://api.openai.com/auth.account_id",
		"https://api.openai.com/auth.space_id",
		"space_id",
		"account_id",
	} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "account_id", "space_id"} {
			if value, ok := nested[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func jwtEmail(token string) string {
	claims := jwtPayload(token)
	for _, key := range []string{"email", "https://api.openai.com/auth.email"} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"https://api.openai.com/profile", "https://api.openai.com/auth"} {
		if nested, ok := claims[key].(map[string]any); ok {
			if value, ok := nested["email"].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
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
