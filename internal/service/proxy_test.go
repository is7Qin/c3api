// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// newProxyTestService 代理写面测试的固定装配：fake store + 失效记录（代理逻辑
// 不碰 DB/墙钟；时钟按需注入 statsNow）。
func newProxyTestService() (*Service, *fakeStore, *invRecorder) {
	fs := newFakeStore()
	rec := &invRecorder{}
	return &Service{store: fs, inv: rec, log: nil}, fs, rec
}

// validProxyInput 合法基线（子用例只改一个字段，便于定位越界的**那一条**）。
func validProxyInput() ProxyInput {
	return ProxyInput{
		Name:   "p1",
		Scheme: domain.ProxySchemeSOCKS5,
		Host:   "10.0.0.1",
		Port:   1080,
	}
}

// TestCreateProxyValidation 校验矩阵（spec §3.5 的固定顺序与边界）：非法一律
// ErrInvalidInput（400），且**不落库、不触发失效**；合法基线能建、并按
// KindTemplates 全量 reload（spec §8）。
func TestCreateProxyValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		mutate  func(in *ProxyInput)
		wantErr bool
	}{
		{"baseline", func(*ProxyInput) {}, false},
		// 1. name：trim 后 1..64。
		{"name empty", func(in *ProxyInput) { in.Name = "" }, true},
		{"name blank", func(in *ProxyInput) { in.Name = "   " }, true},
		{"name 64 runes", func(in *ProxyInput) { in.Name = strings.Repeat("n", 64) }, false},
		{"name 65 runes", func(in *ProxyInput) { in.Name = strings.Repeat("n", 65) }, true},
		{"name trimmed", func(in *ProxyInput) { in.Name = "  padded  " }, false},
		// 2. scheme：小写后命中白名单；socks5h 不折成 socks5。
		{"scheme uppercase", func(in *ProxyInput) { in.Scheme = domain.ProxyScheme("SOCKS5") }, false},
		{"scheme socks5h kept", func(in *ProxyInput) { in.Scheme = domain.ProxyScheme("SOCKS5H") }, false},
		{"scheme socks4", func(in *ProxyInput) { in.Scheme = domain.ProxyScheme("socks4") }, true},
		{"scheme empty", func(in *ProxyInput) { in.Scheme = "" }, true},
		// 3. host：trim、去 IPv6 方括号、拒绝分隔符/空白/控制字符、<= 253。
		{"host empty", func(in *ProxyInput) { in.Host = "" }, true},
		{"host blank", func(in *ProxyInput) { in.Host = "  " }, true},
		{"host slash", func(in *ProxyInput) { in.Host = "10.0.0.1/24" }, true},
		{"host at sign", func(in *ProxyInput) { in.Host = "user@10.0.0.1" }, true},
		{"host inner space", func(in *ProxyInput) { in.Host = "10.0.0 .1" }, true},
		{"host control char", func(in *ProxyInput) { in.Host = "10.0.0.1\x00" }, true},
		{"host 253 bytes", func(in *ProxyInput) { in.Host = strings.Repeat("h", 253) }, false},
		{"host 254 bytes", func(in *ProxyInput) { in.Host = strings.Repeat("h", 254) }, true},
		{"host ipv6 bracketed", func(in *ProxyInput) { in.Host = "[::1]" }, false},
		{"host ipv6 bare", func(in *ProxyInput) { in.Host = "::1" }, false},
		{"host double bracketed", func(in *ProxyInput) { in.Host = "[[::1]]" }, true},
		{"host fqdn", func(in *ProxyInput) { in.Host = "proxy.example.com" }, false},
		// 4. port 1..65535。
		{"port negative", func(in *ProxyInput) { in.Port = -1 }, true},
		{"port zero", func(in *ProxyInput) { in.Port = 0 }, true},
		{"port 65535", func(in *ProxyInput) { in.Port = 65535 }, false},
		{"port 65536", func(in *ProxyInput) { in.Port = 65536 }, true},
		// 5. username/password：都空 = 无认证；username 非空时 password 可空。
		{"username without password", func(in *ProxyInput) { in.Username = new("u1") }, false},
		{"username control char", func(in *ProxyInput) { in.Username = new("u\x00") }, true},
		{"password control char", func(in *ProxyInput) { in.Password = new("p\x7f") }, true},
		{"empty username means no auth", func(in *ProxyInput) { in.Username = new(""); in.Password = new("") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, fs, rec := newProxyTestService()
			in := validProxyInput()
			tc.mutate(&in)
			id, err := svc.CreateProxy(ctx, in)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidInput, "校验失败一律 ErrInvalidInput（400）")
				require.Zero(t, id)
				require.Zero(t, rec.total(), "校验失败不得触发失效")
				list, lerr := fs.ListProxies(ctx)
				require.NoError(t, lerr)
				require.Empty(t, list, "校验失败不得落库")
				return
			}
			require.NoError(t, err)
			require.NotZero(t, id)
			require.Equal(t, 1, rec.countKind("templates"),
				"代理写面 → 全量 reload（KindTemplates → Sched.InvalidateAll，spec §8）")
			require.Zero(t, rec.countKind("accounts"), "代理变更不走账号代际/组级定向")
		})
	}
}

