// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// —— 设备码登录适配（协议在 codex-sdk；本层只做 domain 翻译与状态映射） ——

// codexLoginJWT 构造 payload 为指定 claims 的 JWT（SDK 不验签）。
func codexLoginJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCodexLoginStartAndPoll(t *testing.T) {
	idToken := codexLoginJWT(t, map[string]any{
		"email":                       "codex@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-9"},
	})
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = w.Write([]byte(`{"device_auth_id":"auth-1","user_code":"ABCD-EFGHI","interval":"4"}`))
		case "/api/accounts/deviceauth/token":
			switch status {
			case http.StatusOK:
				_, _ = w.Write([]byte(`{"authorization_code":"c","code_verifier":"v"}`))
			case http.StatusForbidden, http.StatusGone:
				w.WriteHeader(status)
			}
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-1", "refresh_token": "rt-1", "id_token": idToken, "expires_in": 3600,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	l := newCodexLogin(codexsdk.NewDeviceAuthClient(codexsdk.WithIssuer(srv.URL)))
	ctx := context.Background()

	code, err := l.StartDeviceFlow(ctx)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if code.AuthID != "auth-1" || code.UserCode != "ABCD-EFGHI" {
		t.Fatalf("code = %+v", code)
	}
	if code.VerificationURL != srv.URL+"/codex/device" {
		t.Fatalf("VerificationURL = %q", code.VerificationURL)
	}
	// 上游未下发 expires_in 时 SDK 按真客户端 15 分钟上限补齐——对外恒为正，
	// 前端才能维护本地截止时间（0 会被读成「永不过期」）。
	if code.Interval.Seconds() != 4 || code.ExpiresIn != 15*time.Minute {
		t.Fatalf("interval/expires = %v/%v", code.Interval, code.ExpiresIn)
	}

	// pending：状态枚举，非错误。
	status = http.StatusForbidden
	st, tokens, err := l.PollDeviceFlow(ctx, code.AuthID, code.UserCode)
	if err != nil {
		t.Fatalf("pending err = %v", err)
	}
	if st != domain.CodexLoginPending || tokens != nil {
		t.Fatalf("pending = %v/%v", st, tokens)
	}

	// expired：状态枚举。
	status = http.StatusGone
	st, tokens, err = l.PollDeviceFlow(ctx, code.AuthID, code.UserCode)
	if err != nil {
		t.Fatalf("expired err = %v", err)
	}
	if st != domain.CodexLoginExpired || tokens != nil {
		t.Fatalf("expired = %v/%v", st, tokens)
	}

	// authorized：tokens 翻译（含 claims 派生的 email/account id 与过期时刻）。
	status = http.StatusOK
	st, tokens, err = l.PollDeviceFlow(ctx, code.AuthID, code.UserCode)
	if err != nil {
		t.Fatalf("authorized err = %v", err)
	}
	if st != domain.CodexLoginAuthorized {
		t.Fatalf("status = %v", st)
	}
	if tokens == nil || tokens.AccessToken != "at-1" || tokens.RefreshToken != "rt-1" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens.Email != "codex@example.com" || tokens.AccountID != "acct-9" {
		t.Fatalf("claims = %q/%q", tokens.Email, tokens.AccountID)
	}
	if tokens.ExpiresAt == nil {
		t.Fatal("ExpiresAt 应非 nil（expires_in 已下发）")
	}

	// 上游 5xx：非哨兵 → 原样错误（调用方据此保留会话并可重试）。
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	l500 := newCodexLogin(codexsdk.NewDeviceAuthClient(codexsdk.WithIssuer(srv500.URL)))
	if _, _, err := l500.PollDeviceFlow(ctx, "a", "u"); err == nil {
		t.Fatal("500 应返回错误（不能误判为 pending 终态）")
	}
	// 发起失败同理原样上抛。
	if _, err := l500.StartDeviceFlow(ctx); err == nil {
		t.Fatal("usercode 500 应返回错误")
	}
}

func TestCodexLoginDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()

	l := newCodexLogin(codexsdk.NewDeviceAuthClient(codexsdk.WithIssuer(srv.URL)))
	st, tokens, err := l.PollDeviceFlow(context.Background(), "a", "u")
	if err != nil {
		t.Fatalf("denied err = %v", err)
	}
	if st != domain.CodexLoginDenied || tokens != nil {
		t.Fatalf("denied = %v/%v", st, tokens)
	}
}
