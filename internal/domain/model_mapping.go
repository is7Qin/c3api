// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// LookupModelMapping 取客户端模型名的映射条目（**唯一命中规则**：scheduler
// 的两个活跃映射读点与 account-model-test 的探测执行都调用本函数，不得就地
// 各写一份 `tpl.ModelMapping[m]`）。
//
// nil 模板 / 未命中 → ok=false；命中时返回条目原值（MappedModel 与 Mode 均由
// 调用方按需消费）。
func LookupModelMapping(t *Template, requested string) (ModelMappingEntry, bool) {
	if t == nil {
		return ModelMappingEntry{}, false
	}
	e, ok := t.ModelMapping[requested]
	return e, ok
}

// MapModelEntry 调度映射读点的完整形态：命中 → 条目的 (MappedModel, Mode)；
// 未命中/nil 模板 → (原模型名, invalid)。两个活跃调度读点与探测都用这一条
// 命中规则（映射条目里的空 MappedModel 按原值返回，与就地读取行为一致）。
func MapModelEntry(t *Template, requested string) (string, ModelMappingMode) {
	if e, ok := LookupModelMapping(t, requested); ok {
		return e.MappedModel, e.Mode
	}
	return requested, ModelMappingModeInvalid
}

// MapModel 探测用的映射结果：命中 → 条目里的上游模型名；未命中/nil 模板 →
// 原样返回客户端模型名（与 MapModelEntry 同一命中规则）。
func MapModel(t *Template, requested string) string {
	m, _ := MapModelEntry(t, requested)
	return m
}
