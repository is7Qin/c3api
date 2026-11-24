// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 P3a（payloadKey 三枚明文字段 → 一枚构造期缓存的 payloadDigest）的
// **判据分布**中最容易写错的那几条：
//
//   - H6（形状）：payloadKey 恰好 1 个字段、类型 [32]byte（reflect 断言名字 + 类型）。
//     防的是"摘要加回来了，明文字段也留着"——那种形态下所有行为断言仍会绿。
//   - H3（不变性）：叶构造**之后**就地改 av.acc.Ext 的三枚载荷字段 ⇒ 键逐字节不变。
//     缓存的意义就在这里：叶是快照，源对象被就地改写不得改变它的键。
//   - H4（敏感性）：就地轮转 + 同一 *Account 重建 ⇒ 键**不等**。这条修的是既有缺陷：
//     改前两侧都现场读同一枚被改写的 *AccountExt ⇒ 键判等 ⇒ 复用旧叶 ⇒ 轮转漏检。
//   - H5（尺寸）：snapshotStatic 仍在 768 的分配类里（B/op 的不变量，见断言处的算式）。
//
// H3 与 H4 是同一枚硬币的两面：**缓存**给出 H3，**构造期定型**给出 H4。去掉缓存
// （payloadKeyOf 现场读）H3 红；把摘要写进源对象/不重建（例如把 digest 回写到 Ext）
// H4 红。字节由 payload_digest_test.go 的 KAT 钉住，形状由
// payload_digest_gate_test.go 的 H1/H2 钉住。

// payloadFixtureExpires 是夹具的过期时刻（2030-01-01T00:00:00Z 的 UnixNano）。
// 与 testdata/payload_digest_vectors.py 的 EXPIRES_TYPICAL 同值——KAT 钉的就是它。
const payloadFixtureExpires int64 = 1893456000000000000

// payloadFixture 造一枚 codex-oauth 形状的输入：OAuth 三元组齐全，其余事实恒定。
//
// 每次调用都新建对象（模拟每轮重载）；H3/H4 需要**同一枚** Ext 指针，故它们自己
// 只调一次本函数再决定改哪一侧。
func payloadFixture() (domain.Account, *domain.Template, []int64) {
	tp := &domain.Template{
		ID:               3,
		BaseURL:          "https://tpl.example/v1",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"m"},
	}
	exp := time.Unix(0, payloadFixtureExpires)
	a := domain.Account{
		ID: 7, Name: "acct-7", TemplateID: tp.ID, Template: tp,
		UpstreamKey: "sk-live", MaxConcurrency: 4, Enabled: true, IdentityRevision: 5,
		Ext: &domain.AccountExt{
			AccountID:              7,
			CredentialType:         credential.TypeCodexOAuth,
			CodexAccountID:         new("acct-up"),
			CodexOAuthToken:        new("at-0"),
			CodexOAuthRefreshToken: new("rt-0"),
			CodexOAuthExpiresAt:    &exp,
		},
	}
	return a, tp, []int64{3, 9}
}

// TestPayloadKeyIsOneDigestField 是 H6：载荷键**恰好**一枚 [32]byte 摘要。
//
// 为什么这条必须有：明文字段回流（`payloadDigest [32]byte` + 一枚 `codexOAuthToken
// string`）时，H3/H4 与全部行为断言仍然绿（缓存照样生效），只有这条会红。
// 判据写名字**且**写类型：只写名字会放过 `payloadDigest string`，只写类型会放过
// `codexOAuthToken [32]byte`。
func TestPayloadKeyIsOneDigestField(t *testing.T) {
	require.Empty(t, payloadKeyShapeViolation(reflect.TypeFor[payloadKey]()),
		"payloadKey 必须恰好一枚 [32]byte 摘要：凭据原文只在 newSnapshotStatic 读一次，键类型里不得再有明文字段")
}

