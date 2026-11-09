// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/logx"
)

// codexTransportTpl 构造携带传输开关的 codex 模板：开关只对 codex 号池类型
// 有意义（写面对非 codex 只放行 ""），用例必须在该类型上验证才有判别力。
func codexTransportTpl(id int64, tr domain.CodexResponsesTransport) *domain.Template {
	tp := tpl(id, domain.FormatOpenAIChat, []string{"m"})
	tp.CredentialType = credential.TypeCodexOAuth
	tp.CodexResponsesTransport = tr
	return tp
}

// TestPlanKeyCodexResponsesTransportEquivalence 钉住 planKey 的归一语义
// （spec §5 / §8.2 scheduler 行）：归一**只**发生在 domain 的 Key()。
//   - "" 与 "http" 同键：把留空改成显式 http 是一次空改动，不得换叶换计划；
//   - "ws" 与二者都不同键：换臂必须换新叶子；
//   - 脏值 "WS" 折默认面：与 "" 同键（归一 = 默认面，不是"非法值另开一档"）。
func TestPlanKeyCodexResponsesTransportEquivalence(t *testing.T) {
	base := codexTransportTpl(1, domain.CodexResponsesTransportDefault)
	a := acc(7, base, 4)
	keyFor := func(tr domain.CodexResponsesTransport) staticKey {
		tp := *base
		tp.CodexResponsesTransport = tr
		return staticKeyOf(newSnapshotStatic(*a, &tp, nil, directEgress()))
	}

	def := keyFor(domain.CodexResponsesTransportDefault)
	httpExplicit := keyFor(domain.CodexResponsesTransportHTTP)
	ws := keyFor(domain.CodexResponsesTransportWS)
	dirty := keyFor(domain.CodexResponsesTransport("WS"))

	require.True(t, def == httpExplicit, `"" 与 "http" 必须同键（否则一次空改动就换叶换计划）`)
	require.False(t, ws == def, `"ws" 必须与默认面不同键（否则复用旧叶 ⇒ 开关永远不生效）`)
	require.False(t, ws == httpExplicit, `"ws" 必须与显式 "http" 不同键`)
	require.True(t, dirty == def, `脏值 "WS" 归一为默认面 ⇒ 与 "" 同键`)
}

// TestSelectPicksUpCodexTransportAfterTemplateSwitch 是本批最重要的行为用例
// （spec §8.2「叶复用行为用例」）：走**真实 reserve 路径**，不比较计划结构。
//
// 场景：同一账号先以 tpl.CodexResponsesTransport == "" 建快照并装进视图，
// 再把模板改成 "ws" 重建快照。键里没有该维度时 staticKeyOf 判等 ⇒ 复用旧叶子
// ⇒ reserveOnView 从旧叶子读 tpl ⇒ Selection 仍报 ""（开关静默失效）。
//
// 关键构造：第二次 reload 用**新模板对象**。若原地改同一个 *domain.Template，
// 旧叶子持有的指针也会变成 "ws"，复用分支即便命中也能读到新值——用例会失去
// 判别力（变异验证必须红，靠的就是这里）。
func TestSelectPicksUpCodexTransportAfterTemplateSwitch(t *testing.T) {
	tplx := codexTransportTpl(1, domain.CodexResponsesTransportDefault)
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	before := reuseByID(s, 1)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportDefault, sel.CodexResponsesTransport,
		"未配置 ⇒ Selection 携带零值（默认面 http）")
	sel.Release()

	m.mu.Lock()
	nextTpl := *tplx
	nextTpl.CodexResponsesTransport = domain.CodexResponsesTransportWS
	nextAcc := *m.byGroup[10][0]
	nextAcc.Template = &nextTpl
	m.byGroup[10] = []*domain.Account{&nextAcc}
	m.mu.Unlock()

	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()

	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportWS, sel2.CodexResponsesTransport,
		"模板改成 ws 后，选中必须看到 ws（复用旧叶会让开关永远不生效）")
	sel2.Release()

	// 判别力的第二重证据（不是判据本身）：键里没有该维度时复用分支留下同一片
	// 叶子，上面那条断言就会读到旧 tpl。两者必须同时成立。
	require.NotSame(t, before, reuseByID(s, 1),
		"开关变值必须换新叶子（复用旧叶 = 读到旧 tpl）")
}

