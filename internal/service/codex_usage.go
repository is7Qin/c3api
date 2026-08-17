// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

const (
	codexUsageCacheTTL = 5 * time.Minute
	codexUsageTimeout  = 20 * time.Second
	codexMaxExpiresIn  = 365 * 24 * time.Hour
	codexRefreshURL    = "https://auth.openai.com/oauth/token"
	codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// CodexUsageResult is the management-safe representation of the two upstream
// quota documents. Raw payloads are retained because OpenAI adds fields without
// versioning this endpoint; neither payload contains credentials.
type CodexUsageResult struct {
	AccountID         int64           `json:"account_id"`
	CodexAccountID    string          `json:"codex_account_id"`
	Email             *string         `json:"email,omitempty"`
	FetchedAt         time.Time       `json:"fetched_at"`
	Usage             json.RawMessage `json:"usage"`
	ResetCredits      json.RawMessage `json:"reset_credits,omitempty"`
	ResetCreditsError string          `json:"reset_credits_error,omitempty"`
}

type CodexUsageBatchItem struct {
	AccountID int64             `json:"account_id"`
	Status    string            `json:"status"`
	Usage     *CodexUsageResult `json:"usage,omitempty"`
	Message   string            `json:"message,omitempty"`
}

type CodexUsageBatchResult struct {
	Items []CodexUsageBatchItem `json:"items"`
}

type codexUsageCacheEntry struct {
	result    *CodexUsageResult
	fetchedAt time.Time
}

type codexUsageCall struct {
	done   chan struct{}
	result *CodexUsageResult
	err    error
}

type codexUsageManager struct {
	service *Service
	log     *logx.Logger

	clientMu sync.RWMutex
	client   *http.Client

	mu         sync.Mutex
	cache      map[int64]codexUsageCacheEntry
	inflight   map[int64]*codexUsageCall
	now        func() time.Time
	refreshURL func() string
}

func newCodexUsageManager(s *Service, client *http.Client, log *logx.Logger) *codexUsageManager {
	if client == nil {
		client = http.DefaultClient
	}
	return &codexUsageManager{
		service:    s,
		log:        log,
		client:     client,
		cache:      make(map[int64]codexUsageCacheEntry),
		inflight:   make(map[int64]*codexUsageCall),
		now:        time.Now,
		refreshURL: func() string { return codexRefreshURL },
	}
}

func (m *codexUsageManager) setHTTPClient(client *http.Client) {
	if client == nil {
		client = http.DefaultClient
	}
	m.clientMu.Lock()
	m.client = client
	m.clientMu.Unlock()
}

func (m *codexUsageManager) httpClient() *http.Client {
	m.clientMu.RLock()
	client := m.client
	m.clientMu.RUnlock()
	if client == nil {
		return http.DefaultClient
	}
	return client
}

func (m *codexUsageManager) get(ctx context.Context, accountID int64, force bool) (*CodexUsageResult, error) {
	if accountID <= 0 {
		return nil, ErrInvalidInput
	}
	m.mu.Lock()
	if !force {
		if cached, ok := m.cache[accountID]; ok && m.now().Sub(cached.fetchedAt) < codexUsageCacheTTL {
			result := cloneCodexUsage(cached.result)
			m.mu.Unlock()
			return result, nil
		}
	}
	if call := m.inflight[accountID]; call != nil {
		m.mu.Unlock()
		select {
		case <-call.done:
			return cloneCodexUsage(call.result), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &codexUsageCall{done: make(chan struct{})}
	m.inflight[accountID] = call
	m.mu.Unlock()

	result, err := m.fetch(ctx, accountID)
	m.mu.Lock()
	call.result, call.err = result, err
	if err == nil {
		m.cache[accountID] = codexUsageCacheEntry{result: cloneCodexUsage(result), fetchedAt: result.FetchedAt}
	}
	delete(m.inflight, accountID)
	close(call.done)
	m.mu.Unlock()
	return cloneCodexUsage(result), err
}

func cloneCodexUsage(in *CodexUsageResult) *CodexUsageResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Usage = append(json.RawMessage(nil), in.Usage...)
	out.ResetCredits = append(json.RawMessage(nil), in.ResetCredits...)
	return &out
}

func (s *Service) codexUsageFor(ctx context.Context, accountID int64, force bool) (*CodexUsageResult, error) {
	return s.getCodexUsageManager().get(ctx, accountID, force)
}

// GetCodexUsage returns a cached result when it is younger than five minutes.
func (s *Service) GetCodexUsage(ctx context.Context, accountID int64) (*CodexUsageResult, error) {
	return s.codexUsageFor(ctx, accountID, false)
}

// RefreshCodexUsage bypasses the per-account cache and performs one upstream
// refresh. Concurrent callers for the same account still share one request.
func (s *Service) RefreshCodexUsage(ctx context.Context, accountID int64) (*CodexUsageResult, error) {
	return s.codexUsageFor(ctx, accountID, true)
}

// RefreshCodexUsageBatch refreshes selected accounts with bounded concurrency;
// one failed account never hides successful results for the rest of the batch.
func (s *Service) RefreshCodexUsageBatch(ctx context.Context, accountIDs []int64) *CodexUsageBatchResult {
	result := &CodexUsageBatchResult{Items: make([]CodexUsageBatchItem, len(accountIDs))}
	if len(accountIDs) == 0 {
		return result
	}
	jobs := make(chan int)
	workers := len(accountIDs)
	if workers > 8 {
		workers = 8
	}
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				id := accountIDs[index]
				item := CodexUsageBatchItem{AccountID: id}
				if err := ctx.Err(); err != nil {
					item.Status = "failed"
					item.Message = err.Error()
					result.Items[index] = item
					continue
				}
				usage, err := s.RefreshCodexUsage(ctx, id)
				if err != nil {
					item.Status = "failed"
					item.Message = err.Error()
				} else {
					item.Status = "refreshed"
					item.Usage = usage
				}
				result.Items[index] = item
			}
		}()
	}
	for index := range accountIDs {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return result
}

