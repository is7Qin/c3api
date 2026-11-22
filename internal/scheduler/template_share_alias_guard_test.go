// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestSharedTemplatePointerFreezesDerivedFacts 是 U22 §2.3「别名防护」的可执行声明。
//
// 载入器现在让同一次查询里同 template_id 的多个账号共享**同一枚** *domain.Template
// （repository.toDomainAccountWithTemplateCache）。共享指针的真正风险不是判等，而是
// 可变别名：发布之后任何对模板字段的就地写都会被所有共享者看到。共享之所以安全，
// 唯一依据是**派生事实在构造期定型**——newSnapshotStatic 一次派生 planKey（含五项
// 模板摘要）并随叶子持有，此后叶子只读自己那份值，不再回头读 tpl。
//
// 本用例按这个主张执行：账号侧同一枚指针（别名真实存在）→ 构造两个叶子 → 故意
// 就地写共享模板 → 两个叶子的 planKey 一字不变；再用"同样一次写如果发生在构造前
// 就一定会改变键"作反证，证明上面的等值不是"键换了个地方读、看不见变化"。
//
// 若这条红：说明共享对象不安全，应退回"共享只读摘要对象"，不要改成共享别的可变
// 对象（U22 §2.4 的退路）。
func TestSharedTemplatePointerFreezesDerivedFacts(t *testing.T) {
	tpl := &domain.Template{
		ID: 42, Name: "tpl-shared", BaseURL: "https://shared.example/v1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m-1"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m-1"}},
		ModelMapping: domain.ModelMapping{
			"m-1": {MappedModel: "up-1", Mode: domain.ModelMappingModeExplicit},
		},
	}
	// 载入器的输出形状：两个账号、同一枚模板指针。
	a1 := domain.Account{ID: 1, Name: "a1", TemplateID: tpl.ID, Template: tpl, Enabled: true}
	a2 := domain.Account{ID: 2, Name: "a2", TemplateID: tpl.ID, Template: tpl, Enabled: true}
	require.Same(t, a1.Template, a2.Template, "前提：两个账号共享同一枚模板指针")

	eg := directEgress()
	s1 := newSnapshotStatic(a1, a1.Template, []int64{10}, eg)
	s2 := newSnapshotStatic(a2, a2.Template, []int64{10}, eg)
	published1, published2 := planKeyOf(s1), planKeyOf(s2)

	// 发布后就地写共享模板字段——这正是共享指针引入的唯一风险面。
	tpl.Models = append(tpl.Models, "m-2")
	tpl.BaseURL = "https://mutated.example/v1"
	tpl.ModelMapping["m-1"] = domain.ModelMappingEntry{MappedModel: "up-2", Mode: domain.ModelMappingModeExplicit}

	// 别名是真实的：另一个共享者的视图确实变了（所以安全性不能靠"看不见"）。
	require.Equal(t, "https://mutated.example/v1", a2.Template.BaseURL,
		"共享指针下两个账号看到的是同一枚模板")
	require.Same(t, s2.tpl, a2.Template, "叶子持有的是共享指针本身")

	// 派生事实在构造期定型：已发布的 planKey 不得随共享对象的写而改变。
	require.Equal(t, published1, planKeyOf(s1),
		"共享指针的就地写不得改变已发布叶子的 planKey")
	require.Equal(t, published2, planKeyOf(s2),
		"另一个共享该指针的账号，派生事实同样不得变")

	// 反证（防"永远相等"的空断言）：同样一次写若发生在构造前，键必须变。
	later := planKeyOf(newSnapshotStatic(a2, a2.Template, []int64{10}, eg))
	require.NotEqual(t, published2.baseURL, later.baseURL,
		"模板 BaseURL 改动必须进入 planKey——否则本条防护没有可观测面")
	require.NotEqual(t, published2.modelsDigest, later.modelsDigest,
		"模板 Models 改动必须进入 modelsDigest")
	require.NotEqual(t, published2.modelMappingKey, later.modelMappingKey,
		"模板 ModelMapping 改动必须进入 modelMappingKey")
}
