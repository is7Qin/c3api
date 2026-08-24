// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// blockingUpstream 阻塞式上游：直到 release 通道放行才响应（测并发门禁用）。
func blockingUpstream(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": "gpt-4o",
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	return up, release
}

// waitGateKey 轮询等待 key 并发计数达到 n（在途请求占槽断言）。
func waitGateKey(t *testing.T, p *Proxy, keyID int64, n int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap := p.auth.gate.store.Load()
		c, ok := snap.keys[keyID]
		return ok && c.Load() == n
	}, 3*time.Second, 10*time.Millisecond)
}

// waitGateUser 轮询等待 user 并发计数达到 n（用户级门禁断言）。
func waitGateUser(t *testing.T, p *Proxy, userID int64, n int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap := p.auth.gate.store.Load()
		c, ok := snap.users[userID]
		return ok && c.Load() == n
	}, 3*time.Second, 10*time.Millisecond)
}

func chatReq(key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// 并发门禁 429（key 级）：上游停滞保持请求在途 → 第二请求 429
// "concurrency limit exceeded"；释放后计数归零、请求可恢复。
func TestProxyConcurrencyLimit429(t *testing.T) {
	up, release := blockingUpstream(t)
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	meta := activeKey(1, 1, 10)
	meta.KeyMaxConc = 1
	p.auth.Upsert("ck-1", meta)

	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.HandleChat(rec1, chatReq("ck-1"))
		close(done)
	}()
	waitGateKey(t, p, 1, 1) // 第一请求已占 key 槽

	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, chatReq("ck-1"))
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, "body=%s", rec2.Body.String())
	require.Contains(t, rec2.Body.String(), "concurrency limit exceeded")

	close(release)
	<-done
	require.Equal(t, http.StatusOK, rec1.Code)
	waitGateKey(t, p, 1, 0)

	// 释放后可恢复
	rec3 := httptest.NewRecorder()
	p.HandleChat(rec3, chatReq("ck-1"))
	require.Equal(t, http.StatusOK, rec3.Code, "释放后可恢复: %s", rec3.Body.String())
	waitGateKey(t, p, 1, 0)
}

// TestProxyRejectionStormNoPending P2a 源头修复（压测 2026-08-11 复测）：本地
// 预用量拒绝（并发超限 429 等）不产生 usage_logs 明细/pending——单 key 限流
// 161k req/s 的拒绝风暴（实证 60s → 9.8M pending 行 / RSS 7.5GB，全部漏斗到
// 单用户 billed flusher）不再无界积压；拒绝统计计数由离线聚合 worker 兜底
// （spec 2026-08-14 请求路径零统计）。billing on/off 双形态（on 形态修复前
// 429 与成功请求同走 billed flusher）。
func TestProxyRejectionStormNoPending(t *testing.T) {
	t.Run("billing off", func(t *testing.T) {
		up, release := blockingUpstream(t)
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
		meta := activeKey(1, 1, 10)
		meta.KeyMaxConc = 1
		p.auth.Upsert("ck-1", meta)

		rec1 := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { p.HandleChat(rec1, chatReq("ck-1")); close(done) }()
		waitGateKey(t, p, 1, 1) // 第一请求占住唯一并发槽

		const storm = 300 // 风暴：并发超限 429
		for i := 0; i < storm; i++ {
			recw := httptest.NewRecorder()
			p.HandleChat(recw, chatReq("ck-1"))
			require.Equal(t, http.StatusTooManyRequests, recw.Code, "body=%s", recw.Body.String())
		}
		require.Zero(t, p.rec.Pending(), "429 风暴不产生 Recorder 明细 pending（修复前 300 条全进 pending）")

		close(release)
		<-done
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		require.Len(t, store.logs, 1, "仅成功请求产生 usage_logs 明细（429 拒绝零 usage_logs 明细）")
		store.mu.Unlock()
		// err_logs 为拒绝审计的独立落盘面（有界队列背压采样）：风暴 300 条全量
		// 入队（容量内不丢）——与 usage_logs 零明细互斥成立（分表彻底化）
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1+storm, "429 拒绝全部进 err_logs（错误审计明细）")
	})

	t.Run("billing on", func(t *testing.T) {
		up, release := blockingUpstream(t)
		defer up.Close()
		store := &captureLogStore{}
		rec := usage.New(usage.UsageConfig{
			BatchSize: 100, FlushInterval: time.Hour,
			QuotaFlushInterval: time.Hour,
		}, store, nil)
		bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
		require.NoError(t, bal.Reload(context.Background()))
		p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}, variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()}}, bal, rec)
		meta := activeKey(1, 1, 10)
		meta.KeyMaxConc = 1
		p.auth.Upsert("ck-1", meta)

		rec1 := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { p.HandleChat(rec1, chatReq("ck-1")); close(done) }()
		waitGateKey(t, p, 1, 1)

		const storm = 300 // 风暴：并发超限 429（UserID>0 → 修复前全部漏斗到 billed flusher）
		for i := 0; i < storm; i++ {
			recw := httptest.NewRecorder()
			p.HandleChat(recw, chatReq("ck-1"))
			require.Equal(t, http.StatusTooManyRequests, recw.Code, "body=%s", recw.Body.String())
		}
		require.Zero(t, p.rec.Pending(), "429 风暴不产生 Recorder 明细 pending")

		close(release)
		<-done
		require.NoError(t, rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "仅成功请求落 usage_logs 单写点（429 风暴零记录——修复前 9.8M 行实证）")
	})
}

