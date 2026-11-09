// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/templateext"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// codex responses 出站传输开关（spec-codex-outbound-transport §4.1–§4.4）真实 PG
// 测试：列读写三态 + 全列替换清空 + 快照合并矩阵（含独立 if 门控——strip=NULL
// 行必须也合并）+ 陈旧 ext 行不合并。
// 基座见 pg_account_groups_test.go 的 newPGReposShared（逐测试克隆库）。
// ---------------------------------------------------------------------------

// seedPGCodexTemplate 建 codex 类型模板（merge 只对 codex 类型生效）。
func seedPGCodexTemplate(t *testing.T, repos *repository.Repository, name string) *domain.Template {
	t.Helper()
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

// snapshotTransportOf 从 LoadGroupsAccounts 全量快照取账号模板合并后的出站传输。
func snapshotTransportOf(t *testing.T, repos *repository.Repository, groupID, accountID int64) (domain.CodexResponsesTransport, bool) {
	t.Helper()
	m, err := repos.Groups.LoadGroupsAccounts(context.Background())
	require.NoError(t, err)
	for _, a := range m[groupID] {
		if a.ID == accountID && a.Template != nil {
			return a.Template.CodexResponsesTransport, true
		}
	}
	return domain.CodexResponsesTransportDefault, false
}

// TestTemplateExtCodexTransportPG 列读写：""/"http"/"ws" 三态 roundtrip +
// 全列替换语义（缺省 / 空串 ⇒ 落 ""，非可空列不 Clear）+ 幂等 upsert 单行。
func TestTemplateExtCodexTransportPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-transport")

	// 未配置 = 零值 ""（NOT NULL DEFAULT ''：插入路径不显式 Set 也不为 NULL）
	saved := seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
	})
	require.Equal(t, domain.CodexResponsesTransportDefault, saved.CodexResponsesTransport)

	for _, want := range []domain.CodexResponsesTransport{
		domain.CodexResponsesTransportHTTP,
		domain.CodexResponsesTransportWS,
		domain.CodexResponsesTransportDefault,
	} {
		saved = seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: want,
		})
		require.Equal(t, want, saved.CodexResponsesTransport, "upsert 返回值必须反映落库值")

		got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err)
		require.Equal(t, want, got.CodexResponsesTransport, "回读必须与落库值一致")
	}

	rows, err := repos.Client.TemplateExt.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, rows, "同 template_id 幂等 upsert 恒单行")
}

// TestPGCodexTransportSnapshotMerge 快照合并矩阵：
//   - strip=NULL, transport=ws  ⇒ 合并 ws（**独立 if** 门控：塞进 strip 守卫里这行必红）
//   - strip=true, transport=ws  ⇒ 合并 ws + strip 保持 true（两列互不干扰）
//   - strip_image_tools 先写 true 再清 NULL，transport 仍生效（守卫与开关解耦）
func TestPGCodexTransportSnapshotMerge(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("strip NULL transport ws merges", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-null")
		g := seedPGGroup(t, repos, "g-null")
		acc := seedPGAccount(t, repos, tpl.ID, "a-null")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 本行要的是「列缺失」这条路径的机制：**不传** StripImageTools ⇒ 仓库里
		// 的 SetNillableStripImageTools(nil) 是 no-op ⇒ strip_image_tools 列根本
		// 不进入 INSERT 列表 ⇒ 落 DB 默认值 NULL。**不要**为了"可读性"补成
		// new(false)：那是显式 false，走的是另一条路径（列在 INSERT 里、值非
		// NULL），本用例就退化成下面那条 strip=true 的近亲，独立 if 的门控再也
		// 测不到。
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok, "快照必须含该账号")
		require.Equal(t, domain.CodexResponsesTransportWS, got,
			"strip=NULL 行也必须合并出站传输（合并是独立 if，不得挂在 strip 守卫内）")

		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.Equal(t, domain.CodexResponsesTransportWS, members[0].Template.CodexResponsesTransport,
			"组级重载数据源同语义")
	})

	t.Run("strip true transport ws merges and strip preserved", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-strip")
		g := seedPGGroup(t, repos, "g-strip")
		acc := seedPGAccount(t, repos, tpl.ID, "a-strip")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			StripImageTools:         new(true),
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		m, err := repos.Groups.LoadGroupsAccounts(ctx)
		require.NoError(t, err)
		var found *domain.Template
		for _, a := range m[g.ID] {
			if a.ID == acc.ID {
				found = a.Template
			}
		}
		require.NotNil(t, found)
		require.True(t, found.StripImageTools, "strip 列照旧合并")
		require.Equal(t, domain.CodexResponsesTransportWS, found.CodexResponsesTransport,
			"两列互不干扰：strip 与出站传输同一次合并各自独立")
	})

	t.Run("strip cleared later keeps transport", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-clear")
		g := seedPGGroup(t, repos, "g-clear")
		acc := seedPGAccount(t, repos, tpl.ID, "a-clear")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			StripImageTools:         new(true),
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})
		// 清 strip（nil ⇒ NULL），transport 原样重写：守卫为假时出站传输仍必须合并
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportWS, got)
	})
}

