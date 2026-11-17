// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/is7qin/c3api/pkg/egress"
)

// ProxyScheme 出口代理的协议族（spec-proxy-pool §2 协议矩阵）。取值即落库字符串：
// **不做归一化改写**——socks5 与 socks5h 在本项目语义相同（Go 标准库
// http.Transport 明示 treated the same），但原样保存用户输入。
type ProxyScheme string

const (
	ProxySchemeSOCKS5  ProxyScheme = "socks5"
	ProxySchemeSOCKS5H ProxyScheme = "socks5h"
	ProxySchemeHTTP    ProxyScheme = "http"
	ProxySchemeHTTPS   ProxyScheme = "https"
)

// Valid 报告 scheme 是否在白名单内（供写面校验用；小写比较，调用方负责先归一
// 小写——落库值恒为小写，归一化不改变 socks5/socks5h 的区分）。
func (s ProxyScheme) Valid() bool {
	switch s {
	case ProxySchemeSOCKS5, ProxySchemeSOCKS5H, ProxySchemeHTTP, ProxySchemeHTTPS:
		return true
	}
	return false
}

// Proxy 出口代理条目（spec-proxy-pool §3.4）：连接规格 + 探测结果。
//
// Enabled 是**唯一**可用性开关（§6.3：探测结果不影响账号能否被派发）；
// Routable 由它派生（§4.1），故装载期必须把 Enabled 带上——少了它就无法判定
// 账号可用性。
type Proxy struct {
	ID   int64
	Name string
	// OwnerUID 归属（spec-proxy-pool §3.6，D7 = C 模式）：nil = 平台代理（只由管理员
	// 维护，也只有管理员能分配），非 nil = 该供应商自建（私池：只能承载其主的账号
	// 流量，管理员也遵守这一条）。
	//
	// **写面/可见性事实，不进任何键**：调度判定不看它（Routable 只看 Enabled + 绑定
	// 是否可用），egress 的 proxyDigest 也仍只是连接规格摘要——归属变化不换出口规格，
	// 故它不得渗进任何静态键，否则同一条出口会因为换个主人而断连。
	OwnerUID       *int64
	Scheme         ProxyScheme
	Host           string // IPv6 不带方括号
	Port           int
	Username       string
	Password       string // 明文列（§3.3：DB 是信任边界）
	Enabled        bool
	Remark         string
	LastProbeAt    *time.Time // nil = 从未探测
	LastProbeOK    bool
	LastProbeError string // 已脱敏的错误文本（§5.2）
	// LastProbeIP 是**探测结论列**（spec §3.3/§3.4）：最近一次探测经这条代理看到的
	// 出口 IP，空串 = 未取到。**不进任何键、不影响可路由性**（Enabled 是唯一可用性
	// 开关，Routable 只看它 + 绑定是否可用），只用于回显与排障。
	//
	// **写入侧不变量（不是 DB 约束）**：`LastProbeOK && LastProbeError != ""` ⇔
	// 第一段 TCP 通、第二段取 IP 失败，此时本字段为 ""；LastProbeOK = false ⇒ 第二段
	// 未尝试，本字段同样为 ""。这条等价关系**由 service.ProbeProxy 的那一次整行覆盖写
	// 保证**——DB 层没有 CHECK/触发器，手工改库（运维 SQL、数据修复脚本）不受此限，
	// 读侧不得把它当作数据库保证来推导业务结论。
	LastProbeIP string    // 整行覆盖写：第二段失败必须清空上一次的值
	CreatedAt   time.Time // 管理面读模型回显（§6.1）；不参与任何判定
	UpdatedAt   time.Time // 同上
}

// EgressSpec 把领域代理对象降为出口规格值（egress.Spec）——**唯一的转换点**：
// 摘要（调度静态键）与 Dial 参数（egress 池）都从这一条规格派生，不得在别处
// 重新拼 scheme/host/port。nil（未绑定）= 零值 Spec（直连是一个确定状态，不是
// 缺失）。
func (p *Proxy) EgressSpec() egress.Spec {
	if p == nil {
		return egress.Spec{}
	}
	return egress.New(string(p.Scheme), p.Host, p.Port, p.Username, p.Password)
}

// String 返回脱敏摘要：只出 id / name / scheme / host:port。**用户名与密码恒不出**
// ——它进日志与 err_logs.message（§5.2），任何一路泄漏都是凭据落盘。
func (p *Proxy) String() string {
	if p == nil {
		return "proxy(<nil>)"
	}
	return fmt.Sprintf("proxy{id=%d name=%q scheme=%s addr=%s}",
		p.ID, p.Name, p.Scheme, net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
}