// TestCreateProxyNormalizes 归一化的三个落点：name 去首尾空白、scheme 消大小写
// （**不折 socks5h**）、host 去 IPv6 方括号；认证原样落值（凭据不 trim）。
func TestCreateProxyNormalizes(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()

	username, password := "user name", " pass with spaces "
	id, err := svc.CreateProxy(ctx, ProxyInput{
		Name: "  norm  ", Scheme: domain.ProxyScheme("SOCKS5H"), Host: "  [2001:db8::1]  ",
		Port: 1080, Username: &username, Password: &password,
	})
	require.NoError(t, err)

	got, err := svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "norm", got.Name, "name 落 trim 后的值")
	require.Equal(t, domain.ProxySchemeSOCKS5H, got.Scheme, "socks5h 不归一成 socks5")
	require.Equal(t, "2001:db8::1", got.Host, "IPv6 落库不带方括号")
	require.Equal(t, "user name", got.Username, "凭据原样落值（不 trim）")
	require.Equal(t, " pass with spaces ", got.Password)
	require.Nil(t, got.LastProbeAt, "从未探测 = nil")
}

// TestCreateProxyDefaultsAndConflict enabled 缺省 true、显式 false 生效；重名 →
// ErrConflict（409，含 name 详情），与 template/group 同一条映射路径。
func TestCreateProxyDefaultsAndConflict(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	id, err := svc.CreateProxy(ctx, validProxyInput())
	require.NoError(t, err)
	got, err := svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.True(t, got.Enabled, "enabled 缺省 true（spec §3.1）")

	in := validProxyInput()
	in.Name = "p-disabled"
	in.Enabled = new(false)
	disabledID, err := svc.CreateProxy(ctx, in)
	require.NoError(t, err)
	disabled, err := svc.GetProxy(ctx, disabledID)
	require.NoError(t, err)
	require.False(t, disabled.Enabled, "显式 false 生效")

	before := rec.total()
	_, err = svc.CreateProxy(ctx, validProxyInput()) // 同名
	require.ErrorIs(t, err, ErrConflict, "重名 → ErrConflict（409）")
	require.Contains(t, err.Error(), `name="p1"`, "409 消息含冲突详情")
	require.Equal(t, before, rec.total(), "冲突路径不落库、不失效")
}

// TestUpdateProxyFieldSemantics 全量更新非密字段：提供的落值、nil 的保持原值；
// enabled 翻转同样触发全量 reload。
func TestUpdateProxyFieldSemantics(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	username := "u1"
	id, err := svc.CreateProxy(ctx, ProxyInput{
		Name: "old", Scheme: domain.ProxySchemeHTTP, Host: "10.0.0.1", Port: 8080,
		Username: &username, Enabled: new(false), Remark: new("keep"),
	})
	require.NoError(t, err)

	// 只改名与端口：username/remark/enabled 三项 nil = 不变。
	before := rec.total()
	require.NoError(t, svc.UpdateProxy(ctx, id, ProxyInput{
		Name: "renamed", Scheme: domain.ProxySchemeHTTPS, Host: "10.0.0.2", Port: 8443,
	}))
	require.Equal(t, before+1, rec.total())
	require.Equal(t, 2, rec.countKind("templates"), "创建 + 更新各一次全量 reload")

	got, err := svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
	require.Equal(t, domain.ProxySchemeHTTPS, got.Scheme)
	require.Equal(t, "10.0.0.2", got.Host)
	require.Equal(t, 8443, got.Port)
	require.Equal(t, "u1", got.Username, "username=nil ⇒ 不变")
	require.Equal(t, "keep", got.Remark, "remark=nil ⇒ 不变")
	require.False(t, got.Enabled, "enabled=nil ⇒ 不变")

	// 显式落值：username 清空、remark 清空、enabled 翻转。
	require.NoError(t, svc.UpdateProxy(ctx, id, ProxyInput{
		Name: "renamed", Scheme: domain.ProxySchemeHTTPS, Host: "10.0.0.2", Port: 8443,
		Username: new(""), Remark: new(""), Enabled: new(true),
	}))
	got, err = svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Empty(t, got.Username, "username=&\"\" ⇒ 清空")
	require.Empty(t, got.Remark, "remark=&\"\" ⇒ 清空")
	require.True(t, got.Enabled, "enabled 翻转生效（含 enabled 翻转 ⇒ 全量 reload）")
}