// TestPGCodexTransportMergeGating 三重门控的两个否定行：
//   - ext 行类型 ≠ 模板类型（模板改过类型、陈旧 ext 行仍带 ws）⇒ 不合并
//   - 模板不是 codex 类型（如 custom 挂着陈旧 ws 行）⇒ 不合并
func TestPGCodexTransportMergeGating(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("stale ext row type mismatch not merged", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-stale")
		g := seedPGGroup(t, repos, "g-stale")
		acc := seedPGAccount(t, repos, tpl.ID, "a-stale")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 陈旧行：ext.credential_type=codex-pat 而模板是 codex-oauth
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexPAT,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportDefault, got,
			"ext 行类型与模板类型不一致 ⇒ 不得合并（否则类型改回 codex 时 ws 突然生效）")
	})

	t.Run("non codex template not merged", func(t *testing.T) {
		tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "t-custom", BaseURL: "",
			CredentialType:   credential.TypeCustom,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, err)
		g := seedPGGroup(t, repos, "g-custom")
		acc := seedPGAccount(t, repos, tpl.ID, "a-custom")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 脏行：custom 模板的 ext 行带着 ws（写面会 400，这里模拟 DB 直改）
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportDefault, got,
			"非 codex 类型不经过 responses 出站面 ⇒ 不合并")
	})
}

// ---------------------------------------------------------------------------
// 守卫写的原子性（spec §4.3 原子性约束 + §8.2 并发门禁）
// ---------------------------------------------------------------------------

// guardDecide 复刻 §4.3 第 3 步（降级门）的判定：old == ws && new != ws ⇒ 409
// （不带确认）。仓库层用例只关心「判定发生在事务内、基于事务内那一次读」，所以
// 用这一条最小判定就够了；service 的完整判定（类型门 + 幂等例外 + WARN）见
// internal/service/ext.go 的 decideTransport。
func guardDecide(newTransport domain.CodexResponsesTransport) repository.TemplateExtDecide {
	return func(old *domain.TemplateExt) (bool, error) {
		oldTransport := domain.CodexResponsesTransportDefault
		if old != nil {
			oldTransport = old.CodexResponsesTransport
		}
		if oldTransport == domain.CodexResponsesTransportWS && newTransport != domain.CodexResponsesTransportWS {
			return false, fmt.Errorf("%w: stub downgrade gate", repository.ErrConflict)
		}
		return false, nil
	}
}

// resetTransportColumn 把列**直接**改回 ""（不经守卫写）：本文件的并发用例要的是
// 「同一 template_id 反复复用」，重置动作本身不该成为被测代码的一部分，也不该为它
// 重建模板或清库（整包时间会被拖垮）。
func resetTransportColumn(t *testing.T, repos *repository.Repository, templateID int64) {
	t.Helper()
	_, err := repos.Client.TemplateExt.Update().
		Where(templateext.TemplateIDEQ(templateID)).
		SetCodexResponsesTransport("").
		Save(context.Background())
	require.NoError(t, err)
}

