// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// recoveryTestLogger 审计日志面替身：os.Stdout 重定向到临时文件后构造 logx
// （zap stdout sink 构造时捕获 os.Stdout 值——sink.go newFileSinkFromPath），
// Sync 排空后读回断言操作留痕。
func recoveryTestLogger(t *testing.T) (*logx.Logger, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "audit*.log")
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = f
	logger, err := logx.New("info", "stdout")
	require.NoError(t, err)
	read := func() string {
		require.NoError(t, logger.Sync()) // 排空 zap 缓冲
		b, rerr := os.ReadFile(f.Name())
		require.NoError(t, rerr)
		return string(b)
	}
	t.Cleanup(func() {
		os.Stdout = old
		_ = f.Close()
	})
	return logger, read
}

// TestUpdateAccountRecoveryAuditLog 失效恢复审计（T5 §4 日志面）：管理面
// status→active 且账号此前已失效（failed_at 置位）→ 恢复操作日志留痕（单
// 条 Info 含 account_id）；未失效账号置 active → 不留痕（恢复动作才审计）。
func TestUpdateAccountRecoveryAuditLog(t *testing.T) {
	fs := newFakeStore()
	failed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	a := &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		Status: domain.StatusActive, Weight: 1, MaxConcurrency: 4, FailedAt: &failed}
	created, err := fs.CreateAccount(nil, a)
	require.NoError(t, err)
	healthy := &domain.Account{Name: "h", TemplateID: 1, UpstreamKey: "sk-h",
		Status: domain.StatusActive, Weight: 1, MaxConcurrency: 4}
	h2, err := fs.CreateAccount(nil, healthy)
	require.NoError(t, err)

	logger, read := recoveryTestLogger(t)
	svc := &Service{store: fs, inv: &invRecorder{}, log: logger}

	// 已失效账号 status→active → 恢复审计留痕
	cur, err := svc.GetAccount(nil, created.ID)
	require.NoError(t, err)
	cur.Status = domain.StatusActive
	_, err = svc.UpdateAccount(nil, cur)
	require.NoError(t, err)
	logs := read()
	require.Contains(t, logs, "account failure cleared (status->active)", "恢复操作审计留痕")
	require.Contains(t, logs, `"account_id":`+strconv.FormatInt(created.ID, 10), "审计含 account_id")

	// 未失效账号 status→active → 不留痕（无恢复动作）
	cur2, err := svc.GetAccount(nil, h2.ID)
	require.NoError(t, err)
	cur2.Status = domain.StatusDisabled
	_, err = svc.UpdateAccount(nil, cur2)
	require.NoError(t, err)
	cur3, err := svc.GetAccount(nil, h2.ID)
	require.NoError(t, err)
	cur3.Status = domain.StatusActive
	_, err = svc.UpdateAccount(nil, cur3)
	require.NoError(t, err)
	logs2 := read()
	require.Equal(t, 1, strings.Count(logs2, "account failure cleared (status->active)"),
		"仅真实恢复动作留痕一次")
}

// TestUpdateAccountsBatchRecoveryAuditLog 批量恢复审计（T5 §4 日志面）：批量
// status→active → 操作级恢复留痕（批量路径不做逐账号旧值比较——含 count）。
func TestUpdateAccountsBatchRecoveryAuditLog(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.CreateAccount(nil, &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		Status: domain.StatusActive, Weight: 1, MaxConcurrency: 4})
	require.NoError(t, err)
	_, err = fs.CreateAccount(nil, &domain.Account{Name: "b", TemplateID: 1, UpstreamKey: "sk-b",
		Status: domain.StatusActive, Weight: 1, MaxConcurrency: 4})
	require.NoError(t, err)

	logger, read := recoveryTestLogger(t)
	svc := &Service{store: fs, inv: &invRecorder{}, log: logger}

	st := domain.StatusActive
	require.NoError(t, svc.UpdateAccountsBatch(nil, []int64{1, 2}, repository.AccountPatch{Status: &st}))
	logs := read()
	require.Contains(t, logs, "account failure cleared (batch status->active)", "批量恢复操作审计留痕")
	require.Contains(t, logs, `"count":2`, "审计含批量 count")

	// 非 active 批量 → 不留痕
	st2 := domain.Status429
	require.NoError(t, svc.UpdateAccountsBatch(nil, []int64{1}, repository.AccountPatch{Status: &st2}))
	logs2 := read()
	require.Equal(t, 1, strings.Count(logs2, "account failure cleared (batch status->active)"))
}