// TestUpdateProxyPasswordTriState 密码三态（spec §6.1）：nil = 不变、&"" = 清空、
// &v = 落值；非密字段同期照常覆盖。
func TestUpdateProxyPasswordTriState(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	username, password := "u1", "secret-1"
	id, err := svc.CreateProxy(ctx, ProxyInput{
		Name: "tri", Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.1", Port: 1080,
		Username: &username, Password: &password,
	})
	require.NoError(t, err)
	before := rec.total()

	// 1) nil = 不变（同时改非密字段，证明「不变」只作用于密码列）。
	require.NoError(t, svc.UpdateProxy(ctx, id, ProxyInput{
		Name: "tri", Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.9", Port: 1081,
	}))
	got, err := svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "secret-1", got.Password, "password=nil ⇒ 旧密码原样保留")
	require.Equal(t, "u1", got.Username, "username=nil ⇒ 不变")
	require.Equal(t, "10.0.0.9", got.Host, "非密字段照改")

	// 2) &"" = 清空。
	require.NoError(t, svc.UpdateProxy(ctx, id, ProxyInput{
		Name: "tri", Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.9", Port: 1081,
		Password: new(""),
	}))
	got, err = svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Empty(t, got.Password, "password=&\"\" ⇒ 清空")
	require.Equal(t, "u1", got.Username, "清密码不动 username")

	// 3) &v = 落值。
	require.NoError(t, svc.UpdateProxy(ctx, id, ProxyInput{
		Name: "tri", Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.9", Port: 1081,
		Password: new("secret-2"),
	}))
	got, err = svc.GetProxy(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "secret-2", got.Password)
	require.Equal(t, before+3, rec.total(), "三次更新各触发一次全量 reload（改密码也在其列）")
}

// TestUpdateProxyNotFoundAndValidate 不存在/已软删 → 404；非法入参在校验处即被拒
// （400 优先于 404，与 CreateAccount 的「先校验后取行」同序）。
func TestUpdateProxyNotFoundAndValidate(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	err := svc.UpdateProxy(ctx, 999, validProxyInput())
	require.ErrorIs(t, err, ErrNotFound, "缺 id → 404")
	require.Contains(t, err.Error(), "id=999", "404 带缺失 id 详情")
	require.Zero(t, rec.total())

	bad := validProxyInput()
	bad.Port = 0
	require.ErrorIs(t, svc.UpdateProxy(ctx, 999, bad), ErrInvalidInput, "校验先于取行")

	id, err := svc.CreateProxy(ctx, validProxyInput())
	require.NoError(t, err)
	require.NoError(t, svc.DeleteProxy(ctx, id))
	require.ErrorIs(t, svc.UpdateProxy(ctx, id, validProxyInput()), ErrNotFound, "已软删 = 不存在")
}

