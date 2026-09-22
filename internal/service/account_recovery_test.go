// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"os"
	"strconv"
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

// TestRecoverAccountAuditLog 失效恢复审计（日志面）：fenced recover 清失效三
// 字段 → Info 留痕含 account_id + 新 revision；PUT 全量更新不是恢复入口——
// 不触碰失效字段、不留恢复痕迹。
func TestRecoverAccountAuditLog(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	_, err := fs.CreateTemplate(ctx, &domain.Template{ID: 1, Name: "template", CredentialType: "api_key"})
	require.NoError(t, err)
	failed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	created, err := fs.CreateAccount(ctx, &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		MaxConcurrency: 4, Enabled: true, FailedAt: &failed, LifecycleRevision: 1})
	require.NoError(t, err)

	logger, read := recoveryTestLogger(t)
	svc := &Service{store: fs, inv: &invRecorder{}, log: logger}

	// 改名经唯一写点 → 失效字段保持、无恢复留痕（恢复唯一入口 = fenced recover）
	_, err = svc.PatchAccount(ctx, created.ID, repository.AccountPatch{Name: strPtr("renamed")}, nil)
	require.NoError(t, err)
	require.NotContains(t, read(), "account recovered", "改名不是恢复入口，不留痕")

	// fenced recover → 清失效 + 审计留痕（配置写入已推进 C，故按新 C 恢复）
	afterRename, err := svc.GetAccount(ctx, created.ID)
	require.NoError(t, err)
	got, err := svc.RecoverAccount(ctx, created.ID, afterRename.LifecycleRevision)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt, "recover 清 failed_at")
	logs := read()
	require.Contains(t, logs, "account recovered", "恢复操作审计留痕")
	require.Contains(t, logs, `"account_id":`+strconv.FormatInt(created.ID, 10), "审计含 account_id")
}