// TestPGTemplateExtGuardWriteDecideVetoesWrite decide 非 nil err ⇒ 回滚 + **原样**
// 上抛。repository 层只断言「未落库」（行不存在 / 值未变）；「无 WARN」是 service
// 层用例的事——WARN 在提交之后由 service 写，repository 看不见日志。
func TestPGTemplateExtGuardWriteDecideVetoesWrite(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-veto")

	sentinel := errors.New("decide veto")

	// 首次建行：decide 拒绝 ⇒ 一行都不许落。
	_, down, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, func(old *domain.TemplateExt) (bool, error) {
		require.Nil(t, old, "首次建行的事务内 old 必须是 nil（不是 404、不是空行）")
		return true, sentinel // down=true 也不许泄出去：写没发生
	})
	require.ErrorIs(t, err, sentinel, "decide 的错误必须原样上抛")
	require.Equal(t, sentinel, err, "不得 %w 包成 DB 错误（实际：%v）", err)
	require.False(t, down, "回滚的写不算降级")
	_, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "decide 拒绝 ⇒ 不得建行")

	// 已有行：decide 拒绝 ⇒ 值不得变（409 在 repository 侧的落库证据）。
	seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	})
	_, down, err = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
	}, guardDecide(domain.CodexResponsesTransportDefault))
	require.ErrorIs(t, err, repository.ErrConflict, "降级门拒绝必须以 ErrConflict 返回")
	require.False(t, down, "被拒绝的降级不得报告 down")
	got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportWS, got.CodexResponsesTransport,
		"被 decide 拒绝的写不得改动原值")

	// 反证：同一行换成放行的 new（ws → ws）⇒ 事务内读到 ws、down 为 false、落库正常。
	saved, down, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, guardDecide(domain.CodexResponsesTransportWS))
	require.NoError(t, err)
	require.False(t, down, "ws → ws 不是降级")
	require.Equal(t, domain.CodexResponsesTransportWS, saved.CodexResponsesTransport)
}

// TestPGTemplateExtGuardWriteNilDecidePanics decide == nil ⇒ **panic**，不是"不判定
// 直接放行"（spec v4.26 写死）：nil 等于把 409 门整个关掉，是最危险的默认值。
// 入口处就 panic（连事务都不开），因此也断言未落库。
func TestPGTemplateExtGuardWriteNilDecidePanics(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-nil-decide")

	const wantMsg = "repository: template ext guard write requires a non-nil decide callback"

	// 首次建行：panic 且一行都不许落。
	require.PanicsWithValue(t, wantMsg, func() {
		_, _, _ = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		}, nil)
	})
	_, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "nil decide panic 前不得落库")

	// 已有行：panic 且值不得变。
	seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	})
	require.PanicsWithValue(t, wantMsg, func() {
		_, _, _ = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		}, nil)
	})
	got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportWS, got.CodexResponsesTransport,
		"nil decide panic 不得改动原值")
}

