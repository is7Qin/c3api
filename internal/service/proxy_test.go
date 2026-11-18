// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// probeEgressStub 第二段（经代理取出口 IP）的注入桩（spec §3.5 的可测缝）：计数 + 固定
// 返回；observe 非 nil 时先看 ctx 并可返回错误（模拟真实 HTTP 请求一取消就失败——不观察
// ctx 就显不出「第二段继承了请求 ctx」这个缺陷）。真跑第二段要连 api.ip.sb，单测不能依赖它。
//
// 计数器不加锁：本包测试串行（AGENTS.md 全仓禁用并行原语）。
type probeEgressStub struct {
	calls   int
	ip      string
	err     error
	observe func(context.Context) error
}

// fn 返回可直接赋给 Service.probeEgress 的桩函数。
func (s *probeEgressStub) fn() func(context.Context, domain.Proxy) (string, error) {
	return func(ctx context.Context, _ domain.Proxy) (string, error) {
		s.calls++
		if s.observe != nil {
			if err := s.observe(ctx); err != nil {
				return "", err
			}
		}
		return s.ip, s.err
	}
}

// listeningTCPAddr 开一个本机临时监听端口（t.Cleanup 关闭）：第一段 TCP 能真拨通，
// 第二段交给注入桩（否则它会真的去连 api.ip.sb 等 10s）。
func listeningTCPAddr(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

// probeFixtureNow 探测夹具的注入时刻（statsNow）：用例不赌墙钟毫秒差。
var probeFixtureNow = time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)

// withScheme 让 seed 建一条指定 scheme 的条目（默认 SOCKS5）。
func withScheme(s domain.ProxyScheme) func(*ProxyInput) {
	return func(in *ProxyInput) { in.Scheme = s }
}

// withCreds 让 seed 带上一对凭据（脱敏靶子要用它把密码落到条目上）。
func withCreds(user, pass string) func(*ProxyInput) {
	return func(in *ProxyInput) { in.Username, in.Password = &user, &pass }
}

// newProbeFixture 探测用例的固定装配：fake store + 注入时钟。临时端口/httptest 服务器
// 各自挂 t.Cleanup，夹具本身不额外持有资源。
//
// seed(name, host, port, opts...) 建一条代理并返回 id（默认 SOCKS5 + 无凭据）。ctx 是
// Background：fakeStore 不读 ctx 的取消，真实 PG 在请求已断时写不进去是既有行为——
// 「父 ctx 已取消」只能靠桩观察 ctx 来验。
func newProbeFixture(t *testing.T) (*Service, context.Context, func(name, host string, port int, opts ...func(*ProxyInput)) int64) {
	t.Helper()
	svc, _, _ := newProxyTestService()
	svc.statsNow = func() time.Time { return probeFixtureNow }
	ctx := context.Background()
	seed := func(name, host string, port int, opts ...func(*ProxyInput)) int64 {
		t.Helper()
		in := ProxyInput{Name: name, Scheme: domain.ProxySchemeSOCKS5, Host: host, Port: port}
		for _, opt := range opts {
			opt(&in)
		}
		id, err := svc.CreateProxy(ctx, in)
		require.NoError(t, err)
		return id
	}
	return svc, ctx, seed
}

// probeInvalidations 取夹具装进 Service 的失效记录器（「探测不触发失效」要用它）：
// 夹具签名只给 svc/ctx/seed，记录器仍是 newProxyTestService 装进去的同一个 *invRecorder。
func probeInvalidations(t *testing.T, svc *Service) *invRecorder {
	t.Helper()
	rec, ok := svc.inv.(*invRecorder)
	require.True(t, ok, "newProbeFixture 必须装 *invRecorder")
	return rec
}

