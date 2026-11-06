// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// proxyProbeTimeout 探测的硬上限（spec §6.3）：只拨 TCP，不验证认证、不验证出口
// IP；结果不影响条目可用性（enabled 是唯一开关）。
const proxyProbeTimeout = 5 * time.Second

// proxyNameMaxRunes name 长度上限（spec §3.5 第 1 条）。
const proxyNameMaxRunes = 64

// proxyHostMaxLen host 长度上限（DNS 线格式上限，spec §3.5 第 3 条；按字节计）。
const proxyHostMaxLen = 253

// ProxyInput 代理条目的写面入参（service 层）。
//
// **不复用 `domain.Proxy` 当补丁**：domain 类型带 ID 与探测三列，那三列由
// WriteProxyProbe 独占写（spec §6.3 列的写者归属）；把它当入参只会让调用方以为
// 「顺手能改探测结论」。这里只声明管理面真正能写的字段，三态用指针表达：
//
//	Username/Password/Remark：nil = 不变；&"" = 清空；&v = 落值
//	Enabled：nil = 不变（Create 缺省 true）
type ProxyInput struct {
	Name     string
	Scheme   domain.ProxyScheme
	Host     string
	Port     int
	Username *string
	Password *string
	Enabled  *bool
	Remark   *string
}

// ListProxies 代理条目列表（仓库侧软删过滤 + id 稳定序）。无业务逻辑：直接转发。
func (s *Service) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	return s.store.ListProxies(ctx)
}

// GetProxy 单个条目：不存在/已软删 → ErrNotFound（404）。
func (s *Service) GetProxy(ctx context.Context, id int64) (domain.Proxy, error) {
	p, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return domain.Proxy{}, mapRepoErr(err)
	}
	return p, nil
}

// CreateProxy 校验 → 落库 → 失效通知，返回新 id。
//
// name 唯一性不在 service 侧预先查一遍：唯一约束由 DB 索引裁决、由仓库映射成
// repository.ErrConflict（→ 409，含 name 详情），与 template/group 同一条既有路径
// （先查后写只多一个 TOCTOU 窗口，不多一个保证）。
func (s *Service) CreateProxy(ctx context.Context, in ProxyInput) (int64, error) {
	p, err := normalizeProxyInput(in)
	if err != nil {
		return 0, err
	}
	// enabled：缺省 true（spec §3.1），显式 false 生效。
	p.Enabled = true
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.Remark != nil {
		p.Remark = strings.TrimSpace(*in.Remark)
	}
	id, err := s.store.CreateProxy(ctx, p)
	if err != nil {
		return 0, mapRepoErr(err)
	}
	s.invalidateProxyPool(ctx)
	if s.log != nil {
		s.log.Info("proxy created", logx.Int64("id", id), logx.String("name", p.Name))
	}
	return id, nil
}

// UpdateProxy 全量更新非密字段 + 凭据三态（PUT 语义）。
//
// 非密字段：补丁提供即落值、未提供（nil）即保持原值——故先读一次当前行，这次读
// 同时给出 404/软删判定。
//
// **凭据两列都不允许「读旧值再整体写回」**：那会把并发的另一次改凭据用读到的旧值
// 静默覆盖（read-modify-write 窗口）。三态因此交给仓库在同一条 UPDATE 里表达
// （store.UpdateProxy 的 credential 形参：nil = 不变 / &"" = 清空 / &v = 落值）。
func (s *Service) UpdateProxy(ctx context.Context, id int64, in ProxyInput) error {
	p, err := normalizeProxyInput(in)
	if err != nil {
		return err
	}
	cur, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return mapRepoErr(err) // 不存在/已软删 → 404
	}
	p.ID = id
	p.Enabled = cur.Enabled
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	p.Remark = cur.Remark
	if in.Remark != nil {
		p.Remark = strings.TrimSpace(*in.Remark)
	}
	// 凭据走三态直传（用归一后的值 + 调用方的"是否提供"）：nil 的两列不进 UPDATE，
	// 故不经过当前行，也就没有把别人刚改的凭据回滚的窗口。
	var credential repository.ProxyCredential
	if in.Username != nil {
		credential.Username = &p.Username
	}
	if in.Password != nil {
		credential.Password = &p.Password
	}
	// 改名撞已有 name → repository.ErrConflict → ErrConflict（409）。
	if err := mapRepoErr(s.store.UpdateProxy(ctx, p, credential)); err != nil {
		return err
	}
	s.invalidateProxyPool(ctx)
	return nil
}

