// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package service 实现管理端业务逻辑：CRUD 校验 + 变更后失效调度/客户端缓存。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// 错误哨兵定义下沉 internal/service/errors（叶子包，单一真相）；此处别名
// re-export 保持既有引用（errors.Is(err, service.ErrXxx)）同一实例语义。
var (
	ErrNotFound     = serviceerr.ErrNotFound
	ErrInvalidInput = serviceerr.ErrInvalidInput
	ErrConflict     = serviceerr.ErrConflict
)

type Store interface {
	TemplateStore
	AccountStore
	GroupStore
	KeyStore
	GroupAssignmentStore
	UserStore
	SettingStore
	RuleStore
	LogStore
	StatStore
	RedemptionStore
	PricingStore
	TemplateExtStore
	AccountExtStore
	// WithTx 在单事务内执行 fn（评审 I-1）：真实仓库为 tx 版 Repository（全部走
	// tx 连接）；fake 为事务语义模拟（fn 内变更先入暂存、成功提交/失败丢弃——
	// 回滚断言的前提）。
	WithTx(ctx context.Context, fn func(repository.TxStore) error) error
}

// UserStore 用户持久化（Phase 3a）。
type UserStore interface {
	CreateUser(ctx context.Context, u *domain.User) (*domain.User, error)
	GetUser(ctx context.Context, id int64) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	// CountUsers 用户总数（注册 bootstrap 用：表空 = 首个注册 = platform_admin）。
	CountUsers(ctx context.Context) (int64, error)
	ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error)
	UpdateUser(ctx context.Context, p *repository.UserPatch) (*domain.User, error)
	UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error
	// 原子资源更新（评审 I-1：兑换码 applier 用；普通 client 与 tx client 均可用）。
	UpdateUserBalance(ctx context.Context, userID, delta int64) error
	UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error
	// CreateTempBalance 创建临时额度行（注册赠品、兑换码兑换等；user_id 外键必
	// 存在）。expiresAt/note 为 nil 时不落该列（nil = 永久；兑换码路径必非零）。
	CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error
	// ListUserTempBalances 用户侧有效临时额度（/api/user/temp-balances：amount > 0
	// 且未过期，expires_at 升序——PG ASC 默认 NULLS LAST，永久最后，与
	// fefoSelectSQL 扣费顺序同源）。
	ListUserTempBalances(ctx context.Context, userID int64) ([]*domain.TempBalance, error)
	// ListTempBalances 管理侧全量临时额度（/api/admin/temp-balances：含过期/用尽/
	// 负扣减行——全量视角；userID 0 = 全部；sort 白名单 + 分页）。
	ListTempBalances(ctx context.Context, q repository.ListQuery, userID int64) ([]*domain.TempBalance, int64, error)
	// ListUserEmails 批量取邮箱（/api/admin/users-top TopN 回填；id IN 一次查询）。
	ListUserEmails(ctx context.Context, ids []int64) (map[int64]string, error)
}

// SettingStore 类型化配置持久化（Phase 3a）。
type SettingStore interface {
	GetSetting(ctx context.Context, key string) (*domain.Setting, error)
	GetAllSettings(ctx context.Context) ([]*domain.Setting, error)
	SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error)
}

// KeyStore 客户端 key 持久化（/api/user/keys 面 + 组删除前置清理）。
type KeyStore interface {
	CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error)
	GetKey(ctx context.Context, id int64) (*domain.Key, error)
	ListKeysByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.Key, int64, error)
	// ListKeys 管理端全量 key 列表（/api/admin/keys：软删过滤 + UserID/GroupID
	// 零值不过滤 + 3 键 sort 白名单；脱敏在 handler 转换面——明文字段不下发）。
	ListKeys(ctx context.Context, q repository.ListQuery) ([]*domain.Key, int64, error)
	// UpdateKey patch 语义更新（S3-F1）：仅 Set 非 nil 字段，nil = 不改——并发
	// 两个 PUT 改不同字段各自生效（对齐 UserPatch 范式）。
	UpdateKey(ctx context.Context, p *repository.KeyPatch) (*domain.Key, error)
	RotateKey(ctx context.Context, id int64, newRaw string) (*domain.Key, error)
	DeleteKey(ctx context.Context, id int64) error
	// DeleteKeysByGroup 组删除前置清理（key.group_id 外键约束；返回被删明文）。
	DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error)
}