// TestSelectionCarriesRawCodexTransport 钉住装配语义：Selection 保存**模板原值**，
// 不做归一（归一在消费方 Key()/Name()）。显式 "http" 不得被折成 ""，否则
// "配置里写的是什么"在响应头/日志里不可辨认。
func TestSelectionCarriesRawCodexTransport(t *testing.T) {
	tplx := codexTransportTpl(1, domain.CodexResponsesTransportHTTP)
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportHTTP, sel.CodexResponsesTransport,
		"显式 http 原样进入 Selection（不归一成零值）")
	sel.Release()
}

// newSchedWithLog 与 newSched 相同，但注入日志器（审计 WARN 的观测面）。
func newSchedWithLog(t *testing.T, m *memLoader, lg *logx.Logger) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil, lg, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// newWarnLogger 建一个只收 WARN 及以上的文件日志器（logx 是业务代码唯一入口，
// 测试同样不 import zap），返回日志器与其输出文件路径。
//
// 目录用 os.MkdirTemp 而不是 t.TempDir：Windows 上 zap 的 sink 文件句柄活到进程
// 结束（没有 Close API），t.TempDir 的强制清理会因此报 unlinkat 失败；清理做成
// best-effort（与 pkg/logx/logx_test.go 同款处理）。
func newWarnLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "scheduler-warn-")
	require.NoError(t, err)
	path := filepath.Join(dir, "scheduler.log")
	lg, err := logx.New("warn", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return lg, path
}

// warnLines 读回日志文件里的 WARN 行（每行一条 JSON 记录）。
func warnLines(t *testing.T, lg *logx.Logger, path string) []string {
	t.Helper()
	require.NoError(t, lg.Sync())
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.Contains(ln, `"level":"warn"`) {
			out = append(out, ln)
		}
	}
	return out
}

// TestWarnDirtyCodexTransportsOncePerTemplate 钉住审计面语义（spec §5「脏值」）：
// 非法值绕过写面落库时，装载侧留一条 WARN，带 template_id 与**原值**；
// 同一模板挂多个账号仍只有一条（不按账号刷屏）；非 codex 模板不报（开关对它
// 永不生效，报了只是噪声）。
func TestWarnDirtyCodexTransportsOncePerTemplate(t *testing.T) {
	lg, path := newWarnLogger(t)
	s := &Scheduler{log: lg}

	dirty := codexTransportTpl(7, domain.CodexResponsesTransport("WS"))
	s.warnDirtyCodexTransports(map[int64][]*domain.Account{
		10: {acc(1, dirty, 4), acc(2, dirty, 4), acc(3, dirty, 4)},
	})
	// 非 codex 模板上的脏值：不报。
	plain := tpl(8, domain.FormatOpenAIChat, []string{"m"})
	plain.CodexResponsesTransport = domain.CodexResponsesTransport("WS")
	s.warnDirtyCodexTransports(map[int64][]*domain.Account{20: {acc(4, plain, 4)}})

	lines := warnLines(t, lg, path)
	require.Len(t, lines, 1, "同一模板多账号只留一条 WARN")
	require.Contains(t, lines[0], `"template_id":7`)
	require.Contains(t, lines[0], `"value":"WS"`, "字段必须是原值，不是归一后的结果")
}

// TestReloadWarnsOnceForDirtyCodexTransport 证明 WARN 真的接在 reload 装配上
// （不是只写了个没人调的函数），且每轮 reload 每模板一条。
func TestReloadWarnsOnceForDirtyCodexTransport(t *testing.T) {
	lg, path := newWarnLogger(t)
	dirty := codexTransportTpl(9, domain.CodexResponsesTransport("WS"))
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, dirty, 4), acc(2, dirty, 4)}})
	newSchedWithLog(t, m, lg)

	lines := warnLines(t, lg, path)
	require.Len(t, lines, 1, "reload 装配时每个 template_id 恰好一条")
	require.Contains(t, lines[0], `"template_id":9`)
	require.Contains(t, lines[0], `"value":"WS"`)
}

// TestReloadSilentOnValidCodexTransports 合法值（""/"http"/"ws"）零 WARN：
// 审计信号只在真脏值上响，正常部署的每次 reload 不得产生噪声。
func TestReloadSilentOnValidCodexTransports(t *testing.T) {
	lg, path := newWarnLogger(t)
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, codexTransportTpl(1, domain.CodexResponsesTransportDefault), 4)},
		20: {acc(2, codexTransportTpl(2, domain.CodexResponsesTransportHTTP), 4)},
		30: {acc(3, codexTransportTpl(3, domain.CodexResponsesTransportWS), 4)},
	})
	newSchedWithLog(t, m, lg)

	require.Empty(t, warnLines(t, lg, path), "合法值不得产生 WARN")
}
