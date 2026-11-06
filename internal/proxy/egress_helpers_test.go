// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/egress"
)

// testEgressPool 用给定 transport 装配一个出口池（M3：SDK 客户端的 transport 不再
// 是 Codex 的构造参数，而是"某个出口 client"的属性——测试用直连出口承载原来的
// 改写 transport）。nil transport ⇒ 裸底座（`WithTransport(nil)` 等价 SDK 默认）。
func testEgressPool(rt http.RoundTripper) *egress.Pool {
	if rt == nil {
		return egress.NewPool(nil)
	}
	return egress.NewPool(&http.Client{Transport: rt})
}

// egressMarkerHeader 出口可辨识标记头（spec §9.3）：`httptest` 下上游看到的
// `RemoteAddr` 对直连与经代理**恒是 127.0.0.1**，所以"是否经代理"只能靠替身注入
// 的这个头判定，绝不能靠来源地址。
const egressMarkerHeader = "X-Egress-Proxy"

// specOfProxyURL 把替身的监听地址转成出口规格（测试只走小写 scheme 这一条写面
// 归一后的形态）。用户名/密码留空 ⇒ URL 里没有 userinfo。
func specOfProxyURL(t *testing.T, scheme, rawURL, user, pass string) egress.Spec {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return egress.New(scheme, u.Hostname(), port, user, pass)
}

// basicAuthValue Proxy-Authorization 的 Basic 形态（与 net/http 自己写出的同形）。
func basicAuthValue(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// egressForwardProxy 最小 HTTP 正向代理（plain http 目标不发 CONNECT——Go 的
// `Transport` 直接把绝对 URI 发给代理）：
//   - 记录每个出站请求的绝对 URI（断言"请求确实经过代理"）
//   - 可要求 Basic `Proxy-Authorization`（认证失败路径）
//   - 转发前注入 egressMarkerHeader（唯一的出口可辨识判据）
type egressForwardProxy struct {
	*httptest.Server
	// requireAuth 非空 ⇒ 只接受该值的 Proxy-Authorization，否则回 407。
	requireAuth string

	rp *httputil.ReverseProxy

	mu   sync.Mutex
	seen []string
}

func newEgressForwardProxy(t *testing.T) *egressForwardProxy {
	t.Helper()
	p := &egressForwardProxy{}
	// 单跳反向代理承载转发：绝对 URI 已由 Transport 写在 r.URL 上，Director 只做
	// "注入标记头"这一件事。**用 ReverseProxy 而不是手工 RoundTrip**：它在 101
	// 响应上会自己 hijack 并双向拷贝（Go ≥1.12），resp-ws 面的 WS 升级才能穿过
	// 这个替身、并且同样带上看标记头。
	p.rp = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Header.Set(egressMarkerHeader, "1")
			req.Header.Del("Proxy-Authorization")
			req.RequestURI = ""
		},
		Transport: http.DefaultTransport,
	}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Server.Close)
	return p
}

// requested 返回收到的绝对 URI 快照（`METHOD absolute-URI`）。
func (p *egressForwardProxy) requested() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.seen))
	copy(out, p.seen)
	return out
}

func (p *egressForwardProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if p.requireAuth != "" && r.Header.Get("Proxy-Authorization") != p.requireAuth {
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-test"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.URL.String())
	p.mu.Unlock()
	p.rp.ServeHTTP(w, r)
}

// tunnel 转发型代理的 CONNECT 分支（https 目标经此）；本包的逐面用例都打 plain
// http 上游，这条分支只保证替身完整（CONNECT 细节的断言在 egress_http_test.go）。
func (p *egressForwardProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	up, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		up.Close()
		clientConn.Close()
		return
	}
	tunnelCopy(up, clientConn)
}

// egressConnectProxy 最小 CONNECT 代理（spec §9.3 的 HTTP 代理门禁）：
// 记录 `CONNECT host:port` 与 `Proxy-Authorization`，覆盖 200 隧道 / 407 认证 /
// CONNECT 失败三条路径。
type egressConnectProxy struct {
	*httptest.Server
	// requireAuth 非空 ⇒ 只接受该值；否则 407。
	requireAuth string
	// status 非 0 ⇒ 直接回该状态码（407 / 502 造错用）。
	status int

	mu      sync.Mutex
	targets []string
	auths   []string
}

func newEgressConnectProxy(t *testing.T) *egressConnectProxy {
	t.Helper()
	p := &egressConnectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Server.Close)
	return p
}

func (p *egressConnectProxy) recorded() (targets, auths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	targets = append(targets, p.targets...)
	auths = append(auths, p.auths...)
	return targets, auths
}