// GroupAssignmentStore private 组授予持久化（/api/admin/groups/{id}/assignments +
// /api/user/groups 可选组列表）。
type GroupAssignmentStore interface {
	GrantGroup(ctx context.Context, groupID, userID int64) error
	RevokeGroup(ctx context.Context, groupID, userID int64) error
	// SetAssignmentMultiplier 设置/清除该用户在该组的专属价格倍率（T3.5 修正：
	// 按组；m = nil → 清除为未设置 → 回退组倍率；0 = 免费）。
	SetAssignmentMultiplier(ctx context.Context, groupID, userID int64, m *int) error
	ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error)
	ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error)
	ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error)
}

type TemplateStore interface {
	CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	GetTemplate(ctx context.Context, id int64) (*domain.Template, error)
	// GetTemplatesByIDs 批量取模板（id IN 一次查询——UpdateTemplatesBatch 类型-
	// 格式约束校验用，避免逐 id N+1）；缺失 id 不报错（数量 < 请求数）。
	GetTemplatesByIDs(ctx context.Context, ids []int64) ([]*domain.Template, error)
	ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error)
	UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error)
	DeleteTemplate(ctx context.Context, id int64) error
	DeleteTemplatesBatch(ctx context.Context, ids []int64) error
	UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error
}

type AccountStore interface {
	CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error)
	UpdateAccount(ctx context.Context, a *domain.Account, cooldownUntil *time.Time) (*domain.Account, error)
	DeleteAccount(ctx context.Context, id int64) error
	DeleteAccountsBatch(ctx context.Context, ids []int64) error
	UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error
	// SetAccountGroups 替换账号的全部分组（替换语义；空数组 = 清空）。
	SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error
	// GetAccountGroups 账号的分组 id 列表（编辑回显；账号缺 id 由调用方先
	// GetAccount 拦截）。
	GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error)
}

type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	GetGroup(ctx context.Context, id int64) (*domain.Group, error)
	ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error)
	UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	DeleteGroupsBatch(ctx context.Context, ids []int64) error
	UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error
	// LoadGroupAccounts 单组账号（删组前组内账号校验：含账号组 → 409 拒绝；
	// 与调度器 Loader 同一数据源）。
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
}

// TemplateExtStore 模板类型化扩展持久化（template_ext 1:1；W1 数据层 CRUD，
// 消费接线留给 W3/W4/W6）。
type TemplateExtStore interface {
	UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error)
	GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error)
}

// AccountExtStore 账号类型化鉴权扩展持久化（account_ext 1:1；W1 数据层 CRUD，
// 消费接线留给 W6）。TryInsertAccountExt：首写原子性（ON CONFLICT DO NOTHING
// 先写者胜）——并发双导入同一账号不覆盖不报错。
type AccountExtStore interface {
	UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error)
	TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error)
	GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error)
	// FindAccountExtByCodexKey 组合幂等键查重（Task B 批量导入——(codex_email,
	// codex_account_id)；GetAccountExt 仅按 account_id，查重面不存在）；缺行 →
	// ErrNotFound。
	FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error)
	// WriteOAuthRotation oauth 凭据三列部分更新（Task B 导入 updated 路径——
	// identity/email/其余列零触碰；SDK 轮转回写 sdkbridge.RotationStore 同签名
	// 独立面，repository.AccountExts 一实现双面）；行缺失 → ErrNotFound。
	WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error
	// WritePATKey pat 凭据列部分更新（WriteOAuthRotation 的 pat 对称形态）；
	// 行缺失 → ErrNotFound。
	WritePATKey(ctx context.Context, accountID int64, patKey string) error
}

// RedemptionStore 兑换码 + 兑换审计持久化（Phase 5 计费前基础设施）。
// 兑换事务编排（Redeem）经 Store.WithTx 以 repository.TxStore 面访问。
type RedemptionStore interface {
	CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error
	GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error)
	GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error)
	ListCodes(ctx context.Context, q repository.ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error)
	ListCodeUses(ctx context.Context, codeID int64, q repository.ListQuery) ([]*domain.RedemptionUse, int64, error)
	// ListUsesByUser 某用户的兑换记录（/api/user/redemptions；use + 码联查视图）。
	ListUsesByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.RedemptionRecord, int64, error)
	DeactivateCodes(ctx context.Context, ids []int64) (int64, error)
	GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error)
	CreateUse(ctx context.Context, use *domain.RedemptionUse) error
	IncrementUsed(ctx context.Context, codeID int64) (bool, error)
}

