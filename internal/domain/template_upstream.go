// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// TemplateUpstream 是 custom 模板的**出站上游修饰**（spec custom-request-patch
// §5.1/§5.3）：逐格式自定义路径与 request_patch **同源同门**——两者都只来自
// template_ext 行、都只对 credential_type=custom 生效，因此必须装进同一个结构体、
// 由同一个 custom 门一次构造（类型从 custom 改成别的类型时陈旧 ext 行不得半生效：
// 路径会打错端点、patch 会改写别的类型模板的请求体）。
//
// nil = 未配置（非 custom 模板，或 custom 但无 ext 配置）。**快照内只读**：
// Paths/Patch 都别名自 ent 读模型（不深拷贝），持有者只能读——两跳写
// （tpl.Upstream.Paths[...] = / tpl.Upstream.Patch = …）由
// internal/repository/template_immutability_gate_test.go 覆盖。
//
// 装进两个快照面位置：domain.Template.Upstream（选号快照）与
// domain.ProbeSpec.Upstream（探测执行）。数据面消费（proxy/probe）只读它，
// 不写、不复制 map/指针。
type TemplateUpstream struct {
	// Paths 逐格式自定义上游路径（原 template_ext.upstream_paths 语义）。
	// nil = 全部回落官方默认端点；值**原样**拼到模板 base_url 之后（不自动补
	// /v1、不改写），可带 query。
	Paths map[string]string
	// Patch 出站请求修饰器（template_ext.request_patch 的**读回解析值**）。
	// nil = 未配置；快照内只读（P3 数据面消费时也不得写）。
	Patch *RequestPatch
}

// NewTemplateUpstream 是 TemplateUpstream 的**唯一装配/归一入口**：空（无路径、
// 无 patch）⇒ nil。「未配置」只有一种形态，否则同一份空配置会在不同装载路径上
// 得到 nil / 空结构体两种指针值（digest 虽等价，但读路径的 nil 判断会分叉）。
//
// 调用点：repository.toDomainTemplate 的 custom 门（选号快照）与
// service.ProbeAccount 的 custom ext 组装（探测）。两处都必须走这里，不得各自
// 手搓结构体（Patch/Paths 同门是安全前提，不是便利）。
func NewTemplateUpstream(paths map[string]string, patch *RequestPatch) *TemplateUpstream {
	if len(paths) == 0 && patch == nil {
		return nil
	}
	return &TemplateUpstream{Paths: paths, Patch: patch}
}
