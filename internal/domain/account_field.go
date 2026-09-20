// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// AccountField 是账号写面的字段标识（位序号）。它与 accountFieldSpecs **逐行
// 一一对应**：新增可写字段必须同时改表与本枚举，二者由
// TestAccountFieldSpecsMatchGeneratedPatch 机械断言（同时比对 openapi 生成的
// AccountConfigPatch 属性集）。
type AccountField uint16

const (
	FieldName AccountField = iota
	FieldTemplateID
	FieldBaseURL
	FieldUpstreamKey
	FieldMaxConcurrency
	FieldGroupIDs
	FieldEnabled
	FieldCacheDomain
	FieldUpstreamCostMultiplier
)

// FieldSet 是 AccountField 的位集合：一次写入**真实变更**到的字段。
type FieldSet uint32

// Has 报告集合是否含该字段。
func (s FieldSet) Has(f AccountField) bool { return s&(1<<f) != 0 }

// With 返回并入该字段后的集合。
func (s FieldSet) With(f AccountField) FieldSet { return s | 1<<f }

// Fields 按枚举序返回集合内的字段。
func (s FieldSet) Fields() []AccountField {
	out := make([]AccountField, 0, len(accountFieldSpecs))
	for _, spec := range accountFieldSpecs {
		if s.Has(spec.Field) {
			out = append(out, spec.Field)
		}
	}
	return out
}

// IdentityChanged 报告集合是否含**身份类**字段。身份类字段按值变更即更换路由
// 目标身份 ⇒ 推进身份代际 K ⇒ 在途工件（失效判决 / latch / 健康记录 /
// continuation）与客户端连接缓存必须作废。判据只来自声明表，不在调用点重写。
func (s FieldSet) IdentityChanged() bool {
	for _, spec := range accountFieldSpecs {
		if spec.Identity && s.Has(spec.Field) {
			return true
		}
	}
	return false
}

// IdentityFields 身份类字段集（由表派生，不是第二份字段清单）。
func IdentityFields() FieldSet {
	var s FieldSet
	for _, spec := range accountFieldSpecs {
		if spec.Identity {
			s = s.With(spec.Field)
		}
	}
	return s
}

// AccountFieldSpec 是字段声明表的一行。
type AccountFieldSpec struct {
	Field    AccountField
	Name     string // API 字段名（与 openapi 的 AccountConfigPatch 属性同名）
	Identity bool   // 身份类：按值变更 ⇒ 推进 K + 作废在途工件
}

// accountFieldSpecs 是账号写面的**单一事实来源**：字段的类别与后果只在此声明。
// 三个消费点都从本表读取，不得各自重写一份字段清单——
//  1. repository 的变更集计算（ChangedFields）：按值比较锁定行旧值；
//  2. service 的失效判定（clients / 在途工件）：IdentityChanged 派生；
//  3. scheduler 的静态键字段集断言：身份类字段必须在键内。
var accountFieldSpecs = [...]AccountFieldSpec{
	{FieldName, "name", false},
	{FieldTemplateID, "template_id", true},
	{FieldBaseURL, "base_url", true},
	{FieldUpstreamKey, "upstream_key", true},
	{FieldMaxConcurrency, "max_concurrency", false},
	{FieldGroupIDs, "group_ids", false},
	{FieldEnabled, "enabled", false},
	{FieldCacheDomain, "cache_domain", false},
	{FieldUpstreamCostMultiplier, "upstream_cost_multiplier", false},
}

// AccountFieldSpecs 返回声明表的副本（顺序 = 枚举序）。
func AccountFieldSpecs() []AccountFieldSpec {
	out := make([]AccountFieldSpec, len(accountFieldSpecs))
	copy(out, accountFieldSpecs[:])
	return out
}