// PricingStore 模型价格持久化（Phase 5 计费价格来源）。source 行级互斥优先级
// manual > litellm 由仓库保证（拉取 upsert 永不覆盖手动价）；service 层快照
// 构建与仓库同语义。
type PricingStore interface {
	UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error)
	UpsertManual(ctx context.Context, m *repository.PricingManual) (*domain.Pricing, error)
	DeleteManual(ctx context.Context, model string) error
	ListPricing(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error)
	GetPricing(ctx context.Context, model string) (*domain.Pricing, error)
	// Task A 双线：图片生成价格（image_price 表；机制与 pricings 同款）。
	UpsertImageFromLiteLLM(ctx context.Context, rows []*domain.ImagePrice) (int, error)
	UpsertImageManual(ctx context.Context, m *repository.ImagePriceManual) (*domain.ImagePrice, error)
	DeleteImageManual(ctx context.Context, model string) error
	ListImagePrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error)
	GetImagePrice(ctx context.Context, model string) (*domain.ImagePrice, error)
	// 价格表三件套：按单元计费功能类价格（function_price 表；机制同款）。
	UpsertFunctionFromLiteLLM(ctx context.Context, rows []*domain.FunctionPrice) (int, error)
	UpsertFunctionManual(ctx context.Context, m *repository.FunctionPriceManual) (*domain.FunctionPrice, error)
	DeleteFunctionManual(ctx context.Context, model string) error
	ListFunctionPrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error)
}

type LogStore interface {
	QueryUsages(ctx context.Context, q repository.UsageQuery) ([]*domain.UsageLog, error)
	QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]*domain.UsageLog, error)
	// ScanUsageAgg 批量账号 usage_logs 区间聚合（/api/admin/accounts/usage 查询面：
	// 单查询 ANY + GROUP BY；无记录账号无键——补零由 service 按 ids 全量组装）。
	ScanUsageAgg(ctx context.Context, accountIDs []int64, from, to time.Time) (map[int64]*domain.UsageAgg, error)
}

type StatStore interface {
	// /api/admin/overview 聚合面（spec 2026-08-14）：SQL 侧聚合（F-P2-2 形态——
	// 服务端 GROUP BY 返回日桶，不拉全行客户端聚合）。
	SummarizeStats(ctx context.Context, from, to time.Time, groupID int64) (*repository.StatSummary, error)
	ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64) ([]*repository.StatDayAgg, error)
	CountOverviewResources(ctx context.Context) (*repository.OverviewResourceCounts, error)
	StatsTrend(ctx context.Context, from, to time.Time, unit string, groupID int64, model string) ([]*domain.StatBucket, error)
	StatsTop(ctx context.Context, from, to time.Time, entityType string, by string, limit int) ([]*domain.EntityStatBucket, error)
	StatsEntityTrend(ctx context.Context, from, to time.Time, unit string, entityType string, entityID int64, model string) ([]*domain.EntityStatBucket, error)
	StatsTTFTSketch(ctx context.Context, from, to time.Time, model string) (*domain.TTFTSummary, error)
	StatsTTFTExact(ctx context.Context, from, to time.Time, entityType string, entityID int64, model string) (*domain.TTFTSummary, error)
}

// Invalidator 管理面变更的去抖定向失效回调（O2 接线矩阵，评审 M-1）：
// service 各 CRUD 在变更落库成功后调用对应方法；实现 = invalidate.Debouncer
// （去抖窗口合并 + 单 goroutine 串行执行 + 按矩阵定向重载，main 装配）。
// key/pricing 变更不走此接口（auth 增量 Upsert/Delete / 内部 reloadPricing，
// 已轻量）。Mark 路径零锁零 DB，不阻塞任何调用方。
type Invalidator interface {
	// Users 用户 CRUD（含创建）与用户余额变更（含 Redeem）：auth + 余额快照
	// 全量 Reload（去抖窗口内合并；新用户必须即刻进余额快照——评审 M-2，
	// 防 ≤10s 402 窗口，回归测试 tools/e2e）。
	Users()
	// Templates 模板（base_url/models/映射）变更：sched 全量 + clients 失效
	// （base_url 变更需按新地址重建 SDK 客户端）。
	Templates()
	// Accounts 账号变更（创建/更新/删除/批量）：sched 组级定向重载受影响组
	// （gids）；keyChanged（upstream_key 变更）→ clients 失效。
	Accounts(gids []int64, keyChanged bool)
	// Multipliers 组倍率 / 用户-组专属倍率（price_multiplier）变更（含组创建/
	// 删除与 group_assignment CRUD——新倍率须即刻进快照）：余额倍率快照定向
	// 刷新（EffectiveMultiplier 陈旧 ≤10s 不可接受）。
	Multipliers()
}

