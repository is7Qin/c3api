// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 管理面（/admin）：排除 user 与 supplier tag（/ops/workers 运维观测并入管理面，
// tags: [ops] 正常生成——路由 chi-server 提供，鉴权走 /admin 组 adminAuth）；生成到本包。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -exclude-tags user,supplier -package handler -o api.gen.go ../../openapi/openapi.yaml
// 用户面（/user）：仅 user tag；独立包（共享 schema 类型在各自包内不冲突）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags user -package user -o user/api.gen.go ../../openapi/openapi.yaml
// 供应商面（/api/user/supplier）：仅 supplier tag；独立包——业务端点路径为绝对
// 路径（/api/user/supplier/*），故无独立 BaseURL，HandlerWithOptions 直接用 spec
// 路径（与 user 面包同款）。账号端点复用管理面生成路由，不经本包（见 router.go）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags supplier -package supplier -o supplier/api.gen.go ../../openapi/openapi.yaml
