// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 管理面（/admin）：排除 user / supplier / payment / payment-admin 四个**非本包**
// 的 tag（/ops/workers 运维观测并入管理面，tags: [ops] 正常生成；支付订单 tags:
// [orders] 也属于管理面——paths 不带 /api/admin 前缀——故正常生成）。生成到本包。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -exclude-tags user,supplier,payment,payment-admin -package handler -o api.gen.go ../../openapi/openapi.yaml
// 用户面（/user）：仅 user tag；独立包（共享 schema 类型在各自包内不冲突）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags user -package user -o user/api.gen.go ../../openapi/openapi.yaml
// 供应商面（/api/user/supplier）：仅 supplier tag；独立包——该 tag 的 path 一律写
// **绝对路径**（业务 5 op + §2.5 的账号/分组/模板子集 17 op + spec-proxy-pool 的代理池
// 6 op，共 28 op），故**无独立
// BaseURL**，HandlerWithOptions 直接用 spec 路径（与 user 面包同款）。账号/分组/模板
// op 的 requestBody/响应/query 参数**全部 $ref 复用管理面 components**（零平行字段
// 清单）；生成的 supplier wrapper **仅供路由绑定**——解析参数**一次**后，supplier surface
// 类型化直调同一批 AdminAPI 实现（字段层零差异化，不再经管理面 ServerInterfaceWrapper）。
// **tag 即边界**：未登记为 supplier 的 path 不进本包 ⇒ 未注册 ⇒ 404（default-deny
// 是结构性的，不再有手写允许清单/guard）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags supplier -package supplier -o supplier/api.gen.go ../../openapi/openapi.yaml
// 支付公开面（/api/payment/{account}/*）：仅 payment tag；独立包——无身份鉴权（网关
// 无 JWT），由 server.go 以 `r.Handle("/api/payment/*", …)` 挂在**非 admin 鉴权组**。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags payment -package payment -o payment/api.gen.go ../../openapi/openapi.yaml
// 支付账户管理面（payment-admin）：仅 payment-admin tag；独立包——该 tag 的 paths
// **不带** /api/admin 前缀（同 orders），但账户/渠道 CRUD 与订单运维面解耦，故单独
// 生成到 paymentadmin 包（server.go 以管理面鉴权组挂载；默认不进主 handler 包）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags payment-admin -package paymentadmin -o paymentadmin/api.gen.go ../../openapi/openapi.yaml