// NopInvalidator 无效化 no-op（测试与无关路径）。
type NopInvalidator struct{}

func (NopInvalidator) Users()                 {}
func (NopInvalidator) Templates()             {}
func (NopInvalidator) Accounts([]int64, bool) {}
func (NopInvalidator) Multipliers()           {}

// RuleReloader 由 rule.RuleEngine 实现：规则 CRUD 后全量重载（invalidate 钩子）。
// 独立于通用 invalidate——规则重载会重置窗口计数，不能随任意资源变更触发。
type RuleReloader interface {
	Reload(ctx context.Context) error
}

// Publisher 多实例 NOTIFY 发布面（#14 T2）：实现 = *notify.Publisher（Publish
// 在 DB 写成功后调用，与 inv.* 调用点并排）；接口化供测试注入 fake（与
// Invalidator 同模式）。nil = 单实例/未装配（T2 过渡），publish no-op。
type Publisher interface {
	Publish(ctx context.Context, ch notify.Change) error
}

// RuntimeProvider 由 scheduler 实现，供账号运行时视图（Runtimes = overview
// 聚合面：账号健康分布/并发水位/err_top 与列表运行时视图同源）。
type RuntimeProvider interface {
	Runtime(accountID int64) (scheduler.RuntimeInfo, bool)
	Runtimes() []scheduler.AccountRuntime
}

// KeyRegistrar 由 proxy.Auth 实现，供客户端 key 变更时增量刷新鉴权快照。
type KeyRegistrar interface {
	Upsert(hash string, meta domain.KeyMeta)
	Delete(hash string)
}

type Service struct {
	store      Store
	sched      RuntimeProvider
	inv        Invalidator // 管理面变更去抖失效（O2 接线矩阵；nil = 不失效）
	pub        Publisher   // 多实例 NOTIFY 发布器（#14 T2；nil = 单实例/未装配，publish no-op）
	ruleReload RuleReloader
	keys       KeyRegistrar
	// local 本地变更分发器（#36 即时重算）：实现 = cmd/server dispatcher
	// （notify.Dispatcher 接口——notify 不 import service，接口定义在 notify
	// 包、装配侧实现，与 Invalidator/Publisher 同依赖方向）。settings 变更
	// 本地直连 Apply（与远端 NOTIFY 同路径：同步 ReloadSettings + 注册表
	// scope 精确重载 auth，gate 预算按新 N 重算）——自播 NOTIFY 被 Listener
	// Src 跳过，本地实例不能依赖 NOTIFY 回环。nil = 未装配（单实例/测试）
	// no-op。
	local notify.Dispatcher
	// settings 设置全量内存快照（默认值 + DB 覆盖）：公开读路径（注册等）
	// 零 DB 直读；仅管理面 UpdateSetting 后重载（低频，无锁）。
	settings atomic.Pointer[map[string]*domain.Setting]
	// pricing 模型价格全量内存快照（key = model 名；表内一行 = 最终生效价）：
	// Phase 5 计费读路径（GetPrice）零 DB 直读；New 初始化 + 管理端改价
	// （UpsertManualPricing/DeleteManualPricing）+ 同步拉取成功后重载（低频，无锁）。
	pricing atomic.Pointer[map[string]*domain.Pricing]
	// imagePrice 图片生成价格全量内存快照（key = model 名；Task A 数据面）：
	// images 端点计费读路径（GetImagePrice）零 DB 直读；重载时机 = 同步拉取
	// 成功后 + 管理端改价后（对齐 pricing 快照，低频无锁）。
	imagePrice atomic.Pointer[map[string]*domain.ImagePrice]
	// functionPrice 按单元计费功能类价格全量内存快照（key = model/功能标识；
	// 价格表三件套）：search 等按单元计费读路径（GetFunctionPrice）零 DB 直读；
	// 重载时机 = 同步拉取成功后 + 管理端改价后（对齐 pricing/imagePrice 快照，
	// 低频无锁）。
	functionPrice atomic.Pointer[map[string]*domain.FunctionPrice]
	// priceFetcher 价格拉取器（pricing.Fetcher 实现）：管理端手动 sync
	// （SyncPricingNow）与 cron worker 共享同一实例（main 装配注入；nil 时
	// SyncPricingNow 返回错误——启动配置缺失，不应发生）。
	priceFetcher pricing.Fetcher
	// usageSnapshots codex 额度快照数据源（*sdkbridge.Codex 满足；AccountUsage
	// 调用——nil = 未装配（测试/单实例），AccountUsage 返回 nil 快照）。
	usageSnapshots CodexUsageSnapshotter
	log            *logx.Logger
}

