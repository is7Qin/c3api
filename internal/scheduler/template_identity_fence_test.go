// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
)

// templateIdentityFixture 搭一个单账号调度器。它的候选指纹可以经**模板侧**改动
// 而改变（strip_image_tools 进候选指纹），同时 K（账号的 identity_revision）与
// 质量类都不变——这正是"模板侧身份变化"的形状：身份变了，代际没变。
func templateIdentityFixture(t *testing.T) (*Scheduler, *memLoader) {
	t.Helper()
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	return newSched(t, m), m
}

// flipTemplateIdentity 模拟一次模板侧身份改动：换上一个**新的**模板对象（不就地
// 改旧对象——旧快照的不可变性正依赖它），翻转 strip_image_tools，然后走生产的
// 失效路径重载 + 重编译。
func flipTemplateIdentity(t *testing.T, s *Scheduler, m *memLoader) {
	t.Helper()
	m.mu.Lock()
	old := m.byGroup[10][0].Template
	next := *old
	next.StripImageTools = !old.StripImageTools
	m.byGroup[10][0].Template = &next
	m.mu.Unlock()
	s.InvalidateAccount(1)
	s.compileOnce()
}

// currentFingerprint 取账号在当前视图下的候选身份指纹。
func currentFingerprint(t *testing.T, s *Scheduler, id int64) string {
	t.Helper()
	av := s.View().static.byID[id].static.Load()
	fp, err := candidateFingerprint(&av.acc)
	require.NoError(t, err)
	return fp
}

// TestTemplateIdentityChangeVoidsHealthVerdict 是"模板侧身份变化必须作废在途
// 工件"的第一段：健康判决按 (账号, 质量类, 身份指纹, K) 落键，所以模板改动
// 改变指纹而 K 不变时，旧判决对新身份不可达——效果等同无记录，即 READY。
//
// 用例自带反向证明：**同一条**记录，用旧指纹读仍然 OPEN，用新指纹读是 READY。
// 差异只可能来自身份分量，不可能是"记录被清掉了"或"视图没同步"。
func TestTemplateIdentityChangeVoidsHealthVerdict(t *testing.T) {
	s, m := templateIdentityFixture(t)
	qc := qualityClassHexForWithOp(domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	fpOld := currentFingerprint(t, s, 1)

	h := NewRuntimeHealth(nil, "self", nil, nil)
	oldKey := HealthKey{AccountID: 1, Quality: qc, Identity: fpOld, IdentityRevision: 1}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{oldKey: {Key: oldKey, State: StateOPEN}}})
	s.health = h
	require.Equal(t, StateOPEN, h.EffectiveState(1, qc, fpOld, 1), "前提：该判决对当前身份生效")

	flipTemplateIdentity(t, s, m)
	fpNew := currentFingerprint(t, s, 1)
	require.NotEqual(t, fpOld, fpNew, "前提：模板侧改动确实改变了候选身份指纹")
	require.Equal(t, int64(1), s.View().static.byID[1].static.Load().acc.IdentityRevision,
		"前提：模板写入不推进任何账号的 K")

	require.Equal(t, StateReady, h.EffectiveState(1, qc, fpNew, 1),
		"身份已变 ⇒ 旧健康判决不得被复用（miss 的语义是 READY，不是立即重探）")
	require.Equal(t, StateOPEN, h.EffectiveState(1, qc, fpOld, 1),
		"反向证明：同一条记录用旧身份读仍命中 ⇒ 差异来自身份分量，不是记录消失")
}

// TestTemplateIdentityChangeVoidsLatch 是第二段：latch 的读侧谓词是 (指纹, K)，
// 故模板侧身份改动之后，旧锁存不得继续生效。同样自带反向证明。
func TestTemplateIdentityChangeVoidsLatch(t *testing.T) {
	s, m := templateIdentityFixture(t)
	fpOld := currentFingerprint(t, s, 1)
	ls := latch.NewLatchStore()
	s.latch = ls
	require.True(t, ls.TryAcquire(1, fpOld, 1), "前提：旧身份下确实上了锁")

	flipTemplateIdentity(t, s, m)
	fpNew := currentFingerprint(t, s, 1)
	require.NotEqual(t, fpOld, fpNew, "前提：模板侧改动确实改变了候选身份指纹")

	require.False(t, ls.IsLatched(1, fpNew, 1), "身份已变 ⇒ 旧锁存不得继续生效")
	require.True(t, ls.IsLatched(1, fpOld, 1),
		"反向证明：同一条锁存用旧身份读仍命中 ⇒ 谓词确实按指纹判，不是只比账号")
}