func (m *codexUsageManager) fetch(ctx context.Context, accountID int64) (*CodexUsageResult, error) {
	account, err := m.service.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	template, err := m.service.store.GetTemplate(ctx, account.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	ext, err := m.service.store.GetAccountExt(ctx, accountID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if ext.CredentialType != credential.TypeCodexOAuth || ext.OAuthToken == nil || strings.TrimSpace(*ext.OAuthToken) == "" {
		return nil, fmt.Errorf("%w: account is not a Codex OAuth account", ErrInvalidInput)
	}
	if ext.CodexAccountID == nil || strings.TrimSpace(*ext.CodexAccountID) == "" {
		return nil, fmt.Errorf("%w: codex account_id is missing", ErrInvalidInput)
	}
	baseURL := strings.TrimSpace(template.BaseURL)
	if account.BaseURL != nil && strings.TrimSpace(*account.BaseURL) != "" {
		baseURL = strings.TrimSpace(*account.BaseURL)
	}
	if baseURL == "" {
		return nil, fmt.Errorf("%w: Codex base_url is missing", ErrInvalidInput)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	token := strings.TrimSpace(*ext.OAuthToken)
	accountExternalID := strings.TrimSpace(*ext.CodexAccountID)
	refreshed := false
	usageBody, status, err := m.fetchEndpoint(ctx, baseURL, token, accountExternalID, "/backend-api/wham/usage")
	if err != nil {
		return nil, err
	}
	if isCodexAuthStatus(status) {
		token, err = m.refreshIfStale(ctx, accountID, ext, token)
		if err != nil {
			return nil, err
		}
		refreshed = true
		usageBody, status, err = m.fetchEndpoint(ctx, baseURL, token, accountExternalID, "/backend-api/wham/usage")
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, codexUpstreamError("usage", status, usageBody)
	}
	if !json.Valid(usageBody) {
		return nil, errors.New("codex usage response is not valid JSON")
	}

	resetBody, resetStatus, resetErr := m.fetchEndpoint(ctx, baseURL, token, accountExternalID, "/backend-api/wham/rate-limit-reset-credits")
	if resetErr != nil {
		return nil, resetErr
	}
	if isCodexAuthStatus(resetStatus) {
		if !refreshed {
			token, err = m.refreshIfStale(ctx, accountID, ext, token)
			if err != nil {
				return nil, err
			}
			refreshed = true
		}
		resetBody, resetStatus, resetErr = m.fetchEndpoint(ctx, baseURL, token, accountExternalID, "/backend-api/wham/rate-limit-reset-credits")
		if resetErr != nil {
			return nil, resetErr
		}
	}
	result := &CodexUsageResult{
		AccountID:      accountID,
		CodexAccountID: accountExternalID,
		Email:          ext.Email,
		FetchedAt:      m.now().UTC(),
		Usage:          append(json.RawMessage(nil), usageBody...),
	}
	if resetStatus >= 200 && resetStatus < 300 {
		if !json.Valid(resetBody) {
			result.ResetCreditsError = "reset credits response is not valid JSON"
		} else {
			result.ResetCredits = append(json.RawMessage(nil), resetBody...)
		}
	} else {
		result.ResetCreditsError = fmt.Sprintf("upstream status: %d", resetStatus)
	}
	return result, nil
}

func (m *codexUsageManager) fetchEndpoint(ctx context.Context, baseURL, token, accountID, path string) ([]byte, int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, codexUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func isCodexAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func codexUpstreamError(endpoint string, status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		return fmt.Errorf("codex %s upstream status: %d", endpoint, status)
	}
	return fmt.Errorf("codex %s upstream status: %d: %s", endpoint, status, message)
}

func (m *codexUsageManager) refreshIfStale(ctx context.Context, accountID int64, original *domain.AccountExt, oldToken string) (string, error) {
	latest, err := m.service.store.GetAccountExt(ctx, accountID)
	if err != nil {
		return "", mapRepoErr(err)
	}
	if latest.OAuthToken != nil && strings.TrimSpace(*latest.OAuthToken) != "" && strings.TrimSpace(*latest.OAuthToken) != oldToken {
		return strings.TrimSpace(*latest.OAuthToken), nil
	}
	// Use the latest row for the refresh token and expiry. Another management
	// update may have rotated the refresh token after the initial request read.
	source := latest
	if source.OAuthRefreshToken == nil || strings.TrimSpace(*source.OAuthRefreshToken) == "" {
		return "", errors.New("codex OAuth refresh_token is missing")
	}
	refreshToken := strings.TrimSpace(*source.OAuthRefreshToken)
	payload, err := json.Marshal(map[string]string{
		"client_id":     codexOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	refreshURL := codexRefreshURL
	if m.refreshURL != nil {
		if configured := strings.TrimSpace(m.refreshURL()); configured != "" {
			refreshURL = configured
		}
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, refreshURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs")
	req.Header.Set("Originator", "codex_cli_rs")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", codexUpstreamError("OAuth refresh", resp.StatusCode, body)
	}
	var refreshed struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&refreshed); err != nil {
		return "", fmt.Errorf("invalid OAuth refresh response: %w", err)
	}
	newToken := strings.TrimSpace(refreshed.AccessToken)
	if newToken == "" {
		return "", errors.New("OAuth refresh response missing access_token")
	}
	newRefresh := strings.TrimSpace(refreshed.RefreshToken)
	if newRefresh == "" {
		newRefresh = refreshToken
	}
	var expiresAt *time.Time
	maxSeconds := int64(codexMaxExpiresIn / time.Second)
	if seconds, parseErr := strconv.ParseInt(string(refreshed.ExpiresIn), 10, 64); parseErr == nil && seconds > 0 && seconds <= maxSeconds {
		expires := m.now().UTC().Add(time.Duration(seconds) * time.Second)
		expiresAt = &expires
	} else {
		expiresAt = source.OAuthExpiresAt
	}
	updated, err := m.service.store.WriteOAuthRotationIfCurrent(ctx, accountID, refreshToken, newToken, newRefresh, expiresAt)
	if err != nil {
		return "", fmt.Errorf("persist refreshed OAuth credentials: %w", err)
	}
	if !updated {
		// A concurrent refresh won. Never overwrite it with this response; use
		// the winner's access token for the retry and let the next request use
		// the persisted refresh token as well.
		latest, err = m.service.store.GetAccountExt(ctx, accountID)
		if err != nil {
			return "", fmt.Errorf("read winning OAuth credentials: %w", mapRepoErr(err))
		}
		if latest.OAuthToken == nil || strings.TrimSpace(*latest.OAuthToken) == "" {
			return "", errors.New("concurrent OAuth rotation did not leave an access token")
		}
		return strings.TrimSpace(*latest.OAuthToken), nil
	}
	m.service.accountExtChanged(ctx, accountID)
	return newToken, nil
}
