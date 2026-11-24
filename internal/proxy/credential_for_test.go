// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/credential"
)

// 本文件替代被删除的凭据分发恒等层，锁住 credentialFor 的全部行为。
//
// 那个层的形状是「类型 → Provider」查表：表里注册的三个 provider 取凭据时都是
// `if in.Type != p.typ { err }; return in.APIKey`，且按类型查到的 provider 的
// typ 恒等于查询类型 ⇒ 类型不匹配分支在生产路径不可达（恒等层）。删掉它之后，
// 行为由三条断言接管（也是删除面上的等价证据）：
//  1. 静态三类型原样返回 Selection.UpstreamKey（含空 key 不替换）；
//  2. 合法但非静态的类型被 ErrUnsupported 包裹拒绝，不吐凭据值；
//  3. 表外类型被拒绝，且**不**包裹 ErrUnsupported——两条错误路径不被合并。
//
// 三条都是纯函数级断言（零值 Proxy + 合成 Selection，不打网络、不碰 PG）。

// TestCredentialForReturnsUpstreamKey 静态通道 = 账号 upstream_key 直读：三种静态
// 类型 × {非空 key, 空 key} 全部原样返回（空 key 不在这里校验——非空约束在写面
// service/account.go，取用面吐空是既有契约）。
func TestCredentialForReturnsUpstreamKey(t *testing.T) {
	static := []credential.Type{credential.TypeAPIKey, credential.TypeResponsesSpecial, credential.TypeCustom}
	keys := []string{"sk-upstream", ""}
	for _, typ := range static {
		for _, key := range keys {
			got, err := (&Proxy{}).credentialFor(&api.Selection{CredentialType: typ, UpstreamKey: key})
			require.NoError(t, err, "type=%q key=%q", typ, key)
			require.Equal(t, key, got, "静态通道必须原样返回 Selection.UpstreamKey（type=%q）", typ)
		}
	}
}

// TestCredentialForRejectsNonStatic 合法类型走错（静态）路径：拒绝并包裹
// ErrUnsupported，绝不把上游凭据换成空串静默发出。正常分流下不可达（codex/bps 在
// ChannelOf 分支就被拦下），这是纵深防御契约。
func TestCredentialForRejectsNonStatic(t *testing.T) {
	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT, credential.TypeBPS} {
		got, err := (&Proxy{}).credentialFor(&api.Selection{CredentialType: typ, UpstreamKey: "sk-must-not-leak"})
		require.Error(t, err, "type=%q 不得在静态通道取到凭据", typ)
		require.Empty(t, got, "type=%q 失败时不得吐凭据值", typ)
		require.ErrorIs(t, err, credential.ErrUnsupported, "type=%q", typ)
		require.Contains(t, err.Error(), string(typ), "错误必须点名类型（排查靠它）")
	}
	// 逐字节锚点：旧 Registry 兜底 provider 走的是同一条 `%w: %q` 形状——下游
	// errors.Is 与日志文本都不因删除分发层而变。
	_, err := (&Proxy{}).credentialFor(&api.Selection{CredentialType: credential.TypeCodexOAuth})
	require.EqualError(t, err, `credential: unsupported credential type: "codex-oauth"`)
}

// TestCredentialForRejectsUnknownType 表外类型（部署/配置错）：拒绝，且错误**不**
// 包裹 ErrUnsupported——它是两条错误路径不合并的唯一证据。合进上面一条会让「配错
// 类型」与「路由错」在 errors.Is 与日志里同形，排查时分不开。
//
// 形式上可达（ChannelOf 对表外类型落 default 静态分支），但正常流量更早被候选指纹
// 门拒掉（TestProxyCredentialUnknownTypeRejectsNoUpstreamCall 证明上游零请求）。
func TestCredentialForRejectsUnknownType(t *testing.T) {
	for _, typ := range []credential.Type{"", "bogus"} {
		got, err := (&Proxy{}).credentialFor(&api.Selection{CredentialType: typ, UpstreamKey: "sk-x"})
		require.Error(t, err, "type=%q 必须被拒", typ)
		require.Empty(t, got)
		require.False(t, errors.Is(err, credential.ErrUnsupported),
			"表外类型的错误不得包裹 ErrUnsupported（type=%q）：合进 ErrUnsupported 就再也分不开「配错」与「路由错」", typ)
		require.Contains(t, err.Error(), "unsupported credential type")
	}
	// 逐字节锚点：不带 `credential: ` 前缀、不被 sentinel 包裹——与上面那条不同形。
	_, err := (&Proxy{}).credentialFor(&api.Selection{CredentialType: "bogus"})
	require.EqualError(t, err, `unsupported credential type "bogus"`)
}