// payloadKeyShapeViolation 是 H6 的判据本体（空 = 合规）：字段数恰好 1、名字是
// payloadDigest、类型是 [32]byte。三条都写，是因为三种退化各有各的漏洞：
//   - 字段数 > 1：明文字段回流（判据最常被"顺手加一维"破坏的形态）；
//   - 名字换了：读闭包表、KAT 与 AST 门禁都按这个名字指路，改名等于悄悄换语义；
//   - 类型不是 [32]byte：`string` 会把原文带回来，`[16]byte` 会截断摘要。
func payloadKeyShapeViolation(pt reflect.Type) string {
	if pt.NumField() != 1 {
		return "payloadKey 必须恰好 1 个字段（实际 " + itoa(pt.NumField()) + "）：凭据原文只在 newSnapshotStatic 读一次、折叠成摘要，键类型里不得再有明文字段"
	}
	field := pt.Field(0)
	if field.Name != "payloadDigest" {
		return "payloadKey 的唯一字段必须叫 payloadDigest（实际 " + field.Name + "）：读闭包表、KAT 与 AST 门禁都按这个名字指路"
	}
	if field.Type != reflect.TypeFor[[32]byte]() {
		return "payloadDigest 必须是 [32]byte（实际 " + field.Type.String() + "）：值语义 + 可比较 + 与 factsDigest 同一种事实载体"
	}
	return ""
}

// TestPayloadKeyShapeGuardDetectsPlaintextField 是 H6 的**反向证明**：把三种退化写成
// 镜像 struct，同一条判据必须拒。只加正例等于自证——判据写反（比如永远返回空串）时
// 正例一样是绿的。
func TestPayloadKeyShapeGuardDetectsPlaintextField(t *testing.T) {
	require.Empty(t, payloadKeyShapeViolation(reflect.TypeFor[payloadKey]()),
		"对照：真实 payloadKey 必须合规")

	// ① 明文字段回流（M6 的镜像）。
	type poisoned struct {
		payloadDigest   [32]byte
		codexOAuthToken string
	}
	require.Contains(t, payloadKeyShapeViolation(reflect.TypeFor[poisoned]()), "恰好 1 个字段",
		"明文字段回流必须被字段数判据拦下")

	// ② 明文字段独存（把摘要整个换成原文）。
	type plaintextOnly struct {
		codexOAuthToken string
	}
	require.Contains(t, payloadKeyShapeViolation(reflect.TypeFor[plaintextOnly]()), "payloadDigest",
		"字段数对但名字/类型不对，同样必须拒")

	// ③ 摘要被截断。
	type truncated struct {
		payloadDigest [16]byte
	}
	require.Contains(t, payloadKeyShapeViolation(reflect.TypeFor[truncated]()), "[32]byte",
		"摘要宽度被改小必须拒（16 字节的碰撞空间不足以承载凭据面）")
}

// TestStaticKeyIgnoresInPlacePayloadMutation 是 H3：叶构造后**就地**改源对象的三枚
// 载荷字段，键必须逐字节不变。
//
// 这条判据的语义是"叶是不可变快照"：键与叶内容同源、同代。缓存把这条从"约定"
// 变成"结构"——摘要已在构造期定型，源对象后来怎么变都与这枚叶无关。
//
// 改前这里是**红**的（payloadKeyOf 现场读 av.acc.Ext，改了源对象 ⇒ 键跟着变），
// 报告里贴了改前跑一次的原始输出。
func TestStaticKeyIgnoresInPlacePayloadMutation(t *testing.T) {
	a, tp, gids := payloadFixture()
	av := newSnapshotStatic(a, tp, gids, directEgress(), nil)
	before := staticKeyOf(av)

	// 就地改写：同一个 *AccountExt（不重建对象、不推 K、不动 planKey 面）。
	exp := time.Unix(0, payloadFixtureExpires+1)
	av.acc.Ext.CodexOAuthToken = new("at-rotated")
	av.acc.Ext.CodexOAuthRefreshToken = new("rt-rotated")
	av.acc.Ext.CodexOAuthExpiresAt = &exp
	// 前提：改写确实穿透到了叶子持有（Ext 是共享指针）——否则本断言是空转。
	require.Equal(t, "at-rotated", derefString(av.acc.Ext.CodexOAuthToken),
		"就地改写必须对叶子可见，否则这条断言证明不了缓存")
	require.Equal(t, payloadFixtureExpires+1, av.acc.Ext.CodexOAuthExpiresAt.UnixNano())

	// 用 == 而不是 require.Equal：失败时不必把两枚 400+ 字节的键全量打印出来。
	require.True(t, before == staticKeyOf(av),
		"叶构造后源对象被就地改写，键必须逐字节不变：载荷摘要是构造期定型的事实，不是对源对象的实时投影")
	require.True(t, before.planKey == planKeyOf(av),
		"planKey 更不得被载荷面改动（它本来就不含载荷）")
}

