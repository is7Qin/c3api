// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
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

// ProxyInput 代理条目的写面入参（service 层，线格式的直接投影）。
//
// **不复用 `domain.Proxy` 当补丁**：domain 类型带 ID 与探测三列，那三列由
// WriteProxyProbe 独占写（spec §6.3 列的写者归属）；把它当入参只会让调用方以为
// 「顺手能改探测结论」。这里只声明管理面真正能写的字段，三态用指针表达：
//
//	Username/Password/Remark：nil = 不变；&"" = 清空；&v = 落值
//	Enabled：nil = 不变（Create 缺省 true）
//
// 归一之后落到 repository.ProxyCreate / ProxyUpdate（同样是"只有可写列"的类型），
// 故三态在 service 这一层就翻成了仓库认的形态，仓库不再需要"忽略某些字段"。
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
	id, name, err := s.createProxyOnce(ctx, in)
	if err != nil {
		return 0, err
	}
	s.invalidateProxyPool(ctx)
	if s.log != nil {
		s.log.Info("proxy created", logx.Int64("id", id), logx.String("name", name))
	}
	return id, nil
}

// createProxyOnce 归一 → 供应商私网判据 → 落库，**不触发失效**。返回（id, 归一后的
// name, err）；失败时 name 是能拿到的最接近输入的那个（name 本身非法 ⇒ 原始输入），
// 供批量导入把结果标回原文那一行。
//
// 抽出来是为了让「单条创建」与「批量导入」走**完全同一条**判据链：唯一区别只是失效
// 时机（单条立刻、批量收在循环之后）。没有任何一条有第二份规则。
func (s *Service) createProxyOnce(ctx context.Context, in ProxyInput) (int64, string, error) {
	c, err := normalizeProxyInput(in)
	if err != nil {
		return 0, strings.TrimSpace(in.Name), err
	}
	// 供应商自建出口的私网拒绝（spec §3.6）：判据只在「供应商创建」分支生效，
	// 管理员建内网出口是合理需求（内网拨测/专线）。
	if err := checkSupplierEgressHost(ctx, c.Host); err != nil {
		return 0, c.Name, err
	}
	// enabled：缺省 true（spec §3.1），显式 false 生效。
	c.Enabled = true
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if in.Remark != nil {
		c.Remark = strings.TrimSpace(*in.Remark)
	}
	id, err := s.store.CreateProxy(ctx, c)
	if err != nil {
		return 0, c.Name, mapRepoErr(err)
	}
	return id, c.Name, nil
}

// proxyImportMaxRows 单次批量导入的行数上限（spec-proxy-import §2）：与代理表量级一致
// （十到百条）。上限判在这里而不是 schema（`maxItems` 只是文档，生成物不校验）——
// 一个判据一个实现点。
const proxyImportMaxRows = 100

// ProxyImportOutcome 批量导入里**单行**的结果：Index 是入参下标，Err 为 nil = 成功。
//
// Name 自带在结果里（而不是让调用方拿 Index 回请求体里捞）：导入的界面就是一张逐行
// 结果表，"第几行、哪条、成没成"必须一次给全。
type ProxyImportOutcome struct {
	Index int
	Name  string
	ID    int64
	Err   error
}

// ImportProxies 批量创建（管理面与供应商面共用，spec-proxy-import）。
//
// **部分成功**：每行独立归一/判据/落库，失败的行不影响其余行。返回切片与入参等长、同序；
// 返回 error 只表示**请求本身**不合法（0 行、超过 proxyImportMaxRows）——逐行失败在
// 结果里，不在 error 里（请求合法性与结果好坏是两件事）。
//
// **整批一次失效**：createProxyOnce 不触发重载；任何一行成功才在循环后调一次
// invalidateProxyPool。这是这条端点存在的真正理由——N 个单条 POST 就是 N 次全量
// 调度重载，而语义上只需要一次（把"何时失效"交给客户端去合并 = 客户端维护一份服务端
// 知识，换个客户端就退化）。
//
// **逐行串行插入，不用 CreateBulk**：唯一约束冲突在批量插入里只报第一个、且无法归属到
// 具体行；"第几行重名"恰是导入功能的全部价值（表是小表，几十次往返不是瓶颈）。
func (s *Service) ImportProxies(ctx context.Context, rows []ProxyInput) ([]ProxyImportOutcome, error) {
	if len(rows) == 0 || len(rows) > proxyImportMaxRows {
		return nil, fmt.Errorf("%w: rows must be 1..%d", ErrInvalidInput, proxyImportMaxRows)
	}
	out := make([]ProxyImportOutcome, 0, len(rows))
	created := 0
	for i, in := range rows {
		id, name, err := s.createProxyOnce(ctx, in)
		out = append(out, ProxyImportOutcome{Index: i, Name: name, ID: id, Err: err})
		if err == nil {
			created++
		}
	}
	if created > 0 {
		s.invalidateProxyPool(ctx)
	}
	return out, nil
}

