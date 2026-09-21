// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package credential 是账号凭据抽象层：类型注册表 + Provider 分发，为后续
// 多种号池类型（codex oauth、codex personal_access_token、claude code 等）
// 打地基。本轮实现接口 + api_key/responses-special 两 provider + 接线。
//
// 正交原则：Provider 只返回凭据值（key/token），不感知请求格式——鉴权头名由
// 格式侧决定（openai → Authorization: Bearer、anthropic → x-api-key，
// aiclient 现状已按格式分头）。
package credential

import (
	"context"
	"errors"
	"fmt"
)

// Type 账号凭据类型：按生态命名（api_key = 静态 Key + 模板 BaseURL，现状语义）。
type Type string

const (
	// TypeAPIKey 默认类型：静态 Key（调度 Selection 携带的 UpstreamKey）。
	TypeAPIKey Type = "api_key"
	// TypeResponsesSpecial Responses 特殊需求模板（图像 tool 剥离等开关）；
	// 只支持 resp / resp-ws 格式（service 校验），配置挂 template_ext。
	TypeResponsesSpecial Type = "responses-special"
	// TypeCodexOAuth Codex OAuth 账号池模板/账号；配置挂 ext 子表，鉴权
	// 接 SDK ws。
	TypeCodexOAuth Type = "codex-oauth"
	// TypeCodexPAT Codex PAT 账号池模板/账号；配置挂 ext 子表，鉴权接 SDK ws。
	TypeCodexPAT Type = "codex-pat"
)

// Valid 类型是否已注册可用的凭据类型（模板主列 credential_type 全量：api_key
// 四格式任意；生态三类型只支持 resp/resp-ws——service 校验）。未知类型（含
// 空串）→ false。
func (t Type) Valid() bool {
	switch t {
	case TypeAPIKey, TypeResponsesSpecial, TypeCodexOAuth, TypeCodexPAT:
		return true
	}
	return false
}

// ValidTemplateExt 模板 ext 子表可用类型（api_key 类型模板无 ext 行——主列已
// 表达静态 Key 语义）。
func (t Type) ValidTemplateExt() bool {
	switch t {
	case TypeResponsesSpecial, TypeCodexOAuth, TypeCodexPAT:
		return true
	}
	return false
}

// ValidAccountExt 账号 ext 子表可用类型（账号只两种 codex 类型）。
func (t Type) ValidAccountExt() bool {
	switch t {
	case TypeCodexOAuth, TypeCodexPAT:
		return true
	}
	return false
}

// CredentialInput 凭据取用输入：api_key 类型用 APIKey（调度 Selection 携带
// 的静态 Key）；oauth 等类型后续按 AccountID 从凭据扩展表加载。
type CredentialInput struct {
	AccountID int64
	Type      Type
	APIKey    string // api_key 类型的静态 Key
}

// Provider 凭据提供者：Credential 返回当前可用的凭据值（api_key → key；
// oauth → access_token）。鉴权头名由转发器按请求格式决定，provider 不感知
// 格式（正交）。
type Provider interface {
	Type() Type
	Credential(ctx context.Context, in CredentialInput) (string, error)
}

// ErrUnsupported 未知/未注册的凭据类型。
var ErrUnsupported = errors.New("credential: unsupported credential type")

// staticKeyProvider 静态 Key 直读 provider（api_key / responses-special 两类型
// 共用——消两份逐字同构 provider：key 来源同为账号 upstream_key，差异
// 仅类型标记，注册表按类型分发）。直接返回 CredentialInput.APIKey（空 Key 也
// 原样返回——行为与现状一致，Key 非空校验在别处）。类型不匹配 → 错误（防御
// 性：For 的兜底路径也会命中——未注册的 Valid 类型落回 unsupportedProvider，
// 显式报错而非吐值，杜绝号池类型凭据错配）。
type staticKeyProvider struct{ typ Type }

func (p staticKeyProvider) Type() Type { return p.typ }

func (p staticKeyProvider) Credential(_ context.Context, in CredentialInput) (string, error) {
	if in.Type != p.typ {
		return "", fmt.Errorf("%w: %q", ErrUnsupported, in.Type)
	}
	return in.APIKey, nil
}

// Registry 类型 → Provider 分发表。
type Registry struct{ m map[Type]Provider }

// New 构造注册表并默认注册 api_key + responses-special 两 provider。
func New() *Registry {
	r := &Registry{m: make(map[Type]Provider)}
	r.Register(staticKeyProvider{TypeAPIKey})
	r.Register(staticKeyProvider{TypeResponsesSpecial})
	return r
}

// Register 注册/覆盖指定类型的 provider。
func (r *Registry) Register(p Provider) { r.m[p.Type()] = p }

// unsupportedProvider 未注册类型兜底 provider（不再复用 apiKeyProvider
// ——旧兜底 Type() 恒返回 TypeAPIKey，对 TypeCodexOAuth 等撒谎，是未来 codex
// HTTP 面注册时错配咬合点）。Type() 返回**真实请求类型**；Credential 恒
// ErrUnsupported（错误文本与现状一致——含输入类型，显式报错不吐值，评审）。
type unsupportedProvider struct{ typ Type }

func (p unsupportedProvider) Type() Type { return p.typ }

func (unsupportedProvider) Credential(_ context.Context, in CredentialInput) (string, error) {
	return "", fmt.Errorf("%w: %q", ErrUnsupported, in.Type)
}

// For 取类型的 provider；未注册 → unsupportedProvider 兜底。兜底是安全网而非
// 静默 fallback：Valid() 通过但未注册的类型也会走到这里——兜底 provider 的
// Credential 恒返回 ErrUnsupported，显式报错不吐值。
func (r *Registry) For(t Type) Provider {
	if p, ok := r.m[t]; ok {
		return p
	}
	return unsupportedProvider{typ: t}
}