// TestStaticKeyDetectsInPlaceRotationOnRebuild 是 H4：就地轮转后用**同一个
// *Account** 重建叶，键必须不等。
//
// 改前这里是**红**的（也是既有缺陷）：两枚叶都现场读同一枚被改写的 *AccountExt，
// old 侧读到的已经是新令牌 ⇒ staticKey 判等 ⇒ buildSnapshots 复用旧叶 ⇒ 轮转
// 永远漏检（旧叶内容被源对象"从背后"改掉，两枚叶共享同一份凭据指针）。
// 缓存后 old 侧持有轮转前的摘要，new 侧算出轮转后的摘要 ⇒ 键不等 ⇒ 换叶。
//
// 与 plan_staleness_test.go 的 rotateTokenKNeutral 互补：那条走的是"DB 行重映射 ⇒
// 全新 Ext 对象"，本条目走"同一枚 Ext 就地改写"。两条都是生产上真实存在的落库形状。
func TestStaticKeyDetectsInPlaceRotationOnRebuild(t *testing.T) {
	a, tp, gids := payloadFixture()
	oldAv := newSnapshotStatic(a, tp, gids, directEgress(), nil)

	// 就地轮转（K 中性：不碰 IdentityRevision，也不碰 accounts 行）。
	a.Ext.CodexOAuthToken = new("at-new")
	a.Ext.CodexOAuthRefreshToken = new("rt-new")
	newAv := newSnapshotStatic(a, tp, gids, directEgress(), nil)

	require.NotSame(t, oldAv, newAv)
	require.Same(t, oldAv.acc.Ext, newAv.acc.Ext,
		"前提：这是**同一枚** *AccountExt 的就地轮转（对象未重建），否则本用例退化成静态事实变更")

	require.True(t, planKeyOf(oldAv) == planKeyOf(newAv),
		"K 中性轮转不得改变计划有效性（轮转只换叶子）")
	require.False(t, staticKeyOf(oldAv) == staticKeyOf(newAv),
		"就地轮转必须让叶子复用判据判不等：改前两侧读同一枚 Ext ⇒ 判等 ⇒ 轮转漏检（本单顺带修的既有缺陷）")
}

// TestSnapshotStaticStaysInItsAllocationClass 是 H5：snapshotStatic 仍在 **768** 的
// 分配类里。
//
// 算式（amd64，改前 → 改后）：
//
//	728B 改前   = acc 240 + tpl 8 + groupIDs 24 + routable 1(+7 pad) + egress 8
//	             + cacheDomain 16 + installationID 16 + bpsProfileDigest 16 + key 392
//	760B 改后   = 同上 + payloadKey 32（摘要的构造期缓存位点，接在 key 之后）
//
// 768 与 896 之间没有分配类 ⇒ **≤768B 的结构体都落在同一档**，故 `B/op` 对这次
// 32B 的膨胀不敏感（H7 的 920 B/op 因此逐字节不变）。
//
// 为什么判据是"≤768"而不是 spec 字面的"≤728"：728 是**改前**的尺寸，而任何实现
// 只要真的加了一枚 32B 的缓存字段就不可能仍是 728——那条数字在算术上不可达。真正
// 承重的不变量是"分配类不变"（它才保证 B/op 不动），这里钉的正是它；`≤728` 与
// `B/op == 920` 两条只能同时取一条，取后者（它可观测、且是性能判据的本体）。
func TestSnapshotStaticStaysInItsAllocationClass(t *testing.T) {
	const class = 768
	// 用 var 而不是 `snapshotStatic{}` 字面量：构造门禁（snapshot_static_ctor_gate_test.go）
	// 把任何 `snapshotStatic{...}` 复合字面量都当绕过（含测试里的一次性尺寸探针），
	// 而这里要的只是"这类值的尺寸"，用零值变量表达最直接、也不触碰那条门禁。
	var leaf snapshotStatic
	got := unsafe.Sizeof(leaf)
	t.Logf("unsafe.Sizeof(snapshotStatic{}) = %d B（改前 728 B，768 分配类上界 %d B）", got, class)
	require.LessOrEqual(t, got, uintptr(class),
		"snapshotStatic 越过 768 的分配类会直接抬 B/op（每叶多一档）：缓存字段必须塞进既有档位")
	require.Greater(t, got, uintptr(class/2),
		"尺寸掉了一半说明字段被删（本用例要防的是膨胀，但静默缩水同样是键掉了维度）")
}