// TestPGTemplateExtTransportConcurrentGuardWrite §8.2 并发门禁（真 PG，取值写死）：
// 同一 template_id，两条**守卫写**并发（各开自己的事务），重复 N=50 次，每次只用
// 直接 UPDATE 把列重置回 ""（复用同一模板与同一个库，不重建）。
//
// 初始 ""；A 提交 new=""（无确认）、B 提交 new="ws"。
//
// 只断言**共同性质**（下面的分支表只说明判别力，**不是**断言条件）：
//   - 终值必是 ws。父行锁排他且唯一 ⇒ 两条守卫写必然一先一后提交；old 只在事务内
//     读 ⇒ 不存在"读到中间值"的第三种结果。
//     · A 先拿锁：A ""→"" 走幂等空写放行（409=0、成功=2），B 随后升级到 ws；
//     · B 先拿锁：B 先落 ws，A 在事务内读到 ws 且 new 非 ws 无确认 ⇒ 409=1、成功=1。
//   - 409 次数 + 成功次数 == 2（成功 = 守卫写返回 nil，**含** new == old 的幂等空写）。
//     这条口径必须写死：不定义"空写算不算成功"，==2 会变 ==1 而误红。
//
// **禁止**逐分支断言：不要求"必须观测到 409"、不要求"两支都出现过"（50 次里两支
// 各 ~25 次是期望、不是保证，按它断言会随机红）。
//
// 为什么重复 50 次：单次配对只是**提高暴露交错的机会**，不是确定性证明——起跑
// channel 不保证两条事务都在对方提交前读到 old，连接获取与调度也不保证等概率。
// 真正的判别力来自变异实测：M1 去掉 `... FOR UPDATE` ⇒ 本用例红；M2 把 old 的读取
// 挪到父行锁之前 ⇒ 本用例红（顺序"先锁后读"因此有判别力）。本用例的职责就是把这两
// 类改动暴露出来。
// 也不用"两侧都写 ws"这类无判别力的配对：那样去掉锁也全绿。
func TestPGTemplateExtTransportConcurrentGuardWrite(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-guard-race")

	// 建行（初始 ""），之后每轮只重置列值。
	seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
	})

	const rounds = 50
	for round := range rounds {
		resetTransportColumn(t, repos, tpl.ID)

		// channel 屏障（不是睡眠等待）：两条写同时起跑。
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, results[0] = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
				TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			}, guardDecide(domain.CodexResponsesTransportDefault))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, results[1] = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
				TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
				CodexResponsesTransport: domain.CodexResponsesTransportWS,
			}, guardDecide(domain.CodexResponsesTransportWS))
		}()
		close(start)
		wg.Wait()

		conflicts, successes := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, repository.ErrConflict):
				conflicts++
			default:
				require.NoError(t, err, "第 %d 轮：守卫写只允许「成功」或「409」", round)
			}
		}
		require.Equal(t, 2, conflicts+successes,
			"第 %d 轮：409 次数 + 成功次数必须恰为 2（成功含幂等空写）", round)

		got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err)
		require.Equal(t, domain.CodexResponsesTransportWS, got.CodexResponsesTransport,
			"第 %d 轮：终值必须是 ws（降级写不得覆盖升级写）", round)
	}
}

