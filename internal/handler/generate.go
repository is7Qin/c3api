// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 管理面（/admin）：排除 user 与 supplier tag（/ops/workers 运维观测并入管理面，
// tags: [ops] 正常生成——路由 chi-server 提供，鉴权走 /admin 组 adminAuth）；生成到本包。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -exclude-tags user,supplier -package handler -o api.gen.go ../../openapi/openapi.yaml
// 用户面（/user）：仅 user tag；独立包（共享 schema 类型在各自包内不冲突）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags user -package user -o user/api.gen.go ../../openapi/openapi.yaml
// 供应商面（/api/user/supplier）：仅 supplier tag；独立包——该 tag 的 path 一律写
// **绝对路径**（业务 5 op + §2.5 的账号/分组/模板子集 17 op，共 22 op），故**无独立
// BaseURL**，HandlerWithOptions 直接用 spec 路径（与 user 面包同款）。账号/分组/模板
// op 的 requestBody/响应/query 参数**全部 $ref 复用管理面 components**（零平行字段
// 清单）；生成的 supplier wrapper **仅供路由绑定**——解析参数**一次**后，supplier surface
// 类型化直调同一批 AdminAPI 实现（字段层零差异化，不再经管理面 ServerInterfaceWrapper）。
// **tag 即边界**：未登记为 supplier 的 path 不进本包 ⇒ 未注册 ⇒ 404（default-deny
// 是结构性的，不再有手写允许清单/guard）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags supplier -package supplier -o supplier/api.gen.go ../../openapi/openapi.yaml