// TestProbeProxyUnreachable 第一段失败（矩阵第一行）：LastProbeOK=false + 脱敏错误
// 文本 + **IP 空**，且第二段零调用（不发起任何出网请求）；enabled 不变、探测时刻来自
// 注入时钟（不赌墙钟毫秒差）、不触发失效。
func TestProbeProxyUnreachable(t *testing.T) {
	svc, ctx, seed := newProbeFixture(t)
	egress := &probeEgressStub{ip: "203.0.113.7"}
	svc.probeEgress = egress.fn()

	username, password := "probe-user", "probe-secret"
	id := seed("unreachable", "127.0.0.1", closedTCPPort(t), withCreds(username, password))
	rec := probeInvalidations(t, svc)
	before := rec.total()

	got, err := svc.ProbeProxy(ctx, id)
	require.NoError(t, err, "拨不通是探测结论，不是 API 失败")
	require.False(t, got.LastProbeOK)
	require.NotEmpty(t, got.LastProbeError, "失败原因落库（运维唯一现场证据）")
	require.NotContains(t, got.LastProbeError, password, "错误文本不得含密码（§5.2）")
	require.NotContains(t, got.LastProbeError, username, "错误文本不得含用户名")
	require.Empty(t, got.LastProbeIP, "TCP 不通 ⇒ 不取 IP（矩阵第一行）")
	require.Equal(t, 0, egress.calls, "第一段失败 ⇒ 第二段**零调用**（§3.3 / M7）")
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(probeFixtureNow), "探测时刻来自注入时钟（want %v, got %v）", probeFixtureNow, got.LastProbeAt)
	require.True(t, got.Enabled, "探测结果不影响 enabled（§6.3：enabled 是唯一开关）")
	require.Equal(t, before, rec.total(), "探测不改路由 ⇒ 不触发失效")
}

// TestProbeProxyReachable 探测成功的正例：本机临时监听端口（t.Cleanup 关闭）当真
// 代理地址，第二段注入桩返回固定 IP —— 四列落成 ok=true / 空错误文本 / ip。
//
// **必须注入桩**（spec §3.5 末条）：裸 net.Listen 骗不过第二段，它会真的去连
// api.ip.sb——要么等满 10s，要么让 LastProbeError 断言莫名其妙地红。
func TestProbeProxyReachable(t *testing.T) {
	svc, ctx, seed := newProbeFixture(t)
	const wantIP = "198.51.100.4"
	egress := &probeEgressStub{ip: wantIP}
	svc.probeEgress = egress.fn()

	host, port := listeningTCPAddr(t)
	id := seed("reachable", host, port)

	got, err := svc.ProbeProxy(ctx, id)
	require.NoError(t, err)
	require.True(t, got.LastProbeOK, "监听中的端口可拨通")
	require.Empty(t, got.LastProbeError)
	require.Equal(t, wantIP, got.LastProbeIP, "第二段取到的出口 IP 落库")
	require.Equal(t, 1, egress.calls, "第一段通 ⇒ 第二段恰好跑一次")
	require.NotNil(t, got.LastProbeAt)
	require.True(t, got.LastProbeAt.Equal(probeFixtureNow))

	// 探测不含认证校验（§6.3）：这条条目无凭据也能拨通。
	require.Empty(t, got.Username)
}