// UpdateProxy 全量更新可写列 + 凭据三态（PUT 语义）。
//
// 非密字段：补丁提供即落值、未提供（nil）即保持原值——故先读一次当前行，这次读
// 同时给出 404/软删判定。
//
// **凭据两列都不允许「读旧值再整体写回」**：那会把并发的另一次改凭据用读到的旧值
// 静默覆盖（read-modify-write 窗口）。三态因此交给仓库在同一条 UPDATE 里表达
// （ProxyUpdate.Credential：nil = 不变 / &"" = 清空 / &v = 落值）。
func (s *Service) UpdateProxy(ctx context.Context, id int64, in ProxyInput) error {
	c, err := normalizeProxyInput(in)
	if err != nil {
		return err
	}
	// 改地址同样过私网判据（§3.6）：否则「先建成公网域名再改成 127.0.0.1」就是
	// 一条绕过创建面拦截的通路。
	if err := checkSupplierEgressHost(ctx, c.Host); err != nil {
		return err
	}
	cur, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return mapRepoErr(err) // 不存在/已软删 → 404
	}
	c.Enabled = cur.Enabled
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	c.Remark = cur.Remark
	if in.Remark != nil {
		c.Remark = strings.TrimSpace(*in.Remark)
	}
	// 凭据走三态直传（用归一后的值 + 调用方的"是否提供"）：nil 的两列不进 UPDATE，
	// 故不经过当前行，也就没有把别人刚改的凭据回滚的窗口。
	var credential repository.ProxyCredential
	if in.Username != nil {
		credential.Username = &c.Username
	}
	if in.Password != nil {
		credential.Password = &c.Password
	}
	// 改名撞同归属已有 name → repository.ErrConflict → ErrConflict（409）。
	if err := mapRepoErr(s.store.UpdateProxy(ctx, repository.ProxyUpdate{
		ID:          id,
		ProxyFields: c.ProxyFields,
		Credential:  credential,
	})); err != nil {
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
// 返回值是仓库的**创建** DTO（可写列 + 凭据值）：创建与更新共用同一份归一结果，
// 更新面只是把它塞进 ProxyUpdate 并补上 id 与凭据三态——两条路因此校验同一份规则，
// 不会出现"创建校验过、更新绕过去"的形状。
//
// scheme 只做小写归一，**不把 socks5h 折成 socks5**：两者在本项目语义相同（Go 标准库
// 同样处理），但用户输入原样落库是契约（domain.ProxyScheme 注释）。
// 校验失败一律 `ErrInvalidInput`，但**带字段级说明**（spec-proxy-import §4）：单条创建时
// 调用方刚从表单出来、知道自己在改哪一格；批量导入时一行只回一句 "invalid input"
// 等于没说——几十行里没法定位。文案是给人读的，不是契约（测试只断言 ErrorIs）。
func normalizeProxyInput(in ProxyInput) (repository.ProxyCreate, error) {
	// 1. name：trim 后 1..64（按 rune 计：它是人读标签）。
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > proxyNameMaxRunes {
		return repository.ProxyCreate{}, fmt.Errorf("%w: name must be 1..%d characters after trim", ErrInvalidInput, proxyNameMaxRunes)
	}
	// 2. scheme：小写后必须命中白名单。
	scheme := domain.ProxyScheme(strings.ToLower(strings.TrimSpace(string(in.Scheme))))
	if !scheme.Valid() {
		return repository.ProxyCreate{}, fmt.Errorf("%w: scheme must be one of socks5, socks5h, http, https", ErrInvalidInput)
	}
	// 3. host。
	host, err := normalizeProxyHost(in.Host)
	if err != nil {
		return repository.ProxyCreate{}, err
	}
	// 4. port。
	if in.Port < 1 || in.Port > 65535 {
		return repository.ProxyCreate{}, fmt.Errorf("%w: port must be 1..65535", ErrInvalidInput)
	}
	// 5. username/password：都空 = 无认证；username 非空时 password 可空——故这里
	// 只做控制字符拒绝，不强制成对。凭据**不做 trim/归一**：它是原样拿去认证的字节。
	username, err := normalizeProxyCredential("username", in.Username)
	if err != nil {
		return repository.ProxyCreate{}, err
	}
	password, err := normalizeProxyCredential("password", in.Password)
	if err != nil {
		return repository.ProxyCreate{}, err
	}
	return repository.ProxyCreate{
		ProxyFields: repository.ProxyFields{
			Name:   name,
			Scheme: scheme,
			Host:   host,
			Port:   in.Port,
		},
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
		return "", fmt.Errorf("%w: host is required", ErrInvalidInput)
	}
	// 只剥**成对**的方括号：单个 '[' 是写错，不猜。
	if len(h) >= 2 && strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", fmt.Errorf("%w: host is required", ErrInvalidInput)
	}
	if len(h) > proxyHostMaxLen {
		return "", fmt.Errorf("%w: host must be at most %d bytes", ErrInvalidInput, proxyHostMaxLen)
	}
	for _, r := range h {
		// '/' 会被 URL 拼接读成路径，'@' 会被读成 userinfo 分隔符（凭据被劫持到
		// host 段）；空白与控制字符能穿透到 Host 头/拨测串。残留的 '[' ']' 是
		// 双重括号之类的手写错误（"[[::1]]"），同样拒绝。
		if r == '/' || r == '@' || r == '[' || r == ']' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%w: host must not contain '/', '@', brackets, spaces or control characters", ErrInvalidInput)
		}
	}
	return h, nil
}