// TestDeleteProxy 删除 → 失效通知；删除后 Get/再次 Delete 均 404；绑定账号被
// 同事务解绑（回落直连）。
func TestDeleteProxy(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	id, err := svc.CreateProxy(ctx, validProxyInput())
	require.NoError(t, err)
	tpl := seedTemplate(t, svc, "tpl-proxy-delete")
	acc := seedAccount(t, svc, tpl.ID, "acc-proxy-delete")
	proxyID := id
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{ProxyID: &proxyID}, nil)
	require.NoError(t, err)
	bound, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, bound.ProxyID)
	require.Equal(t, id, *bound.ProxyID)

	before := rec.total()
	require.NoError(t, svc.DeleteProxy(ctx, id))
	require.Equal(t, before+1, rec.total(), "删除 → 全量 reload")

	_, err = svc.GetProxy(ctx, id)
	require.ErrorIs(t, err, ErrNotFound, "删除后 GetProxy → 404")
	require.ErrorIs(t, svc.DeleteProxy(ctx, id), ErrNotFound, "重复删除不静默成功")
	list, err := svc.ListProxies(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "软删条目不出现在列表")

	unbound, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, unbound.ProxyID, "删除代理 ⇒ 同事务解绑（proxy_id 落 NULL = 直连）")
}

// TestProbeProxyUnreachable 不可达地址：返回条目 + LastProbeOK=false + 脱敏错误
// 文本，且 enabled 不变、探测时刻来自注入时钟（不赌墙钟毫秒差）、不触发失效。
func TestProbeProxyUnreachable(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()
	fixed := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	svc.statsNow = func() time.Time { return fixed }

	username, password := "probe-user", "probe-secret"
	id, err := svc.CreateProxy(ctx, ProxyInput{
		Name: "unreachable", Scheme: domain.ProxySchemeSOCKS5, Host: "127.0.0.1",
		Port: closedTCPPort(t), Username: &username, Password: &password,
	})
	require.NoError(t, err)
	before := rec.total()

	got, err := svc.ProbeProxy(ctx, id)
	require.NoError(t, err, "拨不通是探测结论，不是 API 失败")
	require.False(t, got.LastProbeOK)
	require.NotEmpty(t, got.LastProbeError, "失败原因落库（运维唯一现场证据）")
	require.NotContains(t, got.LastProbeError, password, "错误文本不得含密码（§5.2）")
	require.NotContains(t, got.LastProbeError, username, "错误文本不得含用户名")
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(fixed), "探测时刻来自注入时钟（want %v, got %v）", fixed, got.LastProbeAt)
	require.True(t, got.Enabled, "探测结果不影响 enabled（§6.3：enabled 是唯一开关）")
	require.Equal(t, before, rec.total(), "探测不改路由 ⇒ 不触发失效")
}

// TestProbeProxyReachable 探测成功的正例：本机临时监听端口（t.Cleanup 关闭），
// 三元组落成 ok=true / 空错误文本。
func TestProbeProxyReachable(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	fixed := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	svc.statsNow = func() time.Time { return fixed }

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	id, err := svc.CreateProxy(ctx, ProxyInput{
		Name: "reachable", Scheme: domain.ProxySchemeHTTP, Host: host, Port: port,
	})
	require.NoError(t, err)

	got, err := svc.ProbeProxy(ctx, id)
	require.NoError(t, err)
	require.True(t, got.LastProbeOK, "监听中的端口可拨通")
	require.Empty(t, got.LastProbeError)
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(fixed))

	// 探测不含认证校验（§6.3）：这条条目无凭据也能拨通。
	require.Empty(t, got.Username)
}

// TestProbeProxyNotFound 缺 id → 404（探测写回也走同一映射）。
func TestProbeProxyNotFound(t *testing.T) {
	svc, _, _ := newProxyTestService()
	_, err := svc.ProbeProxy(context.Background(), 4242)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestProxyRedactedError 脱敏是**显式**动作而不是「相信 dial 错误天然干净」：
// 认证型代理的握手错误可以带 userinfo，而这段文本会落进 last_probe_error 并回显。
func TestProxyRedactedError(t *testing.T) {
	p := domain.Proxy{Username: "alice", Password: "s3cr3t"}
	err := errors.New(`proxyconnect tcp: dial tcp: socks5://alice:s3cr3t@10.0.0.9:1080 refused`)
	text := proxyRedactedError(err, p)
	require.NotContains(t, text, "s3cr3t")
	require.NotContains(t, text, "alice")
	require.Contains(t, text, "***", "被替换而非丢弃上下文")
	require.Contains(t, text, "10.0.0.9:1080", "地址等排障信息保留")
}

// closedTCPPort 取一个「刚被关闭」的本机端口（拨测必然失败）。
func closedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return port
}