// TestProbeProxyTwoStage §3.3 三态矩阵的五条子例（spec §3.5 的 service 侧判据）：
// 两段全过 / TCP 失败且第二段零调用 / 第二段失败仍 ok=true / 第二段失败清掉旧 IP /
// 父 ctx 已取消不算第二段失败。
//
// 断言按**列**而不是按"整体结果"写：`ok` 只表示 TCP，出口 IP 是另一列——这两个结论
// 互不覆盖正是 owner 的裁决（§3.3），测试必须钉住它，别在下一次改动里被"统一"掉。
func TestProbeProxyTwoStage(t *testing.T) {
	const wantIP = "203.0.113.7"
	const (
		probeCredUser = "egress-user"
		probeCredPass = "egress-secret"
	)
	reachableAddr := func(t *testing.T) (string, int) { return listeningTCPAddr(t) }
	closedAddr := func(t *testing.T) (string, int) { return "127.0.0.1", closedTCPPort(t) }

	cases := []struct {
		name string
		stub *probeEgressStub
		// addr 第一段的落点。
		addr func(t *testing.T) (host string, port int)
		// creds 把 probeCredUser/probeCredPass 落到条目上（第二段的错误文本故意带同一对
		// 字面量：去掉脱敏，NotContains 就红）。
		creds bool
		// preProbe 在注入 stub 之前跑：造「上一次探测留下旧 IP」这类前态。
		preProbe func(t *testing.T, svc *Service, ctx context.Context, id int64)
		// parentCanceled 为 true 时用「已取消的父 ctx」调 ProbeProxy。
		parentCanceled bool

		wantOK           bool
		wantOKMsg        string
		wantIP           string
		wantIPMsg        string
		wantEmptyErr     bool
		wantErrMsg       string
		wantErrContains  string
		wantErrRedacted  bool
		wantOKWithErr    bool
		wantStubCalls    int
		wantStubCallsMsg string
		wantNoErrMsg     string
		wantAt           time.Time
	}{
		{
			name:          "both_stages_pass",
			stub:          &probeEgressStub{ip: wantIP},
			addr:          reachableAddr,
			wantOK:        true,
			wantIP:        wantIP,
			wantEmptyErr:  true,
			wantErrMsg:    "两段全过 ⇒ 无错误文本",
			wantStubCalls: 1,
			wantAt:        probeFixtureNow,
		},
		{
			name:             "tcp_failure_skips_egress",
			stub:             &probeEgressStub{ip: wantIP},
			addr:             closedAddr,
			wantStubCalls:    0,
			wantStubCallsMsg: "第一段失败不得发起第二段（M7 的变异靶子）",
		},
		{
			name: "egress_failure_keeps_ok",
			// 桩错误里**故意带凭据**：这段文本会落库并回显，服务层必须过 proxyRedactedError
			// （M6 的变异靶子——去掉脱敏这句断言就红）。
			stub: &probeEgressStub{err: fmt.Errorf(
				"egress ip probe: dial socks5://%s:%s@127.0.0.1:1080: proxy refused",
				probeCredUser, probeCredPass)},
			addr:            reachableAddr,
			creds:           true,
			wantOK:          true,
			wantOKMsg:       "TCP 通 ⇒ ok 仍是 true（§3.3 中间列的语义不变）",
			wantErrMsg:      "第二段失败原因必须落库（否则用户看不出 IP 为什么是空的）",
			wantErrContains: "***",
			wantErrRedacted: true,
			wantOKWithErr:   true,
			wantStubCalls:   1,
		},
		{
			name: "egress_failure_clears_previous_ip",
			stub: &probeEgressStub{err: errors.New("egress ip probe: api.ip.sb: connection timed out")},
			addr: reachableAddr,
			preProbe: func(t *testing.T, svc *Service, ctx context.Context, id int64) {
				// 第一次探测成功：旧 IP 落库。
				svc.probeEgress = (&probeEgressStub{ip: wantIP}).fn()
				first, err := svc.ProbeProxy(ctx, id)
				require.NoError(t, err)
				require.Equal(t, wantIP, first.LastProbeIP)
			},
			wantOK:        true,
			wantIPMsg:     "第二段失败必须清空上一次的 IP，不能留旧值冒充本次结果（§3.3 末条）",
			wantStubCalls: 1,
		},
		{
			name: "canceled_parent_ctx_does_not_fail_egress",
			// 桩**观察 ctx**（真实实现同款：HTTP 请求带 ctx，一取消就失败）——否则「第二段
			// 继承了请求 ctx」这个缺陷在桩上根本显不出来。
			stub: &probeEgressStub{ip: wantIP, observe: func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("egress ip probe: %w", err)
				}
				return nil
			}},
			addr:           reachableAddr,
			parentCanceled: true,
			wantOK:         true,
			wantIP:         wantIP,
			wantIPMsg:      "父 ctx 的取消不得让探测结论变成「IP 空 + 错误文本」",
			wantEmptyErr:   true,
			wantErrMsg:     "客户端走了不代表这条代理取不到 IP（与第一段 dialProxyAddr 同一条理由）",
			wantStubCalls:  1,
			wantNoErrMsg:   "fakeStore 不读 ctx（真实 PG 在请求已断时写不进去，那是既有行为）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, seed := newProbeFixture(t)
			host, port := tc.addr(t)
			var opts []func(*ProxyInput)
			if tc.creds {
				opts = append(opts, withCreds(probeCredUser, probeCredPass))
			}
			id := seed(tc.name, host, port, opts...)
			if tc.preProbe != nil {
				tc.preProbe(t, svc, ctx, id)
			}
			svc.probeEgress = tc.stub.fn()

			probeCtx := ctx
			if tc.parentCanceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel() // 客户端已断开（这里更狠：发请求之前就断了）
				probeCtx = canceled
			}

			got, err := svc.ProbeProxy(probeCtx, id)
			require.NoError(t, err, tc.wantNoErrMsg)
			require.Equal(t, tc.wantStubCalls, tc.stub.calls, tc.wantStubCallsMsg)
			require.Equal(t, tc.wantOK, got.LastProbeOK, tc.wantOKMsg)
			if tc.wantEmptyErr {
				require.Empty(t, got.LastProbeError, tc.wantErrMsg)
			} else {
				require.NotEmpty(t, got.LastProbeError, tc.wantErrMsg)
			}
			if tc.wantErrRedacted {
				require.NotContains(t, got.LastProbeError, probeCredPass, "第二段错误文本不得含密码（§5.2 / §5.3-3）")
				require.NotContains(t, got.LastProbeError, probeCredUser, "第二段错误文本不得含用户名")
			}
			if tc.wantErrContains != "" {
				require.Contains(t, got.LastProbeError, tc.wantErrContains, "被替换而非丢弃上下文")
			}
			if tc.wantIP != "" {
				require.Equal(t, tc.wantIP, got.LastProbeIP, tc.wantIPMsg)
			} else {
				require.Empty(t, got.LastProbeIP, tc.wantIPMsg)
			}
			if tc.wantOKWithErr {
				// 不变量（§3.3）：ok=true 且 error 非空 ⇔ 第一段通、第二段失败。
				require.True(t, got.LastProbeOK && got.LastProbeError != "")
			}
			if !tc.wantAt.IsZero() {
				require.True(t, got.LastProbeAt.Equal(tc.wantAt))
			}
		})
	}
}