func New(store Store, sched RuntimeProvider, invalidate Invalidator, pub Publisher, ruleReload RuleReloader, keys KeyRegistrar, log *logx.Logger) *Service {
	s := &Service{store: store, sched: sched, inv: invalidate, pub: pub, ruleReload: ruleReload, keys: keys, log: log}
	// settings 快照构造时首载（注册表不覆盖 settings——NOTIFY 处理路径
	// ReloadSettings 保持既有行为）；pricing 快照首载统一由快照注册表
	// ReloadAll 承担（单一启动入口，消灭"构造即载 + 注册表再刷"双重加载）。
	s.reloadSettings(context.Background())
	return s
}

// SetLocalDispatcher 注入本地变更分发器（#36 本地实例即时重算）：main 装配序
// 上 dispatcher 需要 svc（SettingsReloader）、svc 需要 dispatcher（本地分发）
// ——构造环，svc 构造完成后回填（与 invalidate.Debouncer.SetSettings 同模式）。
// 未注入 = 单实例/测试：settings 变更不做本地 scope 分发（预算重算由 60s
// auth-sync / 下次变更兜底收敛，单实例无多实例分摊语义）。
func (s *Service) SetLocalDispatcher(d notify.Dispatcher) { s.local = d }

// publish 发布一条 NOTIFY 变更（#14 T2）：与现有 inv.* 调用点并排，DB 写成功
// 后调用。失败忽略——NOTIFY 是事件提示，丢一条由 60s 周期兜底收敛（Publisher
// 内部已 Warn），不回滚业务。pub 为 nil（T2 过渡：main 未装配）→ no-op；
// T3 main 装配后必非 nil。
// 空 Change（评审 I-1）：全字段为空（Users/Templates/Clients/Multipliers/
// Keys/Settings/Rules 全 false 且 Groups 空）→ 判空跳过不 Publish（no-op）。
// CreateAccount 无 GroupIDs / UpdateAccount 无变更的空载荷在此统一覆盖（与
// O2 inv.Accounts 空集 no-op 同语义）。
// 发布脱离请求 ctx（评审 I-2）：请求 ctx 取消（客户端断开）不吞 NOTIFY——
// context.WithoutCancel 剥离取消/超时信号仅继承值；NOTIFY 是连接写无悬挂
// 风险，发布必须到最后一个字节。
func (s *Service) publish(ctx context.Context, ch notify.Change) {
	if s.pub == nil {
		return
	}
	if !ch.Users && !ch.Templates && !ch.Clients && !ch.Multipliers &&
		!ch.Keys && !ch.Settings && !ch.Rules && len(ch.Groups) == 0 {
		return // 空 Change：无任何变更语义（评审 I-1）
	}
	_ = s.pub.Publish(context.WithoutCancel(ctx), ch)
}

// validateBaseURL 校验 base_url：可解析、有 scheme/host，且为裸根（不含尾
// /v1）。/v1 是协议细节（aiclient 按格式追加；anthropic SDK 自带 v1 前缀，
// base 含 /v1 会拼出 /v1/v1/messages 404）——约定裸根，防呆拒绝含 /v1。
func validateBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
	}
	if strings.HasSuffix(strings.TrimSuffix(base, "/"), "/v1") {
		return ErrInvalidInput
	}
	return nil
}