func (p *egressConnectProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, r.Host)
	p.auths = append(p.auths, r.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if p.requireAuth != "" && r.Header.Get("Proxy-Authorization") != p.requireAuth {
		w.Header().Set("Proxy-Authenticate", `Basic realm="egress-test"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		return
	}
	up, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		up.Close()
		clientConn.Close()
		return
	}
	if err := bufrw.Flush(); err != nil {
		up.Close()
		clientConn.Close()
		return
	}
	tunnelCopy(up, clientConn)
}

// tunnelCopy 双向拷贝并在任一侧结束后关闭两端（CONNECT 隧道与 WS 转发共用）。
func tunnelCopy(up net.Conn, client net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
	_ = up.Close()
	_ = client.Close()
	<-done
}

// egressSocksServer 最小 SOCKS5 服务端（RFC1928 + RFC1929 用户名/口令，零新依赖）：
//   - 记录客户端提供了哪些认证方法（断言"有凭据时确实提供 0x02"）
//   - 校验用户名/口令（认证失败路径）
//   - 记录**目标地址的形态**：域名（ATYP=0x03）原样记下——Go 对 socks 代理恒
//     远程解析，域名必须原封不动到达代理（spec §9.3 的"以域名到达"）
//   - 连上真实上游替身后做纯字节转发（替身忽略请求里的地址与端口）
type egressSocksServer struct {
	ln       net.Listener
	target   string // 真实上游 host:port（替身的目标）
	wantUser string
	wantPass string

	mu       sync.Mutex
	domains  [][2]string // ATYP=3 目标 {host, port}
	methods  [][]byte
	auths    [][2]string
	requests int
}

func newEgressSOCKS5Server(t *testing.T, target, wantUser, wantPass string) *egressSocksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &egressSocksServer{ln: ln, target: target, wantUser: wantUser, wantPass: wantPass}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *egressSocksServer) hostPort() (string, int) {
	addr := s.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func (s *egressSocksServer) snapshot() (domains [][2]string, methods [][]byte, auths [][2]string, requests int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(domains, s.domains...), append(methods, s.methods...), append(auths, s.auths...), s.requests
}

func (s *egressSocksServer) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *egressSocksServer) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)

	// 握手：VER NMETHODS METHODS...
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || hdr[0] != socks5Version {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	s.mu.Lock()
	s.methods = append(s.methods, methods)
	s.mu.Unlock()

	method := byte(socks5MethodNoAuth)
	if s.wantUser != "" {
		method = socks5MethodUserPass
	}
	if _, err := c.Write([]byte{socks5Version, method}); err != nil {
		return
	}
	if method == socks5MethodUserPass {
		if !s.authenticate(c, br) {
			return
		}
	}

	// 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[0] != socks5Version || req[1] != socks5CmdConnect {
		return
	}
	host, ok := readSocksAddr(br, req[3], func(domain string) {
		s.mu.Lock()
		s.domains = append(s.domains, [2]string{domain, ""})
		s.mu.Unlock()
	})
	if !ok {
		return
	}
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return
	}
	s.mu.Lock()
	s.requests++
	if n := len(s.domains); n > 0 && s.domains[n-1][1] == "" {
		s.domains[n-1][1] = strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))
	}
	s.mu.Unlock()
	_ = host // 目标地址按 spec 只做"到达形态"断言；实际连替身

	// 成功应答（BND.ADDR/BND.PORT 用 0.0.0.0:0）
	if _, err := c.Write([]byte{socks5Version, socks5RepSuccess, 0x00, socks5AtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	up, err := net.DialTimeout("tcp", s.target, 5*time.Second)
	if err != nil {
		return
	}
	// br 可能已缓冲客户端先发来的字节（HTTP 请求），必须从 br 继续读。
	go func() { _, _ = io.Copy(up, br); _ = up.Close() }()
	_, _ = io.Copy(c, up)
	_ = up.Close()
}

// authenticate RFC1929 用户名/口令子协商。
func (s *egressSocksServer) authenticate(c net.Conn, br *bufio.Reader) bool {
	var ver [1]byte
	if _, err := io.ReadFull(br, ver[:]); err != nil || ver[0] != socks5AuthVersion {
		return false
	}
	var ulen [1]byte
	if _, err := io.ReadFull(br, ulen[:]); err != nil {
		return false
	}
	user := make([]byte, int(ulen[0]))
	if _, err := io.ReadFull(br, user); err != nil {
		return false
	}
	var plen [1]byte
	if _, err := io.ReadFull(br, plen[:]); err != nil {
		return false
	}
	pass := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(br, pass); err != nil {
		return false
	}
	s.mu.Lock()
	s.auths = append(s.auths, [2]string{string(user), string(pass)})
	s.mu.Unlock()
	status := byte(0x00)
	if string(user) != s.wantUser || string(pass) != s.wantPass {
		status = 0x01
	}
	if _, err := c.Write([]byte{socks5AuthVersion, status}); err != nil {
		return false
	}
	return status == 0x00
}

// readSocksAddr 按 ATYP 读目标地址；域名形态回调 onDomain（断言"域名到达"）。
func readSocksAddr(br *bufio.Reader, atyp byte, onDomain func(string)) (string, bool) {
	switch atyp {
	case socks5AtypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	case socks5AtypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	case socks5AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return "", false
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		d := string(b)
		onDomain(d)
		return d, true
	}
	return "", false
}

// SOCKS5 协议常量（RFC1928 / RFC1929；只取替身要用的那些）。
const (
	socks5Version        = 0x05
	socks5AuthVersion    = 0x01
	socks5MethodNoAuth   = 0x00
	socks5MethodUserPass = 0x02
	socks5CmdConnect     = 0x01
	socks5RepSuccess     = 0x00
	socks5AtypIPv4       = 0x01
	socks5AtypDomain     = 0x03
	socks5AtypIPv6       = 0x04
)

// egressFailureMessage 造一个"代理拒绝连接"的错误替身（用于不需要真起代理的
// 分类断言）。
func egressFailureMessage(proxyAddr string) string {
	return fmt.Sprintf("proxyconnect tcp: dial tcp %s: connect: connection refused", proxyAddr)
}