// TestProbeEgressIPRealPath 第二段**真实实现**（不经桩）：把 proxyProbeEgressURL
// 指到一个不可解析的假域，于是请求只能经注入的假 HTTP 代理出去——这正是该常量必须是
// var 的理由（spec §3.2/§3.5：单测不许真连 api.ip.sb）。
//
// 假代理用普通 httptest 服务器即可：向 **http** 目标发请求时 Go 的 Transport 以
// 绝对 URI 形态直接把请求发给代理（不需要 CONNECT 隧道）；https 目标才要 CONNECT，
// 所以这里把 URL 也设成 http。
func TestProbeEgressIPRealPath(t *testing.T) {
	origURL := proxyProbeEgressURL
	t.Cleanup(func() { proxyProbeEgressURL = origURL })
	proxyProbeEgressURL = "http://egress.invalid/ip"

	// newFakeProxy 启一个假出口代理，返回它作为 domain.Proxy 的规格。gotTargetHost
	// 非 nil 时记录代理侧看到的目标 host（证明请求确实走了代理而非直连）。
	newFakeProxy := func(t *testing.T, status int, body string, gotTargetHost *string) domain.Proxy {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if gotTargetHost != nil {
				*gotTargetHost = r.Host
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		host, portStr, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		port, err := strconv.Atoi(portStr)
		require.NoError(t, err)
		return domain.Proxy{Scheme: domain.ProxySchemeHTTP, Host: host, Port: port}
	}

	t.Run("through_proxy_returns_ip", func(t *testing.T) {
		svc, ctx, seed := newProbeFixture(t)

		var targetHost string
		spec := newFakeProxy(t, http.StatusOK, "203.0.113.7\n", &targetHost)
		id := seed("real-egress", spec.Host, spec.Port, withScheme(spec.Scheme))
		// **不注入桩**：probeEgress 为 nil ⇒ 走真实实现（子进程里的 New 也是这条）。
		require.Nil(t, svc.probeEgress)

		got, err := svc.ProbeProxy(ctx, id)
		require.NoError(t, err)
		require.True(t, got.LastProbeOK)
		require.Empty(t, got.LastProbeError)
		require.Equal(t, "203.0.113.7", got.LastProbeIP, "真实第二段：经代理 GET 纯文本端点 → ParseIP 规范化后落库")
		require.Equal(t, "egress.invalid", targetHost, "请求确实经代理发出（绝对 URI 形态）")
	})

	t.Run("non_ip_body_is_second_stage_failure", func(t *testing.T) {
		svc, ctx, seed := newProbeFixture(t)

		spec := newFakeProxy(t, http.StatusOK, "<html>captive portal</html>", nil)
		id := seed("real-egress-bad-body", spec.Host, spec.Port, withScheme(spec.Scheme))

		got, err := svc.ProbeProxy(ctx, id)
		require.NoError(t, err)
		require.True(t, got.LastProbeOK, "TCP 通 ⇒ ok 仍是 true，脏响应只让 IP 段失败")
		require.Contains(t, got.LastProbeError, "not an IP address")
		require.Empty(t, got.LastProbeIP)
	})

	t.Run("non_200_status_fails", func(t *testing.T) {
		spec := newFakeProxy(t, http.StatusBadGateway, "203.0.113.7", nil)
		// 真实实现的预算由调用方给（ProbeProxy 挂 proxyProbeEgressTimeout），这里
		// 直接调就得自己带上限，避免假代理不回包把用例挂死。
		ectx, cancel := context.WithTimeout(context.Background(), proxyProbeEgressTimeout)
		defer cancel()
		ip, err := probeProxyEgressIP(ectx, spec)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unexpected status 502")
		require.Empty(t, ip)
	})
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

// supplierScopeCtx 注入供应商面行层作用域（镜像 handler.SupplierScopeInject）：
// spec §3.6 的两条判据都以它区分「供应商」与「管理员」。
func supplierScopeCtx(uid int64) context.Context {
	return domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(uid))
}

// TestCreateProxySupplierPrivateHostRejected 供应商自建出口的私网拒绝（spec §3.6）：
// 供应商面命中私网/回环/链路本地/保留/元数据地址 ⇒ ErrInvalidInput（400）且不落库；
// 管理员面同地址放行（内网出口是合法需求）；域名两方都放行（创建期不解析 DNS）。
func TestCreateProxySupplierPrivateHostRejected(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		forbidden bool
	}{
		{"ipv4 10/8", "10.1.2.3", true},
		{"ipv4 172.16/12", "172.16.5.4", true},
		{"ipv4 172.31 edge", "172.31.255.254", true},
		{"ipv4 192.168/16", "192.168.1.1", true},
		{"ipv4 loopback", "127.0.0.1", true},
		{"metadata 169.254/16", "169.254.169.254", true},
		{"cgnat 100.64/10", "100.64.7.7", true},
		{"ipv4 0/8", "0.1.2.3", true},
		{"ipv6 loopback", "::1", true},
		{"ipv6 ula fc00::/7", "fc00::1", true},
		{"ipv6 link-local fe80::/10", "fe80::1", true},
		{"ipv4-mapped ipv6", "::ffff:10.0.0.1", true},
		{"localhost literal", "localhost", true},
		{"localhost uppercase", "LocalHost", true},
		{"dot local suffix", "printer.local", true},
		// 放行：公网 IP 与域名（域名创建期不解析——DNS 查询本身是阻塞/外泄面）。
		{"public ip", "203.0.113.9", false},
		{"public ip 172.32", "172.32.0.1", false},
		{"domain", "proxy.example.com", false},
		{"domain-looking-localhost", "notlocalhost.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 供应商面。
			svc, _, rec := newProxyTestService()
			in := validProxyInput()
			in.Name = "sup-" + tc.name
			in.Host = tc.host
			before := rec.total()
			_, err := svc.CreateProxy(supplierScopeCtx(41), in)
			if tc.forbidden {
				require.ErrorIs(t, err, ErrInvalidInput, "供应商建私网出口 ⇒ 400")
				require.Equal(t, before, rec.total(), "拒绝路径不触发失效")
				list, lErr := svc.ListProxies(context.Background())
				require.NoError(t, lErr)
				require.Empty(t, list, "拒绝路径不落库")
			} else {
				require.NoError(t, err, "域名/公网 IP 在供应商面放行")
			}

			// 管理员面：同一地址一律通过（含私网——内网拨测/专线是合法需求）。
			adminSvc, _, _ := newProxyTestService()
			adminIn := validProxyInput()
			adminIn.Name = "admin-" + tc.name
			adminIn.Host = tc.host
			_, err = adminSvc.CreateProxy(context.Background(), adminIn)
			require.NoError(t, err, "管理员建内网出口不得被拦")
		})
	}
}

// TestUpdateProxySupplierPrivateHostRejected 改地址同样过私网判据（§3.6）：否则
// 「先建成公网域名、再改成 127.0.0.1」就是一条绕过创建面拦截的通路。
func TestUpdateProxySupplierPrivateHostRejected(t *testing.T) {
	svc, _, _ := newProxyTestService()
	supplierCtx := supplierScopeCtx(41)

	in := validProxyInput()
	in.Name = "update-private"
	in.Host = "proxy.example.com"
	id, err := svc.CreateProxy(supplierCtx, in)
	require.NoError(t, err, "公网域名先建成")

	in.Host = "127.0.0.1"
	require.ErrorIs(t, svc.UpdateProxy(supplierCtx, id, in), ErrInvalidInput,
		"供应商把地址改成回环 ⇒ 400")
	kept, err := svc.GetProxy(supplierCtx, id)
	require.NoError(t, err)
	require.Equal(t, "proxy.example.com", kept.Host, "拒绝路径不落值")

	// 管理员改同样的地址：通过（同一判据只在供应商分支生效）。
	require.NoError(t, svc.UpdateProxy(context.Background(), id, in), "管理员不受此限")
}