func validateTemplate(t *domain.Template) error {
	// 评审 M-1：默认值兜底在 service 层——repo 全字段 Set 会原样写空串，
	// handler 直传也可能缺省；空/缺省在此归一为 api_key，随后才校验合法性。
	if t.CredentialType == "" {
		t.CredentialType = credential.TypeAPIKey
	}
	if !t.CredentialType.Valid() {
		return ErrInvalidInput
	}
	if t.Name == "" {
		return ErrInvalidInput
	}
	// base_url 全类型可选（用户裁决 2026-08-14：模板层级所有类型都可空——codex
	// 走 SDK 默认端点；api_key 静态透传留空则路由时失败，管理面不拦截）；提供时
	// 校验格式（可解析、裸根不含 /v1）。
	if t.BaseURL != "" {
		if err := validateBaseURL(t.BaseURL); err != nil {
			return err
		}
	}
	if len(t.SupportedFormats) == 0 {
		return ErrInvalidInput
	}
	seen := make(map[domain.RequestFormat]bool, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		if !f.Valid() || seen[f] {
			return ErrInvalidInput
		}
		seen[f] = true
	}
	// 类型-格式约束（W1 + Task B/D 扩展）：responses-special/codex-oauth/codex-pat
	// 类型模板支持 resp / resp-ws / openai-images / openai-search 格式（images 直连
	// 为 Task B 用户裁决：responses-special 与 api_key 同支持两图片端点，codex 类型
	// 走 SDK 生图；search 为 Task D 用户裁决：search 端点四类型分派全可达——
	// codex 类型走 SDK Search、api_key/responses-special 静态透传）；api_key 类型全部格式任意。
	if t.CredentialType != credential.TypeAPIKey {
		for _, f := range t.SupportedFormats {
			if f != domain.FormatOpenAIResponses && f != domain.FormatOpenAIResponsesWS && f != domain.FormatOpenAIImages && f != domain.FormatOpenAISearch {
				return ErrInvalidInput
			}
		}
	}
	for f, models := range t.FormatModels {
		if !seen[f] || len(models) == 0 {
			return ErrInvalidInput
		}
		for _, m := range models {
			// 模型必须在可服务集合（排除 format_models 自身，防自引用循环）
			if !slices.Contains(t.Models, m) {
				if _, ok := t.ModelMapping[m]; !ok {
					return ErrInvalidInput
				}
			}
		}
	}
	return nil
}

// validateAccount 基础校验。upstream_key 必填性按模板类型由调用方判定
// （api_key/responses-special 必填；codex-oauth/codex-pat 可选——凭据走
// account_ext，Create/UpdateAccount 处查模板类型）。
func validateAccount(a *domain.Account) error {
	if a.Name == "" || a.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if a.Weight < 0 {
		return ErrInvalidInput
	}
	if a.MaxConcurrency < 1 {
		a.MaxConcurrency = 8
	}
	// 账号级 base_url 提供时复用 validateBaseURL（对齐模板面先例；nil/空串
	// 跳过——create 路径空串已归一 nil，此处双保险）。
	if a.BaseURL != nil && *a.BaseURL != "" {
		if err := validateBaseURL(*a.BaseURL); err != nil {
			return err
		}
	}
	return nil
}

// listSortFields 各资源允许的 sort 白名单（与 repo 层白名单一致，双保险）。
var listSortFields = map[string][]string{
	"templates": {"id", "name", "base_url", "created_at", "updated_at"},
	"accounts":  {"id", "name", "template_id", "status", "cooldown_until", "weight", "max_concurrency", "last_used_at", "created_at", "updated_at"},
	"groups":    {"id", "name", "created_at", "updated_at"},
	"users":     {"id", "email", "role", "status", "max_concurrency", "created_at", "updated_at"},
	"keys":      {"id", "name", "status", "max_concurrency", "quota", "quota_used", "created_at", "updated_at"},
	// 与 repo 层 keyAdminSortFields 白名单一致（双保险；/api/admin/keys——管理端仅
	// id/name/created_at 三键，用户端 8 键白名单见上 "keys"）。
	"admin_keys": {"id", "name", "created_at"},
	// 与 repo 层 redemptionCodeSortFields 白名单一致（双保险）。
	"redemption_codes": {"id", "code", "type", "value", "max_uses", "used_count", "status", "created_by", "created_at", "updated_at"},
	// 与 repo 层 redemptionUseSortFields 白名单一致（双保险；/api/user/redemptions）。
	"redemption_uses": {"id", "code_id", "user_id", "value", "created_at"},
	// 与 repo 层 tempBalanceSortFields 白名单一致（双保险；/api/admin/temp-balances）。
	"temp_balances": {"expires_at", "amount", "created_at"},
	// 与 repo 层 pricingSortFields 白名单一致（双保险；/api/admin/pricing）。
	"pricing": {"model", "updated_at"},
	// 与 repo 层 imagePriceSortFields 白名单一致（双保险；/api/admin/image-price）。
	"image_price": {"model", "updated_at"},
	// 与 repo 层 functionPriceSortFields 白名单一致（双保险；/api/admin/function-prices）。
	"function_price": {"model", "updated_at"},
}