// DeleteProxy 软删 + 同事务解绑（仓库已实现：删条目的同一事务把指向它的账号
// proxy_id 置 NULL，回落直连）→ 失效通知。
func (s *Service) DeleteProxy(ctx context.Context, id int64) error {
	if err := mapRepoErr(s.store.DeleteProxy(ctx, id)); err != nil {
		return err // 不存在/已软删 → 404
	}
	s.invalidateProxyPool(ctx)
	return nil
}

// ProbeProxy TCP 拨测（5s）→ 写回 last_probe_* 三元组 → 返回最新条目。
//
// 返回 **(条目, nil)** 而非错误：拨不通是探测结论（LastProbeOK=false + 已脱敏的
// 错误文本），不是 API 失败——探测的目的就是把「连不上」记下来。只有条目不存在
// 或写回失败才返回错误。
//
// 探测**不改变** enabled/Routable（spec §6.3），故不触发失效通知。
func (s *Service) ProbeProxy(ctx context.Context, id int64) (domain.Proxy, error) {
	cur, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return domain.Proxy{}, mapRepoErr(err)
	}
	probeErr := dialProxyAddr(ctx, cur.Host, cur.Port)
	// 探测时刻走**注入时钟**（Service.statsClock：statsNow 注入，nil = time.Now）——
	// 既有 now 注入模式，测试用固定时间，不赌墙钟毫秒差。
	at := s.statsClock()
	errText := ""
	if probeErr != nil {
		errText = proxyRedactedError(probeErr, cur)
	}
	if err := mapRepoErr(s.store.WriteProxyProbe(ctx, id, at, probeErr == nil, errText)); err != nil {
		return domain.Proxy{}, err
	}
	latest, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return domain.Proxy{}, mapRepoErr(err)
	}
	return latest, nil
}

// invalidateProxyPool 代理条目变更后的失效（spec §8 失效矩阵）：与 template 变更
// 走**同一条**既有 seam（KindTemplates → Sched.InvalidateAll），不另起一条 reload
// 通路——代理决定账号的出口，出口 client 在快照期解析，故它与模板同属「全量重装」
// 类变更；跨实例由既有 NOTIFY（d.Templates() = mark(KindTemplates|KindClients)）
// 收敛，M2 在该 reload 里跑 Pool.Prune。
func (s *Service) invalidateProxyPool(ctx context.Context) {
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
}

// normalizeProxyInput 按 spec §3.5 的**固定顺序**校验并归一（顺序即错误优先级）：
// name → scheme → host → port → username/password。任何一条不通过 → ErrInvalidInput。
//
// scheme 只做小写归一，**不把 socks5h 折成 socks5**：两者在本项目语义相同（Go 标准库
// 同样处理），但用户输入原样落库是契约（domain.ProxyScheme 注释）。
func normalizeProxyInput(in ProxyInput) (domain.Proxy, error) {
	// 1. name：trim 后 1..64（按 rune 计：它是人读标签）。
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > proxyNameMaxRunes {
		return domain.Proxy{}, ErrInvalidInput
	}
	// 2. scheme：小写后必须命中白名单。
	scheme := domain.ProxyScheme(strings.ToLower(strings.TrimSpace(string(in.Scheme))))
	if !scheme.Valid() {
		return domain.Proxy{}, ErrInvalidInput
	}
	// 3. host。
	host, err := normalizeProxyHost(in.Host)
	if err != nil {
		return domain.Proxy{}, err
	}
	// 4. port。
	if in.Port < 1 || in.Port > 65535 {
		return domain.Proxy{}, ErrInvalidInput
	}
	// 5. username/password：都空 = 无认证；username 非空时 password 可空——故这里
	// 只做控制字符拒绝，不强制成对。凭据**不做 trim/归一**：它是原样拿去认证的字节。
	username, err := normalizeProxyCredential(in.Username)
	if err != nil {
		return domain.Proxy{}, err
	}
	password, err := normalizeProxyCredential(in.Password)
	if err != nil {
		return domain.Proxy{}, err
	}
	return domain.Proxy{
		Name:     name,
		Scheme:   scheme,
		Host:     host,
		Port:     in.Port,
		Username: username,
		Password: password,
	}, nil
}