// normalizeProxyCredential 凭据归一：nil/空 = 无；控制字符 → 400。值本身原样保留
// （认证字节，不 trim）。field 只用于错误文案（"username"/"password"）。
func normalizeProxyCredential(field string, v *string) (string, error) {
	if v == nil || *v == "" {
		return "", nil
	}
	for _, r := range *v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %s must not contain control characters", ErrInvalidInput, field)
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

// proxyOwnerMatchesAccount 代理归属与目标账号归属的兼容性（spec §3.6 的共享子规则）：
// 平台代理对任何账号成立；私池代理只对自己的账号成立。
// 调用方各自负责「调用者有没有资格做这件事」与错误码，本函数只管归属兼容。
func proxyOwnerMatchesAccount(owner *int64, targetSupplierUserID int64) bool {
	if owner == nil {
		return true // 平台代理：归属上不挑账号（"只有管理员能分配"是调用方的前置检查）
	}
	return targetSupplierUserID == *owner
}

// resolveBindableProxy 应用 spec §3.6 的**唯一绑定谓词**（一处实现，不散开）：
//
//	nil      = 不变（不校验——`AccountPatch.ProxyID == nil` 已表达「不变」）
//	&0       = 解绑（回落直连，无需目标存在）
//	&id>0    = 绑定：目标必须存活 ∧ 命中下面这张表，否则 ErrNotFound（404）
//
//	bindable(proxy, targetAccount, callerScope) =
//	    proxy 存在且未软删
//	  ∧ ( proxy.owner_uid == NULL ? callerScope 为空（管理员）      // 平台代理：仅管理员可分配
//	                             : targetAccount.supplier_user_id == *proxy.owner_uid )
//	                                                       // 私池代理：只能承载其主的流量
//
// 第二行（归属兼容）由 proxyOwnerMatchesAccount 承载，与转属守卫
// checkTransferKeepsBinding 共用同一份实现；第一行（「调用者有没有资格分配平台代理」）
// 是本入口自己的前置检查，错误码也只属于本入口（404）。
//
// 三条后果，都是有意的：
//   - 平台池**不是公共池**：供应商面（scope.Set）绑平台代理 ⇒ 404（看不见也绑不上）；
//   - 私池代理**管理员也不能挪给别人**（targetSupplier 必须等于 owner），否则供应商
//     控制的出口会承载别人的流量（信任倒挂）；
//   - 平台账号的 supplier_user_id 是 0 ⇒ 私池分支恒假 ⇒ 平台账号绑不上任何私池
//     代理（等价于「平台账号只能绑平台代理」）。
//
// 越界一律 **404 而不是 403**：不泄露「别人有这条代理」这一事实。归属走
// store.LookupProxyForBinding（跨 scope 读取——判的是「能不能绑」而非「能不能看」）。
//
// **谓词不看 enabled**（绑定校验因此不带它）：停用是运行期可用性，绑定关系是权限事实。
// 允许「先绑好、后启用」的运维顺序，代价由 Routable 承担——停用条目的绑定会让账号
// Routable=false 而不入选（§4.1），不会把流量送出去。
//
// 落点：三个账号写面（CreateAccount / PatchAccount / UpdateAccountsBatch）里紧跟
// checkGroupsExist 之后，与 supplier_user_id 的存在性/值域校验同一处、同一形态。
func (s *Service) resolveBindableProxy(ctx context.Context, proxyID *int64, targetSupplierUserID int64) error {
	if proxyID == nil || *proxyID <= 0 {
		return nil
	}
	info, err := s.store.LookupProxyForBinding(ctx, *proxyID)
	if err != nil {
		return mapRepoErr(err) // 不存在/已软删 → 404
	}
	// 前置检查：平台代理只有管理员（无 scope）能分配。这不是归属兼容问题——私池代理
	// 的分配资格由归属本身给出——所以留在本入口，且同样落 404（不泄露存在性）。
	if info.OwnerUID == nil && domain.AccountScopeFrom(ctx).Set {
		return fmt.Errorf("%w: proxy_id=%d is not bindable", ErrNotFound, *proxyID)
	}
	// 归属兼容（与转属守卫共用子规则）：私池代理只能承载其主的账号，管理员也遵守。
	if !proxyOwnerMatchesAccount(info.OwnerUID, targetSupplierUserID) {
		return fmt.Errorf("%w: proxy_id=%d is not bindable", ErrNotFound, *proxyID)
	}
	return nil
}

// checkTransferKeepsBinding 归属转移时的**既存绑定**守卫（spec §3.6 的第二个入口）。
//
// 谓词只在「显式给了 proxy_id」时被执行，于是有一个漏洞形状：补丁只把账号从供应商
// A 转给 B（不碰 proxy_id），而该账号当前绑的是 **A 的私池代理** —— 写完之后那一行
// 就落在谓词之外：B 的流量走 A 的出口。这不是理论问题，`ownership` 面本来就把
// "把账号分配给供应商"做成一个普通字段更新。
//
// 处置：**400，要求调用方显式决定**——同一补丁里解绑（`"proxy_id": null`）或改绑
// 新归属自己的代理。为什么不静默解绑：解绑会改掉出栈事实（从供应商出口切回直连），
// 而调用方只说了"转属"；让一个隐式副作用改掉流量路径，比让人补一个字段更糟。为什么不
// 静默放过：那条越权状态会一直存在，直到有人碰巧改绑定。
//
// 归属兼容与绑定入口共用 proxyOwnerMatchesAccount（同一条子规则、不同错误码：
// 这里是 400 ⇒ 让调用方补字段；绑定入口是 404 ⇒ 不泄露存在性）。
//
// 平台代理不受此限：它是管理员分配的，转属后仍由管理员维护，没有跨租户信任问题
// （子规则对 owner==nil 恒真，正是这一条）。
func (s *Service) checkTransferKeepsBinding(ctx context.Context, proxyID *int64, fromSupplier, toSupplier int64) error {
	if fromSupplier == toSupplier || proxyID == nil || *proxyID <= 0 {
		return nil
	}
	info, err := s.store.LookupProxyForBinding(ctx, *proxyID)
	if err != nil {
		return mapRepoErr(err)
	}
	if proxyOwnerMatchesAccount(info.OwnerUID, toSupplier) {
		return nil // 平台代理恒真；私池代理的归属正好等于新归属
	}
	return fmt.Errorf("%w: transferring the account would leave proxy_id=%d (owned by %d) bound to a %d account; clear proxy_id or bind a proxy owned by the new owner",
		ErrInvalidInput, *proxyID, *info.OwnerUID, toSupplier)
}

// checkSupplierEgressHost 供应商自建出口的私网拒绝（spec §3.6）：只在 scope.Set 时生效
// ——管理员建内网出口是合理需求（内网拨测/专线），供应商建就成了一个免费的**内网穿透
// 跳板**。命中 ⇒ ErrInvalidInput（400）。
//
// **只对 IP 字面量生效，域名一律放行**：创建时不发 DNS 查询（解析本身是阻塞面，也是
// 一条外泄面：把内网主机名丢给解析器就能探测域内命名），解析由代理侧在拨测/出栈时
// 自己做。故 `internal.corp` 这类名字绕不过判据的说法是不准确的——它压根不在判据范围
// 内；这条边界的代价（供应商可自建指向内网的域名出口）已记在 spec §3.6 的已知取舍里。
func checkSupplierEgressHost(ctx context.Context, host string) error {
	if !domain.AccountScopeFrom(ctx).Set {
		return nil
	}
	if supplierForbiddenEgressHost(host) {
		return ErrInvalidInput
	}
	return nil
}

// supplierForbiddenEgressHost 私网/回环/链路本地/保留地址判据（§3.6 列表）：`10/8`、
// `172.16/12`、`192.168/16`、`127/8`、`169.254/16`、`100.64/10`、`0/8`、`::1`、
// `fc00::/7`、`fe80::/10`、IPv4-mapped IPv6，以及字面量 `localhost` 与 `.local` 后缀。
//
// IPv4-mapped（`::ffff:10.0.0.1`）不需要单独分支：net.IP.To4() 会把映射地址还原成 4
// 字节，后面的判定因此对两种写法一致。
func supplierForbiddenEgressHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	// mDNS 后缀 `.local` 与字面量 localhost：两者在任何解析器下都指向本地/链路内。
	if h == "localhost" || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false // 域名：不解析 DNS，放行（见 checkSupplierEgressHost 注释）
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 0 {
			return true // 0/8（"本网络"，含 0.0.0.0 之外的写法）
		}
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true // 100.64/10 运营商级 NAT（云上常被当内网用）
		}
	}
	return false
}
