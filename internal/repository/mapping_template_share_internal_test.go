// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
)

// 载入器的模板共享（U22 §2.1）在映射层的硬不变量。作用域（TemplateLoadScope）由
// 调用方持有，生命周期 = 一次构建 / 一个失效批次；生产侧的两个载入入口把它的
// 形状钉死在 group_repo.go，端到端（真库、跨组、跨 reload）验证见
// pg_template_share_test.go。
//
// 夹具形状是**照抄 Ent 行为**：同一次查询里同 template_id 的账号共享同一枚
// *ent.Template 实体（internal/ent/account_query.go:493-518 先按 TemplateID 去重外键，
// 再把同一个邻居实体分配给该 ID 的所有账号）。所以这里不需要给每个账号造一个
// 内容相同但指针不同的 ent 模板。

// shareEntTemplate 造一枚 ent 模板行（只填 toDomainTemplate 读的字段）。
func shareEntTemplate(id int64, name string, models ...string) *ent.Template {
	return &ent.Template{
		ID: id, Name: name, BaseURL: "https://" + name + "/v1",
		CredentialType:   string(credential.TypeAPIKey),
		SupportedFormats: []string{string(domain.FormatOpenAIChat)},
		Models:           models,
	}
}

// shareEntAccount 造一枚 ent 账号行，带上 Spec 的模板边（nil 模板 = 未绑定）。
func shareEntAccount(id, tplID int64, tpl *ent.Template) *ent.Account {
	a := &ent.Account{ID: id, Name: "acc-" + strconv.FormatInt(id, 10), TemplateID: tplID, Enabled: true}
	if tpl != nil {
		a.Edges.Template = tpl
	}
	return a
}

// TestToDomainAccountSharesOneTemplatePerIDInScope 是不变量 3 的映射层半边：
// 同一作用域内同 template_id 的两个账号必须拿到**同一枚** *domain.Template 指针；
// 不同 template_id 必须是不同对象；计数器必须精确到"每模板一次转换"。
func TestToDomainAccountSharesOneTemplatePerIDInScope(t *testing.T) {
	tplA := shareEntTemplate(11, "tpl-a", "m-a")
	tplB := shareEntTemplate(22, "tpl-b", "m-b")
	rows := []*ent.Account{
		shareEntAccount(1, tplA.ID, tplA),
		shareEntAccount(2, tplA.ID, tplA),
		shareEntAccount(3, tplB.ID, tplB),
	}

	scope := NewTemplateLoadScope()
	got := make([]*domain.Account, 0, len(rows))
	for _, r := range rows {
		got = append(got, toDomainAccountWithTemplateScope(r, scope))
	}

	require.Same(t, got[0].Template, got[1].Template,
		"作用域内同 template_id 必须共享同一枚 *domain.Template")
	require.NotSame(t, got[0].Template, got[2].Template,
		"不同 template_id 必须是不同对象（共享只按 ID 复用，不做跨 ID 折叠）")
	require.Equal(t, tplA.ID, got[0].Template.ID)
	require.Equal(t, tplA.Name, got[0].Template.Name)
	require.Equal(t, tplB.ID, got[2].Template.ID)
	require.Equal(t, got[0].Template, got[1].Template, "共享的指针内容当然相等（值语义对照）")

	// 计数器就是"每模板一次转换"的可执行陈述：3 个账号、2 个模板 ⇒ 1 命中 2 未命中。
	require.Equal(t, 2, scope.Misses(), "未命中 = 不同 template_id 数")
	require.Equal(t, 1, scope.Hits(), "命中 = 账号数 - 不同 template_id 数")
}

// TestToDomainAccountWithoutScopeKeepsOneCopyPerAccount 钉住 nil 作用域的既有语义：
// 其它读面（管理面单读/列表）仍然每账号各转一份。这条同时是"别把作用域偷偷做成包级
// 单例"的对照组——那种改法会让这里也变成同一枚。
func TestToDomainAccountWithoutScopeKeepsOneCopyPerAccount(t *testing.T) {
	tplA := shareEntTemplate(11, "tpl-a", "m-a")
	d1 := toDomainAccount(shareEntAccount(1, tplA.ID, tplA))
	d2 := toDomainAccount(shareEntAccount(2, tplA.ID, tplA))

	require.NotNil(t, d1.Template)
	require.NotSame(t, d1.Template, d2.Template,
		"无作用域路径必须保持旧语义：每个账号一份模板副本")
	require.Equal(t, d1.Template, d2.Template, "副本内容相等（形状不变，只是不共享指针）")
}

// TestToDomainAccountScopeDoesNotLeakAcrossLoads 是不变量 3 的"跨 reload"半边：
// 两次载入各建一个作用域 ⇒ 同 ID 的模板是**新对象**，第二轮读到的新内容必须生效。
// 把作用域提到包级会让两个断言同时红。
func TestToDomainAccountScopeDoesNotLeakAcrossLoads(t *testing.T) {
	tplA := shareEntTemplate(11, "tpl-a", "m-old")
	load := func(tpl *ent.Template) []*domain.Account {
		rows := []*ent.Account{
			shareEntAccount(1, tpl.ID, tpl),
			shareEntAccount(2, tpl.ID, tpl),
		}
		scope := NewTemplateLoadScope()
		out := make([]*domain.Account, 0, len(rows))
		for _, r := range rows {
			out = append(out, toDomainAccountWithTemplateScope(r, scope))
		}
		return out
	}
	first := load(tplA)
	// 第二轮查询读到的是同一模板 ID 的**新内容**（新 ent 实体）。
	second := load(shareEntTemplate(11, "tpl-a", "m-new"))

	require.Same(t, first[0].Template, first[1].Template)
	require.Same(t, second[0].Template, second[1].Template)
	require.NotSame(t, first[0].Template, second[0].Template,
		"跨 reload 必须是新对象：复用上一轮的模板对象会让作用域变成包级单例")
	require.Equal(t, []string{"m-old"}, first[0].Template.Models)
	require.Equal(t, []string{"m-new"}, second[0].Template.Models,
		"跨 reload 的新内容必须可见（作用域生命周期 = 一次构建/一个批次）")
}

// templateShareSink 接收基准结果，防止编译器把转换结果整个优化掉。
var templateShareSink *domain.Account

// BenchmarkToDomainAccountTemplateShare 给出 U22 第一步的**载入侧**分配差：
// 3000 账号 / 100 模板的生产形状下，模板转换次数从"每账号一次"降到"每模板一次"。
//
// 说明：scheduler 的 BenchmarkGroupRebuildA3000M100 不经过仓储载入器（它直接吃
// 现成的 domain 账号），所以本步骤的收益只能在载入器这一层量；摘要缓存的收益
// （第二步）才落在那个重建级基准上。
func BenchmarkToDomainAccountTemplateShare(b *testing.B) {
	const accounts, templates = 3000, 100
	tpls := make([]*ent.Template, templates)
	for i := range tpls {
		tpls[i] = shareEntTemplate(int64(i+1), "bench", "m-1", "m-2")
	}
	rows := make([]*ent.Account, accounts)
	for i := range rows {
		tpl := tpls[i%templates]
		rows[i] = shareEntAccount(int64(i+1), tpl.ID, tpl)
	}

	b.Run("shared/one-object-per-template", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			scope := NewTemplateLoadScope()
			for _, r := range rows {
				templateShareSink = toDomainAccountWithTemplateScope(r, scope)
			}
		}
	})
	b.Run("per-account-copy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, r := range rows {
				templateShareSink = toDomainAccountWithTemplateScope(r, nil)
			}
		}
	})
}
