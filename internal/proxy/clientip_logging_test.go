// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// client_ip 落库端到端（fake 基座，spec 2026-08-17 S-E）：
//   - on：伪造头命中 → usage/err 行带 client_ip
//   - off：伪造头被忽略（恒 RemoteAddr 剥端口——零伪造面）
//   - on 无头 → RemoteAddr 值
//   - 不变量（gate M1）：401 鉴权失败（rm 创建在鉴权前）+ 429 并发超限 + 组
//     限流 + 402 余额预检——全部拒绝路径 err_logs 行恒带 client_ip
//
// 开关经测试 seam 构造后翻转（p.cfg.BehindCDN/p.limit，同 p.auth.Upsert 先例
// ——newTestProxyTimeoutLogs 族构造函数硬编码默认关，翻转即开）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// on：成功路径 usage_logs 行 + 错误路径 err_logs 行均带 client_ip。
func TestProxyClientIPBehindCDNOnUsageAndErrLogs(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	p.cfg.BehindCDN = true

	req := chatReq("ck-1")
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))

	store.mu.Lock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "9.9.9.9", store.logs[0].ClientIP, "on 伪造头命中 → usage 行带 client_ip")
	store.mu.Unlock()

	// 错误路径：4xx 透传 → err_logs 行带 client_ip（X-Real-IP 命中）
	up400 := fakeOpenAI(t, "400")
	defer up400.Close()
	store2 := &captureLogStore{}
	p2 := newTestProxyTimeoutLogs(t, up400.URL, 1, store2)
	p2.cfg.BehindCDN = true
	req2 := chatReq("ck-1")
	req2.Header.Set("X-Real-IP", "7.7.7.7")
	rec2 := httptest.NewRecorder()
	p2.HandleChat(rec2, req2)
	require.Equal(t, http.StatusBadRequest, rec2.Code)
	require.NoError(t, p2.rec.Close(context.Background()))
	require.NoError(t, p2.errlog.Close(context.Background()))

	store2.mu.Lock()
	defer store2.mu.Unlock()
	require.Len(t, store2.logs, 1)
	require.Equal(t, "7.7.7.7", store2.logs[0].ClientIP, "on → err_logs 行带 client_ip")
}

// off：伪造头被忽略（恒 RemoteAddr 剥端口——httptest 默认 192.0.2.1:1234）。
func TestProxyClientIPOffIgnoresForgedHeaders(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store) // BehindCDN 缺省 false

	req := chatReq("ck-1")
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")
	req.Header.Set("X-Real-IP", "7.7.7.7")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "192.0.2.1", store.logs[0].ClientIP, "off：伪造头被忽略，恒 RemoteAddr 剥端口")
}

