// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestChangedFieldsCoversEveryDeclaredField 是字段集的**完备性门**：domain 的
// 声明表是唯一事实来源，ChangedFields 必须对表中每一行都有判定分支。新增可写
// 字段而漏加分支会在这里失败，而不是静默退化成"该字段永不算变更"（对身份类
// 字段即"永不推进 K"——失效判决与在途工件再也不会作废）。
func TestChangedFieldsCoversEveryDeclaredField(t *testing.T) {
	base := repository.AccountFieldValues{
		ID:                       1,
		Name:                     "acc",
		TemplateID:               7,
		BaseURL:                  new("https://old.example.com"),
		UpstreamKey:              "sk-old",
		MaxConcurrency:           4,
		Enabled:                  true,
		CacheDomain:              new("old-domain"),
		UpstreamCostMultiplierBp: 10000,
		SupplierUserID:           5,
		ProxyID:                  3, // 非 0 旧值：&0（解绑）与旧值 3 不相等才可判为变更
	}
	// 每个声明字段一条"改这一个字段"的补丁；表新增字段而未补用例 → 下面的
	// 完备性断言失败。
	cases := map[domain.AccountField]repository.AccountPatch{
		domain.FieldName:                   {Name: new("renamed")},
		domain.FieldTemplateID:             {TemplateID: new(int64(8))},
		domain.FieldBaseURL:                {BaseURL: new("https://new.example.com")},
		domain.FieldUpstreamKey:            {UpstreamKey: new("sk-new")},
		domain.FieldMaxConcurrency:         {MaxConcurrency: new(9)},
		domain.FieldGroupIDs:               {GroupIDs: &[]int64{3}},
		domain.FieldEnabled:                {Enabled: new(false)},
		domain.FieldCacheDomain:            {CacheDomain: new("new-domain")},
		domain.FieldUpstreamCostMultiplier: {UpstreamCostMultiplierBp: new(25000)},
		domain.FieldSupplierUserID:         {SupplierUserID: new(int64(9))},
		domain.FieldProxyID:                {ProxyID: new(int64(9))},
	}
	declared := domain.AccountFieldSpecs()
	require.Len(t, cases, len(declared), "every declared field needs a change case")
	for _, spec := range declared {
		patch, ok := cases[spec.Field]
		require.True(t, ok, "declared field %q has no case", spec.Name)
		changed := repository.ChangedFields(patch, base)
		require.Equal(t, []domain.AccountField{spec.Field}, changed.Fields(),
			"changing only %q must yield exactly that field", spec.Name)
	}

	// 幂等重写（补丁带全部字段、值与旧值逐一相同）不得产生任何变更位。
	same := repository.AccountPatch{
		Name:                     new(base.Name),
		TemplateID:               new(base.TemplateID),
		BaseURL:                  new(*base.BaseURL),
		UpstreamKey:              new(base.UpstreamKey),
		MaxConcurrency:           new(base.MaxConcurrency),
		Enabled:                  new(base.Enabled),
		CacheDomain:              new(*base.CacheDomain),
		UpstreamCostMultiplierBp: new(base.UpstreamCostMultiplierBp),
		SupplierUserID:           new(base.SupplierUserID),
		ProxyID:                  new(base.ProxyID),
	}
	require.Empty(t, repository.ChangedFields(same, base).Fields(), "idempotent rewrite is not a change")
}

// TestChangedFieldsIdentityClassification 钉住"哪些字段改身份"：身份类字段集
// 由声明表派生，不得在比较点或调用点重写。四条身份类字段是路由目标身份 I 的
// 全部输入：模板 / 生效 origin / 凭据 key，**加上出口**——上游看到的身份是三
// 维（端点 / 凭据 / 出口 IP），风控按 IP 绑会话是常态，换出口即换身份（spec
// proxy-pool §3.4 最后一段）。
func TestChangedFieldsIdentityClassification(t *testing.T) {
	require.Equal(t, []domain.AccountField{
		domain.FieldTemplateID,
		domain.FieldBaseURL,
		domain.FieldUpstreamKey,
		domain.FieldProxyID,
	}, domain.IdentityFields().Fields())

	base := repository.AccountFieldValues{TemplateID: 7, UpstreamKey: "sk-old"}
	for _, tc := range []struct {
		name  string
		patch repository.AccountPatch
		want  bool
	}{
		{"config only", repository.AccountPatch{Name: new("x"), Enabled: new(false)}, false},
		{"groups only", repository.AccountPatch{GroupIDs: &[]int64{1}}, false},
		{"cache domain", repository.AccountPatch{CacheDomain: new("d")}, false},
		{"cost multiplier", repository.AccountPatch{UpstreamCostMultiplierBp: new(2)}, false},
		{"template", repository.AccountPatch{TemplateID: new(int64(8))}, true},
		{"upstream key", repository.AccountPatch{UpstreamKey: new("sk-new")}, true},
		{"base url set", repository.AccountPatch{BaseURL: new("https://x.example.com")}, true},
		{"proxy bound", repository.AccountPatch{ProxyID: new(int64(9))}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, repository.ChangedFields(tc.patch, base).IdentityChanged())
		})
	}

	// 清空 base_url（&"" = 落 NULL）是身份变更：旧值非 NULL 时清空换了生效 origin。
	cleared := repository.ChangedFields(repository.AccountPatch{BaseURL: new("")}, repository.AccountFieldValues{BaseURL: new("https://x.example.com")})
	require.True(t, cleared.IdentityChanged(), "clearing a set base_url changes the effective origin")
	// 旧值本就是 NULL 时"清空"是幂等重写。
	idempotent := repository.ChangedFields(repository.AccountPatch{BaseURL: new("")}, repository.AccountFieldValues{BaseURL: nil})
	require.False(t, idempotent.IdentityChanged(), "clearing an already-NULL base_url is not a change")
	// 解绑（&0）在旧值已绑定时是身份变更；旧值本就是 NULL 时是幂等重写。
	unbound := repository.ChangedFields(repository.AccountPatch{ProxyID: new(int64(0))}, repository.AccountFieldValues{ProxyID: 3})
	require.True(t, unbound.IdentityChanged(), "unbinding a bound account changes the egress identity")
	// 已是直连（旧值 0 = NULL）时的"解绑"同款幂等：不推进 K。
	noopUnbind := repository.ChangedFields(repository.AccountPatch{ProxyID: new(int64(0))}, repository.AccountFieldValues{ProxyID: 0})
	require.False(t, noopUnbind.IdentityChanged(), "unbinding an already-direct account is not a change")
	// 旧值为 NULL、目标是具体代理 ⇒ 绑定是身份变更。
	bound := repository.ChangedFields(repository.AccountPatch{ProxyID: new(int64(7))}, repository.AccountFieldValues{ProxyID: 0})
	require.True(t, bound.IdentityChanged(), "binding a proxy changes the egress identity")
}
