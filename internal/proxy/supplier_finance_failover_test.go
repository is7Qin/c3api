// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// supplier_finance_failover_test.go 端到端验证财务上下文随选中固定
// （spec 2026-10-09 §4.2/§4.6/A19④）：failover 归属取终态尝试的捕获值；
// 转属后视图未换代前不得把流量记到旧 uid（发布屏障，禁 sleep）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestSupplierFinanceTerminalAttemptOwner A→B failover 成功后，计费日志归属取
// **终态尝试 B** 的 fin（200），非首尝试 A（100）。
func TestSupplierFinanceTerminalAttemptOwner(t *testing.T) {
	upA := fakeOpenAI(t, "429") // 首账号失败
	defer upA.Close()
	upB := fakeOpenAI(t, "") // 次账号成功
	defer upB.Close()

	store := &captureLogStore{}
	tplA := &domain.Template{ID: 1, Name: "tA", BaseURL: upA.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	p := newTestProxyTplTimeoutLogs(t, tplA, 1, true, 30*time.Second, store, nil)
	tplB := &domain.Template{ID: 2, Name: "tB", BaseURL: upB.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	sched := p.sched
	loader := sched.Loader().(noopLoader)

	loader.accs[10][0].SupplierUserID = 100
	accB := &domain.Account{ID: 2, TemplateID: 2, Template: tplB, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, SupplierUserID: 200}
	loader.accs[10] = append(loader.accs[10], accB)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	snap := NewSupplierSnapshot(time.Minute)
	snap.Store(map[int64]int64{1: 100, 2: 200}, map[int64]int{100: 10000, 200: 10000}, time.Unix(0, 0))
	p.SetSupplierSnapshot(snap)
	sched.SetSupplierAdmission(snap)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	sched.FlushRules()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "成功路径一条 usage 日志")
	require.Equal(t, int64(200), store.logs[0].SupplierUserID, "归属取终态尝试 B（200），非首尝试 A（100）")
}

// TestSupplierFinanceTransferBarrierNoStaleOwner 转属（A→平台自有）后、财务视图
// 未换代前，平台自有的账号（ownerUID==0）不得抢跑入选——否则会把平台流量记为旧 uid。
// 视图换代（Reload）后放行，按平台自有（uid 0）落账。
func TestSupplierFinanceTransferBarrierNoStaleOwner(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	// 账号 1 调度静态事实为平台自有（SupplierUserID=0）。
	p := newTestProxyTplTimeoutLogs(t, &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}, 1, true, 30*time.Second, store, nil)

	snap := NewSupplierSnapshot(time.Minute)
	// 财务视图仍是旧归属（账号 1 → 100）：发布未换代。
	snap.Store(map[int64]int64{1: 100}, map[int64]int{100: 10000}, time.Unix(0, 0))
	p.SetSupplierSnapshot(snap)
	p.sched.SetSupplierAdmission(snap)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.NotEqual(t, 200, rec.Code, "视图未换代：平台自有账号不得抢跑（应无可用候选）")
	require.Zero(t, p.rec.Pending(), "屏障期不得产生用量记录")
	store.mu.Lock()
	require.Empty(t, store.logs, "屏障期不得把流量记到旧 uid")
	store.mu.Unlock()

	// 视图换代：旧归属消失 ⇒ 平台自有放行，按 uid 0 落账（无收益）。
	snap.Store(map[int64]int64{}, map[int64]int{}, time.Unix(1, 0))
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req2)
	require.Equal(t, 200, rec2.Code, "body=%s", rec2.Body.String())
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(0), store.logs[0].SupplierUserID, "换代后按平台自有（uid 0）落账")
}
