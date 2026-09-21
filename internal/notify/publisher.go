// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package notify 多实例变更广播（基础层）：PG LISTEN/NOTIFY 定向刷新。
//
// 架构（设计文档 docs/superpowers/plans/2026-08-10-multi-instance-design.md
// §2）：管理面变更落库成功后经 Publisher 发一条 NOTIFY（单 channel
// c3api_invalidate，紧凑 JSON 载荷与 invalidate.State 同构）；每实例一个
// Listener worker（Name="notify"）LISTEN 该 channel，解析后调注入的
// Dispatcher（main 装配）转发现有 invalidate.Debouncer 的 Mark 方法——
// 本地/远端变更共享同一去抖窗口，天然合并去重，Debouncer 本体零改动。
//
// 设计要点：
//   - 载荷守卫：PG NOTIFY 载荷上限 8000B，批量账号变更的 Groups 集合可能
//     超限 → marshal 后 > 6KB 时丢弃 Groups 并置 Templates=true（降级 sched
//     全量重载，sched 全量包含组级重载，语义仍正确）。
//   - Src 自播跳过：Publisher 自动填 src=实例 ID，接收端跳过自身发布的
//     NOTIFY（省一次重复 reload）。
//   - 计费扣费路径绝不发布 NOTIFY（每 flush 即风暴），保持现状语义。
package notify

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/pkg/logx"
)

// Channel NOTIFY 频道名（单 channel：与去抖器合并 State 对齐——一类资源一个
// channel 则监听器多、且与 debouncer 的合并 State 不对齐）。
const Channel = "c3api_invalidate"

// notifySQL 发布语句：pg_notify(text, text) → void（无返回值）。
const notifySQL = "select pg_notify('" + Channel + "', $1)"

// maxPayloadBytes 载荷守卫阈值：PG NOTIFY 载荷上限 8000B，留 ~2KB 余量
// （pgx 参数编码/服务器解析边界开销），超限即降级 full（设计文档 §2.2 / R9）。
const maxPayloadBytes = 6 * 1024

// Change 一条 NOTIFY 变更载荷（与 invalidate.State 同构，紧凑 JSON：
// omitempty + 短字段名，尽量压载荷）。
type Change struct {
	// V 载荷版本（当前 1）；未来字段演进/接收端兼容判读用。
	V int `json:"v"`
	// Users KindUsers：用户 CRUD（含创建）/余额变更 → auth + 余额快照全量。
	Users bool `json:"users,omitempty"`
	// Templates KindTemplates：模板（base_url/models/映射）变更 → sched 全量
	// + clients 失效。载荷守卫降级 full 时也置此位。
	Templates bool `json:"templates,omitempty"`
	// Clients KindClients：aiclient 工厂失效（模板 base_url / 账号
	// upstream_key 变更）。
	Clients bool `json:"clients,omitempty"`
	// Multipliers KindMultipliers：组倍率 / 用户-组专属倍率变更 → 余额倍率
	// 快照定向刷新。
	Multipliers bool `json:"multipliers,omitempty"`
	// Keys key CRUD 缺口（创建/轮换/删除/改额度）→ auth 快照全量 Reload
	//（v1 不做增量定向）。
	Keys bool `json:"keys,omitempty"`
	// Settings settings 快照变更（UpdateSetting）→ settings 快照重载。
	Settings bool `json:"settings,omitempty"`
	// Rules 规则表变更（规则 CRUD）→ 规则表重载（重载清窗口计数，全实例
	// 同步执行语义）。
	Rules bool `json:"rules,omitempty"`
	// Pricing 定价快照变更（价格写面）→ 定价快照重载（缺价 402 窗口跨实例
	// 收敛，不等重启）。
	Pricing bool `json:"pricing,omitempty"`
	// Groups 组级定向（账号变更的受影响组 id）。
	Groups []int64 `json:"groups,omitempty"`
	// Src 发布实例 ID：接收端跳过自播（省一次重复 reload）。Publisher 自动
	// 填，调用方无需关心。
	Src string `json:"src,omitempty"`
}

// IsEmpty 空载荷判定：8 个变更位全 false 且 Groups 为空。V/Src 不参与判定——
// V 恒存在（json 无 omitempty），Src 由 Publisher 发布时自动填充，调用方
// 构造时均为空。service.publish 用此前置跳过无意义 NOTIFY（
// 创建无分组 / 补丁无分组变更的空载荷在此统一覆盖）。
func (c Change) IsEmpty() bool {
	return !c.Users && !c.Templates && !c.Clients && !c.Multipliers &&
		!c.Keys && !c.Settings && !c.Rules && !c.Pricing && len(c.Groups) == 0
}

// Marshal 序列化 Change（含载荷守卫）：估算 marshal 后长度 > maxPayloadBytes
// → 丢弃 Groups 并置 Templates=true（降级 sched 全量重载——sched 全量包含
// 组级重载，语义仍正确）。
func Marshal(c Change) []byte {
	payload, _ := json.Marshal(c) // Change 仅基本类型 + []int64，marshal 不可能失败
	if len(payload) > maxPayloadBytes && len(c.Groups) > 0 {
		c.Groups = nil
		c.Templates = true
		payload, _ = json.Marshal(c)
	}
	return payload
}

// Unmarshal 解析 NOTIFY 载荷。
func Unmarshal(data []byte) (Change, error) {
	var c Change
	if err := json.Unmarshal(data, &c); err != nil {
		return Change{}, err
	}
	return c, nil
}

// Execer 发布端依赖的最小执行面：*pgxpool.Pool（生产，repository.OpenPG 的
// 池）与 pgxmock.PgxPoolIface（测试）均满足。
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Publisher NOTIFY 发布器：marshal → SELECT pg_notify('c3api_invalidate', $1)。
// 独立持有 pool 引用（构造注入，走现有 repository.OpenPG 的 pgxpool），不
// 新建连接。
type Publisher struct {
	pool Execer
	src  string // 实例 ID（自动写入 Change.Src，接收端跳过自播）
	log  *logx.Logger
}

// NewPublisher 构造发布器。pool 必填（未注入时 Publish 返回显式错误——装配
// 缺失显式暴露，不静默 no-op）。
func NewPublisher(pool Execer, src string, log *logx.Logger) *Publisher {
	return &Publisher{pool: pool, src: src, log: log}
}

// Publish 发布一条变更（在 DB 写成功后调用，与现有 inv.* 调用点并排）。
// 发布失败返回错误——NOTIFY 是"事件提示"，丢一条由去抖器 60s 周期兜底收敛
// （设计文档 §2.3），调用方可按现状 inv.* 语义忽略或告警。
func (p *Publisher) Publish(ctx context.Context, ch Change) error {
	if p.pool == nil {
		return fmt.Errorf("notify: publisher pool not configured")
	}
	ch.Src = p.src // 接收端跳过自播（Src 含实例随机 nonce，跨实例唯一；空 src = 不跳过，单实例部署无碍）
	payload := Marshal(ch)
	_, err := p.pool.Exec(ctx, notifySQL, string(payload))
	if err != nil && p.log != nil {
		p.log.Warn("notify publish failed", logx.String("channel", Channel), logx.Error(err))
	}
	return err
}