// TestPGTemplateExtGuardWriteHoldsParentLock 受控锁阻塞用例：**直接观测**父行锁，
// 不用"B 在某个时间窗内没返回"去推断锁存在。
//
// 证据面：A 的 decide 回调只在**已持父行锁之后**被调用，所以它向主 goroutine 发"我已
// 持锁"信号后阻塞在 release 上；B 起跑后，主 goroutine 用**独立观察连接**
// （pgSharedConn：同一 clone，只读 pg_stat_activity，不参与锁竞争）轮询，直到看见 B
// 的后端确实阻塞在那条 `SELECT id FROM templates WHERE id = $1 FOR UPDATE` 上
// （pg_blocking_pids(pid) 非空）才放行 A。轮询间隔 25ms、总窗口 3s——窗口必须短于
// pgtest.OpenPool 给会话设的 lock_timeout=5s，否则"B 锁超时后返回"会被误读成证据。
//
//   - 窗口内 B 先返回 ⇒ 父行锁没有生效；
//   - 窗口结束仍未观测到任何锁等待 ⇒ 不能用「B 变慢」冒充锁生效。
//
// 两个方向都是 Fatal，超时只作失败兜底，绝不当通过证据。
//
// 判别力（变异实测）：去掉 `... FOR UPDATE` 后 B 不再等父行，几毫秒内就把守卫写做完
// ⇒ 观测面永远看不到 pg_blocking_pids 非空 ⇒ 本用例红在"未观测到 B 的父行锁等待"。
//
// A 放行后两者都必须成功（A 先提交，B 随后读到 A 写下的值再写）。
func TestPGTemplateExtGuardWriteHoldsParentLock(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-lock-block")
	seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	})

	// 预热只是降低 B「取连接」那一段的抖动，好把观测窗口留给锁本身：pgxpool 并不保证
	// 连接常驻（空闲连接随时可回收），所以预热不是证据的一部分——证据只来自下面的
	// pg_stat_activity 观测。
	var prewarm sync.WaitGroup
	for range 3 {
		prewarm.Add(1)
		go func() {
			defer prewarm.Done()
			_, _ = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		}()
	}
	prewarm.Wait()

	// 观测连接现在就建好：首个后端建连要 1–2s，落在观测窗口里会挤掉 lock_timeout 的
	// 余量。它只读 pg_stat_activity，不参与 A/B 的锁竞争。
	observer := pgSharedConn(t)

	locked := make(chan struct{})    // A → 主 goroutine：我已持父行锁
	releaseCh := make(chan struct{}) // 主 goroutine → A：可以继续了
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release) // 断言中途失败也要放行 A，别把连接与事务挂到用例结束

	aErr := make(chan error, 1)
	go func() {
		_, _, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		}, func(*domain.TemplateExt) (bool, error) {
			close(locked)
			<-releaseCh
			return false, nil
		})
		aErr <- err
	}()

	select {
	case <-locked:
	case <-time.After(30 * time.Second):
		t.Fatal("A 未能在 30s 内进入 decide（守卫写没走到判定面？）")
	}

	bStarted := make(chan struct{}) // B → 主 goroutine：我马上发那条守卫写
	bDone := make(chan struct{})
	bErr := make(chan error, 1)
	go func() {
		defer close(bDone)
		close(bStarted)
		_, _, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		}, func(*domain.TemplateExt) (bool, error) { return false, nil })
		bErr <- err
	}()
	<-bStarted

	// A 仍持锁：观测 B 是否**真的在等这把父行锁**。判据是 PG 自己的阻塞关系
	// （pg_blocking_pids 非空），不是"这段时间里 B 没返回"。
	// 私有 clone ⇒ datname 已足够隔离；pid <> pg_backend_pid() 排掉观察连接自己
	// （它的 SQL 文本里含有同样的字面量，虽然它不阻塞）。
	lockWaiters := func() int {
		var n int
		require.NoError(t,
			observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND pid <> pg_backend_pid()
  AND cardinality(pg_blocking_pids(pid)) > 0
  AND query LIKE '%FROM templates WHERE id = $1 FOR UPDATE%'`).Scan(&n),
			"观测连接必须能查询 pg_stat_activity")
		return n
	}
	const (
		observeWindow = 3 * time.Second // < 会话 lock_timeout=5s：窗口内不该出现 B 锁超时
		observePoll   = 25 * time.Millisecond
	)
	deadline := time.NewTimer(observeWindow)
	defer deadline.Stop()
	tick := time.NewTicker(observePoll)
	defer tick.Stop()

	bReturned := func() bool {
		select {
		case <-bDone:
			return true
		default:
			return false
		}
	}
	// B 提前把守卫写做完，说明父行上根本没挡住它：观测不到锁等待，且原因要说清楚。
	failWrittenWhileLocked := func() {
		t.Fatalf("未观测到 B 的父行锁等待：B 在 A 持锁期间就完成了守卫写（err=%v）——父行锁没有生效",
			<-bErr)
	}

	waiting := false
	for !waiting {
		if bReturned() {
			failWrittenWhileLocked()
		}
		if lockWaiters() > 0 {
			waiting = true
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			// 截止瞬间做最后一次判定，别把"刚好在这一刻阻塞"判成红。
			if bReturned() {
				failWrittenWhileLocked()
			}
			if lockWaiters() > 0 {
				waiting = true
				break
			}
			t.Fatalf("未观测到 B 的父行锁等待：不能用「B 变慢」冒充锁生效")
		}
	}
	release()
	require.NoError(t, <-aErr, "A 放行后必须成功")
	<-bDone
	require.NoError(t, <-bErr, "B 在 A 提交后必须成功")

	got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Equal(t, domain.CodexResponsesTransportWS, got.CodexResponsesTransport)
}