// 用户级并发：同一用户两个 key 共享 user 上限（跨 key 计数）。
func TestProxyUserConcurrencyAcrossKeys(t *testing.T) {
	up, release := blockingUpstream(t)
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	meta1 := activeKey(1, 1, 10)
	meta1.UserMaxConc = 1
	meta2 := activeKey(2, 1, 10)
	meta2.UserMaxConc = 1
	p.auth.Upsert("ck-1", meta1)
	p.auth.Upsert("ck-2", meta2)

	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.HandleChat(rec1, chatReq("ck-1"))
		close(done)
	}()
	waitGateUser(t, p, 1, 1) // 用户槽被第一请求占用（key 级无上限，只看 user）

	// 第二个 key（同用户）→ user 层超限 429
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, chatReq("ck-2"))
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, "body=%s", rec2.Body.String())
	require.Contains(t, rec2.Body.String(), "concurrency limit exceeded")

	close(release)
	<-done
	waitGateUser(t, p, 1, 0)
}

// quota 门禁：后扣模型——两轮请求累计（每轮 total_tokens=2... 用 usage 断言
// 8？见 fakeOpenAI）后超限 → 下一请求 429 "key quota exhausted"。
func TestProxyQuotaExhaustedAndDeduct(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	meta := activeKey(1, 1, 10)
	meta.HasQuota = true
	meta.Quota = 10 // fakeOpenAI 非流式 total_tokens=8
	p.auth.Upsert("ck-1", meta)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		p.HandleChat(rec, chatReq("ck-1"))
		require.Equal(t, http.StatusOK, rec.Code, "第 %d 轮: %s", i+1, rec.Body.String())
	}
	// 内存已扣 16 ≥ 10 → 429
	rec := httptest.NewRecorder()
	p.HandleChat(rec, chatReq("ck-1"))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "key quota exhausted")
	// 429 不计入扣减（检查在 acquire 前，纯读）
	snap := p.auth.gate.store.Load()
	require.Equal(t, int64(16), snap.quotas[1].consumed.Load())
}

// 无额度 key：无 quota 条目、零扣减（恒 0）、不误拒。
func TestProxyNoQuotaKeyZeroCost(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1) // activeKey 默认 HasQuota=false

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		p.HandleChat(rec, chatReq("ck-1"))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	snap := p.auth.gate.store.Load()
	_, ok := snap.quotas[1]
	require.False(t, ok, "无额度 key 无 quota 条目（扣减路径与现状相同）")
}

// key 禁用即时失效（Auth 快照）：禁用后既有 key 401。
func TestProxyKeyDisableImmediate(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	rec := httptest.NewRecorder()
	p.HandleChat(rec, chatReq("ck-1"))
	require.Equal(t, http.StatusOK, rec.Code, "禁用前正常")

	meta := activeKey(1, 1, 10)
	meta.KeyStatus = domain.KeyStatusDisabled
	p.auth.Upsert("ck-1", meta)

	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, chatReq("ck-1"))
	require.Equal(t, http.StatusUnauthorized, rec2.Code, "key 禁用即时 401: %s", rec2.Body.String())
}

// 用户禁用即时失效：key 快照携带 UserStatus → 401。
func TestProxyUserDisableImmediate(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	meta := activeKey(1, 1, 10)
	meta.UserStatus = domain.UserStatusDisabled
	p.auth.Upsert("ck-1", meta)

	rec := httptest.NewRecorder()
	p.HandleChat(rec, chatReq("ck-1"))
	require.Equal(t, http.StatusUnauthorized, rec.Code, "用户禁用即时 401: %s", rec.Body.String())
}

// user_id/key_id 入日志（context 传递）：成功请求的日志带鉴权归属。
func TestProxyUsageLogCarriesUserAndKey(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	rec := httptest.NewRecorder()
	p.HandleChat(rec, chatReq("ck-1"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(1), store.logs[0].UserID, "user_id 来自鉴权 KeyMeta（context 传递）")
	require.Equal(t, int64(1), store.logs[0].KeyID, "key_id 来自鉴权 KeyMeta")
}

// 401（鉴权失败）路径：无 KeyMeta → user_id/key_id 保持 0。分表：401 → err_logs
// 拒绝行（A1 归因缺口接受：无归属，注释标注）。
func TestProxyUsageLogAuthFailureNoOwner(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := chatReq("wrong-key")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Zero(t, store.logs[0].UserID, "鉴权失败无归属")
	require.Zero(t, store.logs[0].KeyID)
	require.Equal(t, domain.ErrAuth, store.logs[0].ErrorType, "401 记 ErrAuth（err_logs 拒绝行）")
}

// reload 继承（端到端）：在途请求跨 Reload 不丢计数，释放后归零。
func TestProxyGateReloadInherits(t *testing.T) {
	up, release := blockingUpstream(t)
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	meta := activeKey(1, 1, 10)
	meta.KeyMaxConc = 2
	p.auth.Upsert("ck-1", meta)

	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.HandleChat(rec1, chatReq("ck-1"))
		close(done)
	}()
	waitGateKey(t, p, 1, 1)

	// 全量 reload（用户变更等触发）：在途计数继承
	require.NoError(t, p.auth.Reload(context.Background()))
	snap := p.auth.gate.store.Load()
	require.Equal(t, int64(1), snap.keys[1].Load(), "reload 继承在途值")

	close(release)
	<-done
	waitGateKey(t, p, 1, 0)
}
