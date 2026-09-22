// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package credential

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypeValid(t *testing.T) {
	require.True(t, TypeAPIKey.Valid())
	require.False(t, Type("").Valid(), "空类型不合法")
	require.False(t, Type("codex_oauth").Valid(), "未注册生态类型不合法（占位不生效）")
	require.False(t, Type("bogus").Valid())
}

func TestNewRegistersAPIKey(t *testing.T) {
	r := New()
	require.NotNil(t, r.For(TypeAPIKey), "New 必须默认注册 api_key")
	require.Equal(t, TypeAPIKey, r.For(TypeAPIKey).Type())
}

func TestAPIKeyProviderReturnsAPIKey(t *testing.T) {
	p := staticKeyProvider{TypeAPIKey}
	got, err := p.Credential(context.Background(), CredentialInput{
		AccountID: 1, Type: TypeAPIKey, APIKey: "sk-x",
	})
	require.NoError(t, err)
	require.Equal(t, "sk-x", got)
	// 空 Key 也原样返回（行为与现状一致——非空校验在别处）
	got, err = p.Credential(context.Background(), CredentialInput{AccountID: 1, Type: TypeAPIKey})
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestAPIKeyProviderTypeMismatchErrors(t *testing.T) {
	p := staticKeyProvider{TypeAPIKey}
	_, err := p.Credential(context.Background(), CredentialInput{AccountID: 1, Type: "codex_oauth", APIKey: "sk-x"})
	require.ErrorIs(t, err, ErrUnsupported)
	require.Contains(t, err.Error(), "codex_oauth")
}

func TestRegistryRegisterAndFor(t *testing.T) {
	r := New()
	// 覆盖默认 api_key 注册：For 必须返回新注册的 provider
	custom := &stubProvider{typ: TypeAPIKey, val: "sk-custom"}
	r.Register(custom)
	require.Same(t, custom, r.For(TypeAPIKey))
	got, err := r.For(TypeAPIKey).Credential(context.Background(), CredentialInput{Type: TypeAPIKey, APIKey: "sk-old"})
	require.NoError(t, err)
	require.Equal(t, "sk-custom", got, "覆盖后凭据值来自新 provider，而非输入 APIKey")
}

// TestNewRegistersResponsesSpecial New 必须默认注册 responses-special provider
// （修复前未注册 → For 兜底 apiKeyProvider → Credential 类型不匹配报错 →
// 真实流量 502 unsupported credential type）。
func TestNewRegistersResponsesSpecial(t *testing.T) {
	r := New()
	p := r.For(TypeResponsesSpecial)
	require.NotNil(t, p, "New 必须默认注册 responses-special")
	require.Equal(t, TypeResponsesSpecial, p.Type())
	// 经注册表取凭据（与 credentialFor 同路径）：匹配类型直读静态 Key
	got, err := r.For(TypeResponsesSpecial).Credential(context.Background(),
		CredentialInput{AccountID: 1, Type: TypeResponsesSpecial, APIKey: "sk-x"})
	require.NoError(t, err)
	require.Equal(t, "sk-x", got)
}

func TestResponsesSpecialProviderReturnsAPIKey(t *testing.T) {
	p := staticKeyProvider{TypeResponsesSpecial}
	got, err := p.Credential(context.Background(), CredentialInput{
		AccountID: 1, Type: TypeResponsesSpecial, APIKey: "sk-x",
	})
	require.NoError(t, err)
	require.Equal(t, "sk-x", got)
	// 空 Key 也原样返回（同 api_key——非空校验在别处）
	got, err = p.Credential(context.Background(), CredentialInput{AccountID: 1, Type: TypeResponsesSpecial})
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestResponsesSpecialProviderTypeMismatchErrors(t *testing.T) {
	p := staticKeyProvider{TypeResponsesSpecial}
	for _, typ := range []Type{TypeAPIKey, TypeCodexOAuth, Type("bogus")} {
		_, err := p.Credential(context.Background(), CredentialInput{AccountID: 1, Type: typ, APIKey: "sk-x"})
		require.ErrorIs(t, err, ErrUnsupported, "type %q 必须防御报错", typ)
		require.Contains(t, err.Error(), string(typ))
	}
}

// For 未知类型 → unsupportedProvider 兜底（Valid 通过但未注册的类型也走此路
// 径）：Type() 必须返回**真实请求类型**（旧兜底复用 apiKeyProvider
// 恒返回 TypeAPIKey 是撒谎语义，未来 codex HTTP 面注册时错配咬合点）；其
// Credential 恒 ErrUnsupported（错误文本含输入类型，与现状一致）——兜底语义
// 不回归。
func TestRegistryForUnknownType(t *testing.T) {
	r := New()
	p := r.For(Type("bogus"))
	require.Equal(t, Type("bogus"), p.Type(), "兜底 provider Type() 必须返回真实类型（不再撒谎回退 api_key）")
	_, err := p.Credential(context.Background(), CredentialInput{Type: "bogus", APIKey: "sk-x"})
	require.ErrorIs(t, err, ErrUnsupported)
	require.Contains(t, err.Error(), "bogus")
	// Valid 通过但未注册的生态类型（codex-oauth/codex-pat——注册表未注册，凭据
	// 走快照派生直供适配层）同样落兜底：Type() 真实、Credential 显式拒绝。
	for _, typ := range []Type{TypeCodexOAuth, TypeCodexPAT} {
		po := r.For(typ)
		require.Equal(t, typ, po.Type(), "codex 类型兜底 Type() 必须返回真实类型")
		_, err = po.Credential(context.Background(), CredentialInput{Type: typ, APIKey: "sk-x"})
		require.ErrorIs(t, err, ErrUnsupported)
		require.Contains(t, err.Error(), string(typ))
	}
}

type stubProvider struct {
	typ Type
	val string
}

func (s stubProvider) Type() Type { return s.typ }
func (s stubProvider) Credential(_ context.Context, in CredentialInput) (string, error) {
	return s.val, nil
}