// normalizeProxyHost host 归一：trim → 去 IPv6 方括号 → 拒绝分隔符/空白/控制字符
// → 长度 ≤ 253。落库值不带方括号（domain.Proxy.Host 契约；出栈时由 egress 的
// Addr() 用 net.JoinHostPort 自补）。
func normalizeProxyHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", ErrInvalidInput
	}
	// 只剥**成对**的方括号：单个 '[' 是写错，不猜。
	if len(h) >= 2 && strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" || len(h) > proxyHostMaxLen {
		return "", ErrInvalidInput
	}
	for _, r := range h {
		// '/' 会被 URL 拼接读成路径，'@' 会被读成 userinfo 分隔符（凭据被劫持到
		// host 段）；空白与控制字符能穿透到 Host 头/拨测串。残留的 '[' ']' 是
		// 双重括号之类的手写错误（"[[::1]]"），同样拒绝。
		if r == '/' || r == '@' || r == '[' || r == ']' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrInvalidInput
		}
	}
	return h, nil
}

// normalizeProxyCredential 凭据归一：nil/空 = 无；控制字符 → 400。值本身原样保留
// （认证字节，不 trim）。
func normalizeProxyCredential(v *string) (string, error) {
	if v == nil || *v == "" {
		return "", nil
	}
	for _, r := range *v {
		if unicode.IsControl(r) {
			return "", ErrInvalidInput
		}
	}
	return *v, nil
}

// dialProxyAddr 拨一次 TCP（只到 host:port，不做握手后的认证校验）。取消信号被
// 剥掉（context.WithoutCancel）：客户端断开不等于代理不可达，取消不该被记成一次
// 失败探测；5s 上限由 Dialer.Timeout 兜住。
func dialProxyAddr(ctx context.Context, host string, port int) error {
	d := net.Dialer{Timeout: proxyProbeTimeout}
	conn, err := d.DialContext(context.WithoutCancel(ctx), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// proxyRedactedError 探测错误文本脱敏（spec §5.2）：这段文本会落进
// `proxy.last_probe_error`，而该列会经管理面回显——所以**不假设**拨测错误天然不含
// 凭据（认证型代理的握手错误可能带 userinfo），非空用户名/密码一律替成 ***。
// domain.Proxy.String() 同理不落凭据，日志侧用它的摘要而非这里。
func proxyRedactedError(err error, p domain.Proxy) string {
	text := err.Error()
	for _, secret := range []string{p.Password, p.Username} {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "***")
	}
	return text
}

// checkProxyExists 账号写面的绑定校验（spec §3.5 第 6 条）：
//
//	nil = 不变（不校验——`AccountPatch.ProxyID == nil` 已表达「不变」）
//	&0 = 解绑（回落直连，无需目标存在）
//	&id>0 = 绑定：目标必须**存在且未软删**（仓库 GetProxy 已把软删视同不存在），
//	        否则 ErrNotFound（404，带 id 详情——与模板/组缺 id 的既有映射同形）
//
// 落点：三个账号写面（CreateAccount / PatchAccount / UpdateAccountsBatch）里紧跟
// 在 checkGroupsExist 之后，与 supplier_user_id 的存在性/值域校验同一处、同一形态。
func (s *Service) checkProxyExists(ctx context.Context, id *int64) error {
	if id == nil || *id <= 0 {
		return nil
	}
	if _, err := s.store.GetProxy(ctx, *id); err != nil {
		return mapRepoErr(err)
	}
	return nil
}