// validateListQuery sort/order 白名单校验（非法 → ErrInvalidInput；handler 依赖此 400）。
func validateListQuery(q repository.ListQuery, sortFields []string) error {
	if q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		return ErrInvalidInput
	}
	if q.Sort != "" && !slices.Contains(sortFields, q.Sort) {
		return ErrInvalidInput
	}
	return nil
}

// --- 批量操作校验与错误映射 ---

// validateIDs ids 1–100 且去重（handler 已做，service 兜底）。
func validateIDs(ids []int64) error {
	if len(ids) == 0 || len(ids) > 100 {
		return ErrInvalidInput
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateTemplatePatch 校验批量 patch 提供的字段（nil = 未提供，跳过）。
// 多格式语义与 validateTemplate 对齐：supported_formats 非空/枚举/去重；
// format_models 的 key 必须合法枚举且列表非空；两者同批提供时 key 必须
// ∈ supported_formats（跨字段子集校验，与单 PUT 的 validateTemplate 一致）。
func validateTemplatePatch(p repository.TemplatePatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.BaseURL != nil {
		if err := validateBaseURL(*p.BaseURL); err != nil {
			return err
		}
	}
	var supported map[domain.RequestFormat]bool
	if p.SupportedFormats != nil {
		if len(*p.SupportedFormats) == 0 {
			return ErrInvalidInput
		}
		supported = make(map[domain.RequestFormat]bool, len(*p.SupportedFormats))
		for _, f := range *p.SupportedFormats {
			if !f.Valid() || supported[f] {
				return ErrInvalidInput
			}
			supported[f] = true
		}
	}
	if p.FormatModels != nil {
		for f, models := range *p.FormatModels {
			if !f.Valid() || len(models) == 0 {
				return ErrInvalidInput
			}
			if supported != nil && !supported[f] {
				return ErrInvalidInput
			}
		}
	}
	return nil
}

// validateAccountPatch 校验批量 patch 提供的字段（nil = 未提供，跳过）。
func validateAccountPatch(p repository.AccountPatch) error {
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.UpstreamKey != nil && *p.UpstreamKey == "" {
		return ErrInvalidInput
	}
	// 批量 base_url 三态（C1）：空串 = 清空（合法）；非空时复用 validateBaseURL。
	if p.BaseURL != nil && *p.BaseURL != "" {
		if err := validateBaseURL(*p.BaseURL); err != nil {
			return err
		}
	}
	if p.TemplateID != nil && *p.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if p.Weight != nil && *p.Weight < 0 {
		return ErrInvalidInput
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency < 1 {
		return ErrInvalidInput
	}
	// GroupIDs：nil/空数组合法（nil = 不变，[] = 清空）；非空要求长度 ≤ 100、
	// 去重、元素 > 0（与 template_id 对齐：非法 id 值在 service 层拦截为 400，
	// 不落到 repo 层变 404 语义）。
	if p.GroupIDs != nil {
		if len(*p.GroupIDs) > 100 {
			return ErrInvalidInput
		}
		seen := make(map[int64]struct{}, len(*p.GroupIDs))
		for _, id := range *p.GroupIDs {
			if id <= 0 {
				return ErrInvalidInput
			}
			if _, ok := seen[id]; ok {
				return ErrInvalidInput
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

// mapRepoErr 存储错误映射：repository.ErrNotFound → ErrNotFound（保留缺失 id
// 详情，404 响应带 "id=5 missing"）；repository.ErrConflict → ErrConflict
// （保留冲突详情，409 响应带 "name=\"x\""）。其他错误原样返回。
func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		detail := strings.TrimPrefix(err.Error(), repository.ErrNotFound.Error()+": ")
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	case errors.Is(err, repository.ErrConflict):
		detail := strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": ")
		return fmt.Errorf("%w: %s", ErrConflict, detail)
	}
	return err
}