// on 无头 → RemoteAddr 值（兜底）。
func TestProxyClientIPBehindCDNNoHeaderFallback(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	p.cfg.BehindCDN = true

	rec := httptest.NewRecorder()
	p.HandleChat(rec, chatReq("ck-1"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "192.0.2.1", store.logs[0].ClientIP, "on 无头 → RemoteAddr 剥端口")
}

// 不变量（gate M1）：401 鉴权失败（rm 创建在鉴权前）+ 429 并发超限 + 组限流
// ——全部拒绝路径 err_logs 行恒带 client_ip（提取在鉴权前，recordRejected 的
// ctx 统一带 rm）。成功路径行（无伪造头）同时断言 RemoteAddr 兜底。
func TestProxyClientIPRejectedRowsAlwaysCarryIP(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	p.cfg.BehindCDN = true

	// 401 鉴权失败：错误 key → 拒绝行带 client_ip（M1 关键路径：此前 401 分支
	// 在 rm 创建之前失败返回，ctx 无 rm → recordRejected 行恒无 client_ip）
	req := chatReq("wrong-key")
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 组限流 429：窗口限额 2 → 第 3 请求拒绝（拒绝路径同断言）
	p.cfg.GroupKeyRPM = 2
	p.limit = newFixedWindowLimiter(2)
	for i := 0; i < 2; i++ {
		rc := httptest.NewRecorder()
		p.HandleChat(rc, chatReq("ck-1"))
		require.Equal(t, http.StatusOK, rc.Code)
	}
	rc := httptest.NewRecorder()
	req3 := chatReq("ck-1")
	req3.Header.Set("CF-Connecting-IP", "9.9.9.9")
	p.HandleChat(rc, req3)
	require.Equal(t, http.StatusTooManyRequests, rc.Code, "body=%s", rc.Body.String())
	require.Contains(t, rc.Body.String(), "group rate limited")

	// 429 并发超限：key 并发槽 1 → 第二在途请求拒绝（阻塞上游保持第一在途）
	blockUp, release := blockingUpstream(t)
	defer blockUp.Close()
	store4 := &captureLogStore{}
	p4 := newTestProxyTimeoutLogs(t, blockUp.URL, 1, store4)
	p4.cfg.BehindCDN = true
	meta := activeKey(1, 1, 10)
	meta.KeyMaxConc = 1
	p4.auth.Upsert("ck-1", meta)
	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p4.HandleChat(rec1, chatReq("ck-1"))
		close(done)
	}()
	waitGateKey(t, p4, 1, 1) // 第一请求已占 key 槽
	rec2 := httptest.NewRecorder()
	req2 := chatReq("ck-1")
	req2.Header.Set("CF-Connecting-IP", "9.9.9.9")
	p4.HandleChat(rec2, req2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, "body=%s", rec2.Body.String())
	require.Contains(t, rec2.Body.String(), "concurrency limit exceeded")
	close(release)
	<-done

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	require.NoError(t, p4.rec.Close(context.Background()))
	require.NoError(t, p4.errlog.Close(context.Background()))

	store.mu.Lock()
	byReq := map[domain.ErrorType]string{}
	byStatus := map[int]string{}
	for _, l := range store.logs {
		if l.ErrorType == domain.ErrAuth {
			byReq[l.ErrorType] = l.ClientIP
		}
		if l.ErrorType == domain.Err429 && l.StatusCode == http.StatusTooManyRequests {
			byStatus[l.StatusCode] = l.ClientIP // 组限流拒绝行（成功行 ErrorType=none）
		}
	}
	require.Equal(t, "9.9.9.9", byReq[domain.ErrAuth], "401 拒绝行恒带 client_ip（不变量）")
	require.Equal(t, "9.9.9.9", byStatus[http.StatusTooManyRequests], "组限流拒绝行恒带 client_ip")
	store.mu.Unlock()

	store4.mu.Lock()
	defer store4.mu.Unlock()
	var concIP string
	for _, l := range store4.logs {
		if l.ErrorType == domain.Err429 {
			concIP = l.ClientIP
		}
	}
	require.Equal(t, "9.9.9.9", concIP, "429 并发超限拒绝行恒带 client_ip")
}

// newTestProxyClientIPBilling 计费装配 + behind_cdn=on + errlog 捕获的 402 预检
// 测试代理（复用 billing_test 的 fake 基座；errWriter=store 捕获拒绝行——
// newTestProxyBillingT3Logs 传 nil errWriter 不捕获错误明细，故独立构造）。
func newTestProxyClientIPBilling(t *testing.T, upstream string, bal *billing.Balances, store *captureLogStore) *Proxy {
	t.Helper()
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额不足）")
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutRec(t, tpl, 1, true, 30*time.Second, rec, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: bal,
	}, store)
	p.cfg.BillingCapture = true
	p.cfg.BehindCDN = true
	return p
}

// 402 余额预检拒绝行（spec 测试节"全部拒绝路径 429/402/限流"显式断言补齐）：
// behind_cdn=on + 余额不足（快照负）→ 402 + err_logs 行带 client_ip——precheck
// 分支与 401/429 同构（rm 已创建 + ctx 注入，recordRejected(r.Context()) 恒带）。
func TestProxyClientIPRejectedRow402BalancePrecheck(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: -1}}, nil)
	p := newTestProxyClientIPBilling(t, up.URL, bal, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, http.StatusPaymentRequired, recw.Code, "body=%s", recw.Body.String())
	require.Contains(t, recw.Body.String(), "insufficient balance", "402 文案说明余额不足")

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "402 预用量拒绝必须落一条 err_logs")
	l := store.logs[0]
	require.Equal(t, domain.ErrBilling, l.ErrorType)
	require.Equal(t, http.StatusPaymentRequired, l.StatusCode)
	require.Equal(t, "9.9.9.9", l.ClientIP, "402 余额预检拒绝行恒带 client_ip（不变量）")
}
