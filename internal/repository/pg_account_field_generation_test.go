// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// seedPGTemplateNamed 建指定名的模板（accounts.template_id 有外键，必先建；
// 模板名唯一，故表驱动的每个子用例用自己的模板名）。
func seedPGTemplateNamed(t *testing.T, repos *repository.Repository, name string) *domain.Template {
	t.Helper()
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "https://u/v1",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

// fieldPatchForSpec 为声明表的一行构造"单独改这一个字段"的补丁。新值恒与种子
// 不同（否则首次写入即幂等，身份类行的 K 断言被掏空）。default 分支直接失败：
// 声明表新增字段而此处未补分支时，测试必须失败而不是静默跳过（与 ChangedFields
// 完备门同构）。
func fieldPatchForSpec(t *testing.T, repos *repository.Repository, spec domain.AccountFieldSpec) repository.AccountPatch {
	t.Helper()
	switch spec.Field {
	case domain.FieldName:
		return repository.AccountPatch{Name: strPtr("renamed-" + spec.Name)}
	case domain.FieldTemplateID:
		alt := seedPGTemplateNamed(t, repos, "tpl-alt-"+spec.Name)
		return repository.AccountPatch{TemplateID: &alt.ID}
	case domain.FieldBaseURL:
		return repository.AccountPatch{BaseURL: strPtr("https://acc-" + spec.Name + ".example.com")}
	case domain.FieldUpstreamKey:
		return repository.AccountPatch{UpstreamKey: strPtr("sk-rotated-" + spec.Name)}
	case domain.FieldMaxConcurrency:
		return repository.AccountPatch{MaxConcurrency: intPtr(16)}
	case domain.FieldGroupIDs:
		g := seedPGGroup(t, repos, "g-"+spec.Name)
		return repository.AccountPatch{GroupIDs: &[]int64{g.ID}}
	case domain.FieldEnabled:
		return repository.AccountPatch{Enabled: boolPtr(false)}
	case domain.FieldCacheDomain:
		return repository.AccountPatch{CacheDomain: strPtr("cache-" + spec.Name + ".example")}
	case domain.FieldUpstreamCostMultiplier:
		return repository.AccountPatch{UpstreamCostMultiplierBp: intPtr(25000)}
	default:
		t.Fatalf("declared field %q has no patch constructor: add one, do not skip", spec.Name)
		return repository.AccountPatch{}
	}
}

// idempotentPatchForSpec 按读回的账号行构造"同值再写"补丁：每个字段都取当前
// 落库值（可空字段 nil 即传 nil，也就是"不变"；ChangedFields 把它判为无变更）。
func idempotentPatchForSpec(spec domain.AccountFieldSpec, got *domain.Account, groups []int64) repository.AccountPatch {
	switch spec.Field {
	case domain.FieldName:
		return repository.AccountPatch{Name: &got.Name}
	case domain.FieldTemplateID:
		return repository.AccountPatch{TemplateID: &got.TemplateID}
	case domain.FieldBaseURL:
		return repository.AccountPatch{BaseURL: got.BaseURL}
	case domain.FieldUpstreamKey:
		return repository.AccountPatch{UpstreamKey: &got.UpstreamKey}
	case domain.FieldMaxConcurrency:
		return repository.AccountPatch{MaxConcurrency: &got.MaxConcurrency}
	case domain.FieldGroupIDs:
		return repository.AccountPatch{GroupIDs: &groups}
	case domain.FieldEnabled:
		return repository.AccountPatch{Enabled: &got.Enabled}
	case domain.FieldCacheDomain:
		return repository.AccountPatch{CacheDomain: got.CacheDomain}
	case domain.FieldUpstreamCostMultiplier:
		return repository.AccountPatch{UpstreamCostMultiplierBp: &got.UpstreamCostMultiplierBp}
	default:
		return repository.AccountPatch{}
	}
}

// TestPGAccountFieldPatchAdvancesGeneration 是逐字段表驱动：遍历字段声明表
// （domain.AccountFieldSpecs，账号写面的唯一事实来源）的每一行，单独 PATCH
// 该字段，断言配置代际 C +1。身份类字段真变时身份代际 K +1，非身份字段真变
// 时 K 不变；随后同值再写一遍，断言只推进 C、不额外推进 K。
//
// 遍历范围就是"类为配置或身份的字段"的全集：声明表只含这两类，运行时拥有的
// 三个字段与生成只读的 K 都不在表里，故不在此列（用声明表的 Identity 分类判
// 断，不手写字段清单）。
//
// "静态快照被更新"的一半由调度器侧的静态键逐字段子用例覆盖（每个配置或身份
// 字段改了就换键），本包不再重复实现快照断言，只钉住代际推进。
//
// 真实 PostgreSQL 基座（TEST_DATABASE_URL 未设则跳过）。
func TestPGAccountFieldPatchAdvancesGeneration(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	specs := domain.AccountFieldSpecs()
	require.NotEmpty(t, specs, "field declaration table must not be empty")
	for _, spec := range specs {
		spec := spec
		t.Run(spec.Name, func(t *testing.T) {
			tpl := seedPGTemplateNamed(t, repos, "tpl-"+spec.Name)
			acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
				Name: "seed", TemplateID: tpl.ID, UpstreamKey: "sk-seed",
				MaxConcurrency: 8, Enabled: true,
			})
			require.NoError(t, err)
			require.Equal(t, int64(1), acc.LifecycleRevision)
			require.Equal(t, int64(1), acc.IdentityRevision)

			// 1) 单独 PATCH 该字段：C 必 +1；K 按声明表的身份分类推进。
			_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, fieldPatchForSpec(t, repos, spec))
			require.NoError(t, err)
			after, err := repos.Accounts.GetAccount(ctx, acc.ID)
			require.NoError(t, err)
			require.Equal(t, int64(2), after.LifecycleRevision, "patching %q must advance C", spec.Name)
			wantK := int64(1)
			if spec.Identity {
				wantK = 2
			}
			require.Equal(t, wantK, after.IdentityRevision, "patching %q: identity field must advance K, config field must not", spec.Name)

			// 2) 幂等重写（同值再写）：C 再 +1，K 不动。
			var groups []int64
			if spec.Field == domain.FieldGroupIDs {
				groups, err = repos.Accounts.GetAccountGroups(ctx, acc.ID)
				require.NoError(t, err)
			}
			_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, idempotentPatchForSpec(spec, after, groups))
			require.NoError(t, err)
			idem, err := repos.Accounts.GetAccount(ctx, acc.ID)
			require.NoError(t, err)
			require.Equal(t, int64(3), idem.LifecycleRevision, "idempotent rewrite of %q must still advance C", spec.Name)
			require.Equal(t, wantK, idem.IdentityRevision, "idempotent rewrite of %q must not advance K", spec.Name)
		})
	}
}
