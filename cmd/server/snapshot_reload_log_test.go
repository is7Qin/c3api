// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/pkg/logx"
)

// reloadFakeSnap 函数型 fake 快照（caller 日志测试用，与 dispatcher_test 的
// recSnap 区分）：Reload 行为由闭包注入。
type reloadFakeSnap struct {
	name   string
	scopes []string
	calls  atomic.Int64
	reload func(ctx context.Context) error
}

func (f *reloadFakeSnap) Name() string     { return f.name }
func (f *reloadFakeSnap) Scopes() []string { return f.scopes }
func (f *reloadFakeSnap) Reload(ctx context.Context) error {
	f.calls.Add(1)
	return f.reload(ctx)
}

// newJSONFileLogger 写 JSON 行到临时文件（参照 pkg/logx 测试形态）：返回
// logger 与文件路径。Windows 下 zap 进程内持有句柄，目录清理尽力而为。
func newJSONFileLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "snapreload-log-test-")
	require.NoError(t, err)
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("info", out)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logger, out
}

// TestSnapshotReloadLogHelper caller 日志 helper（spec §7.2）：startup 测试直接
// 调用 helper（不启动完整 server）——恒带 snapshot/error 字段；panic 错误经
// snapshot.PanicStackForLog 以 logx.String("stack", ...) 追加结构化字段（不在
// message 内）；raw panic value 不进任何日志输出。
func TestSnapshotReloadLogHelper(t *testing.T) {
	t.Run("panicStackField", func(t *testing.T) {
		reg := snapshot.New()
		boom := &reloadFakeSnap{name: "boom", reload: func(ctx context.Context) error {
			panic("helper-secret-payload")
		}}
		require.NoError(t, reg.Register(boom))
		errs := reg.ReloadAll(context.Background())
		require.Contains(t, errs, "boom")

		log, out := newJSONFileLogger(t)
		logSnapshotReloadErr(log, "snapshot initial reload failed", "boom", errs["boom"])
		require.NoError(t, log.Sync())

		b, err := os.ReadFile(out)
		require.NoError(t, err)
		line := string(b)
		require.Contains(t, line, `"msg":"snapshot initial reload failed"`)
		require.Contains(t, line, `"snapshot":"boom"`)
		require.Contains(t, line, `"error":"snapshot reload panicked"`)
		require.Contains(t, line, `"stack":"`, "panic 错误追加 stack 结构化字段")
		require.NotContains(t, line, "helper-secret-payload", "raw panic value 不进日志")
	})

	t.Run("ordinaryErrorNoStack", func(t *testing.T) {
		log, out := newJSONFileLogger(t)
		logSnapshotReloadErr(log, "snapshot reload failed", "plain", errors.New("plain-boom"))
		require.NoError(t, log.Sync())

		b, err := os.ReadFile(out)
		require.NoError(t, err)
		line := string(b)
		require.Contains(t, line, `"snapshot":"plain"`)
		require.Contains(t, line, `"error":"plain-boom"`)
		require.NotContains(t, line, `"stack"`, "普通错误无 stack 字段")
	})

	t.Run("nilLoggerNoop", func(t *testing.T) {
		require.NotPanics(t, func() {
			logSnapshotReloadErr(nil, "snapshot reload failed", "x", errors.New("e"))
		})
	})
}

// TestDispatcherScopeReloadLogsPanicStack scope reload 失败走 helper：stack 以
// 结构化字段进入 operator 日志（message/Status 不变）。
func TestDispatcherScopeReloadLogsPanicStack(t *testing.T) {
	log, out := newJSONFileLogger(t)
	reg := snapshot.New()
	boom := &reloadFakeSnap{name: "auth", scopes: []string{snapshot.ScopeSettings},
		reload: func(ctx context.Context) error { panic("scope-secret") }}
	require.NoError(t, reg.Register(boom))
	d := &dispatcher{snapshots: reg, log: log}

	d.reloadScopes(context.Background(), snapshot.ScopeSettings)

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	line := string(b)
	require.Contains(t, line, `"msg":"snapshot scope reload failed"`)
	require.Contains(t, line, `"snapshot":"auth"`)
	require.Contains(t, line, `"stack":"`)
	require.NotContains(t, line, "scope-secret")
}

// TestDispatcherFullRefreshLogsEveryItem FullRefresh 逐项记录（spec §7.2）：
// 多个不同错误（panic + 普通 error）每项先经 helper 记录、再保持现有首错返回
// 语义；不断言 map 顺序；raw panic value 不出现。
func TestDispatcherFullRefreshLogsEveryItem(t *testing.T) {
	log, out := newJSONFileLogger(t)
	reg := snapshot.New()
	boom := &reloadFakeSnap{name: "snap-panic", reload: func(ctx context.Context) error {
		panic("full-refresh-secret")
	}}
	fail := &reloadFakeSnap{name: "snap-plain", reload: func(ctx context.Context) error {
		return errors.New("plain-full-refresh")
	}}
	ok := &reloadFakeSnap{name: "snap-ok", reload: func(ctx context.Context) error { return nil }}
	for _, s := range []snapshot.Snapshot{boom, fail, ok} {
		require.NoError(t, reg.Register(s))
	}
	settings := &recSettings2{}
	d := &dispatcher{svc: settings, snapshots: reg, log: log}

	err := d.FullRefresh(context.Background())
	require.Error(t, err, "保持首错返回语义")
	require.Equal(t, 1, settings.calls(), "ReloadSettings 仍执行")

	b, readErr := os.ReadFile(out)
	require.NoError(t, readErr)
	logs := string(b)
	require.Contains(t, logs, `"snapshot":"snap-panic"`, "panic 项已记录")
	require.Contains(t, logs, `"snapshot":"snap-plain"`, "普通 error 项已记录")
	require.Contains(t, logs, `"error":"snapshot reload panicked"`)
	require.Contains(t, logs, `"error":"plain-full-refresh"`)
	require.Contains(t, logs, `"stack":"`)
	require.NotContains(t, logs, "full-refresh-secret")
}

// TestSnapshotStatesProjectionNoStack HTTP projection 隔离（spec §7.2）：管理面
// snapshotStates 只见 Status（ErrReloadPanic 固定摘要），stack 不进 projection。
func TestSnapshotStatesProjectionNoStack(t *testing.T) {
	reg := snapshot.New()
	boom := &reloadFakeSnap{name: "boom", reload: func(ctx context.Context) error {
		panic("projection-secret")
	}}
	require.NoError(t, reg.Register(boom))
	require.NotEmpty(t, reg.ReloadAll(context.Background()))

	states := snapshotStates(reg.Status())
	require.Len(t, states, 1)
	require.NotNil(t, states[0].LastError)
	require.Equal(t, "snapshot reload panicked", *states[0].LastError)
	require.NotContains(t, *states[0].LastError, "projection-secret")
}
