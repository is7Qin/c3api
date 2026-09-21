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
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/pkg/logx"
)

// 错误哨兵定义下沉 internal/service/errors（叶子包，单一真相）；此处别名
// re-export 保持既有引用（errors.Is(err, service.ErrXxx)）同一哨兵实例语义。
var (
	ErrNotFound              = serviceerr.ErrNotFound
	ErrInvalidInput          = serviceerr.ErrInvalidInput
	ErrConflict              = serviceerr.ErrConflict
	ErrPreconditionFailed    = serviceerr.ErrPreconditionFailed
	ErrTooManyRequests       = serviceerr.ErrTooManyRequests
	ErrMailNotConfigured     = serviceerr.ErrMailNotConfigured
	ErrMailQueueFull         = serviceerr.ErrMailQueueFull
	ErrMailChannelTestFailed = serviceerr.ErrMailChannelTestFailed
)

// EmailVerificationRequired 缺验证码时 400 响应的固定哨兵片段（前端发现机制）。
const EmailVerificationRequired = "email verification required"

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
	EmailTemplateStore
	// EmailCodeStore 不在复合面：验证码已迁 Redis（spec 2026-08-25-emailcode-
	// redis-migration §2.2/§2.3），经 New 的 ServiceDeps.EmailCodeStore 独立注入，
	// repository 实现已随 PG 验证码表卸载。
	// WithTx 在单事务内执行 fn：真实仓库为 tx 版 Repository（全部走
	// tx 连接）；fake 为事务语义模拟（fn 内变更先入暂存、成功提交/失败丢弃——
	// 回滚断言的前提）。
	WithTx(ctx context.Context, fn func(repository.TxStore) error) error
}

// UserStore 用户持久化。
type UserStore interface {
	CreateUser(ctx context.Context, u *domain.User) (*domain.User, error)
	GetUser(ctx context.Context, id int64) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	// CountUsers 用户总数（注册 bootstrap 用：表空 = 首个注册 = platform_admin）。
	CountUsers(ctx context.Context) (int64, error)
	ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error)
	UpdateUser(ctx context.Context, p *repository.UserPatch) (*domain.User, error)
	UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error
	// 原子资源更新（兑换码 applier 用；普通 client 与 tx client 均可用）。
	UpdateUserBalance(ctx context.Context, userID, delta int64) error
	UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error
	UpdateUserBalanceWarningThreshold(ctx context.Context, userID int64, threshold int64) (*domain.User, int64, error)
	// CreateTempBalance 创建临时额度行（注册赠品、兑换码兑换等；user_id 外键必
	// 存在）。expiresAt/note 为 nil 时不落该列（nil = 永久；兑换码路径必非零）。
	CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error
	// ListUserTempBalances 用户侧有效临时额度（/api/user/temp-balances：amount > 0
	// 且未过期，expires_at 升序——PG ASC 默认 NULLS LAST，永久最后，与
	// SettleFefoBatch 扣费顺序同源）。
	ListUserTempBalances(ctx context.Context, userID int64) ([]*domain.TempBalance, error)
	// ListTempBalances 管理侧全量临时额度（/api/admin/temp-balances：含过期/用尽/
	// 负扣减行——全量视角；userID 0 = 全部；sort 白名单 + 分页）。
	ListTempBalances(ctx context.Context, q repository.ListQuery, userID int64) ([]*domain.TempBalance, int64, error)
	// ListUserEmails 批量取邮箱（/api/admin/users-top TopN 回填；id IN 一次查询）。
	ListUserEmails(ctx context.Context, ids []int64) (map[int64]string, error)
}

// SettingStore 类型化配置持久化。
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
	// UpdateKey patch 语义更新：仅 Set 非 nil 字段，nil = 不改——并发
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
	// SetAssignmentMultiplier 设置/清除该用户在该组的专属价格倍率：
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
	FailAccountCAS(ctx context.Context, id int64, expectedRevision int64, source string, failedAt time.Time, reason string) error
	RecoverAccountCAS(ctx context.Context, id int64, expectedRevision int64) error
	DeleteAccount(ctx context.Context, id int64) error
	DeleteAccountsBatch(ctx context.Context, ids []int64) error
	UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) ([]repository.AccountWriteResult, error)
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

// TemplateExtStore 模板类型化扩展持久化（template_ext 1:1；数据层 CRUD，
// 消费接线留给后续扩展）。
type TemplateExtStore interface {
	UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error)
	GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error)
}

// AccountExtStore 账号类型化鉴权扩展持久化（account_ext 1:1；数据层 CRUD，
// 消费接线留给后续扩展）。TryInsertAccountExt：首写原子性（ON CONFLICT DO NOTHING
// 先写者胜）——并发双导入同一账号不覆盖不报错。
//
// 写入面只暴露**围栏动词**：管理面全量写走 AdminUpsertAccountExtCAS（CAS +
// 推进 C，按值推进 K），凭据轮转走 AdminWrite*CAS；无围栏的 UpsertAccountExt
// 只作为事务内动词存在（TxStore，批量导入在单事务里建行），不投给 service。
type AccountExtStore interface {
	AdminUpsertAccountExtCAS(ctx context.Context, e *domain.AccountExt, expectedRevision int64) (*domain.AccountExt, error)
	TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error)
	GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error)
	// FindAccountExtByCodexKey 组合幂等键查重（批量导入——(codex_email,
	// codex_account_id)；GetAccountExt 仅按 account_id，查重面不存在）；缺行 →
	// ErrNotFound。
	FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error)
	// WriteOAuthRotation oauth 凭据三列部分更新（SDK 轮转回写，unfenced，不增 revision）。
	WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error
	AdminWriteOAuthRotationCAS(ctx context.Context, accountID int64, expectedRevision int64, at, rt string, expiresAt *time.Time) error
	AdminWritePATKeyCAS(ctx context.Context, accountID int64, expectedRevision int64, patKey string) error
}

// EmailTemplateStore 邮件模板持久化。
type EmailTemplateStore interface {
	GetEmailTemplate(ctx context.Context, purpose string) (*domain.EmailTemplate, error)
	ListEmailTemplates(ctx context.Context) ([]*domain.EmailTemplate, error)
	UpsertEmailTemplate(ctx context.Context, purpose, subject, bodyText string) (*domain.EmailTemplate, error)
	DeleteEmailTemplate(ctx context.Context, purpose string) error
}

// EmailCodeStore 验证码持久化。
type EmailCodeStore interface {
	GetEmailCode(ctx context.Context, email, purpose string) (*domain.EmailCode, error)
	UpsertEmailCode(ctx context.Context, email, purpose, sha256 string, expiresAt time.Time) (*domain.EmailCode, error)
	IncrementEmailCodeAttempts(ctx context.Context, email, purpose string) (int, error)
	DeleteEmailCode(ctx context.Context, email, purpose string) error
}

// RedemptionStore 兑换码 + 兑换审计持久化（计费前基础设施）。
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

// PricingStore 统一价格持久化。
type PricingStore interface {
	UpsertPriceEntriesFromLiteLLM(ctx context.Context, rows []*domain.PriceEntry) (int, error)
	UpsertPriceVariantsFromLiteLLM(ctx context.Context, variants []*domain.PriceVariant) (int, error)
	UpsertPriceEntryManual(ctx context.Context, m *repository.PriceEntryManual) (*domain.PriceEntry, error)
	DeletePriceEntryManual(ctx context.Context, model string) error
	ListPriceEntries(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, provider *string, model string) ([]*domain.PriceEntry, int64, error)
	GetPriceEntry(ctx context.Context, model string) (*domain.PriceEntry, error)
	ListPriceVariants(ctx context.Context, model string) ([]*domain.PriceVariant, error)
	ListAllPriceVariants(ctx context.Context) ([]*domain.PriceVariant, error)
	ReplacePriceVariants(ctx context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error)
	ManualEntryModels(ctx context.Context) ([]string, error)
}

type LogStore interface {
	QueryUsages(ctx context.Context, q repository.UsageQuery) ([]*domain.UsageLog, error)
	QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]*domain.UsageLog, error)
	// ScanUsageAgg 批量账号 usage_logs 区间聚合（/api/admin/accounts/usage 查询面：
	// 单查询 ANY + GROUP BY；无记录账号无键——补零由 service 按 ids 全量组装）。
	ScanUsageAgg(ctx context.Context, accountIDs []int64, from, to time.Time) (map[int64]*domain.UsageAgg, error)
}

type StatStore interface {
	// /api/admin/overview 聚合面（spec 2026-08-14）：SQL 侧聚合——
	// 服务端 GROUP BY 返回日桶，不拉全行客户端聚合）。zone = 请求浏览器时区
	// （handler 边界校验；nil/UTC = 现状 cube 路径）；repo 内按
	// domain.ZoneCubeExact 路由 cube 重组 vs 原始行精确聚合。
	SummarizeStats(ctx context.Context, from, to time.Time, groupID int64, zone *time.Location) (*repository.StatSummary, error)
	ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64, zone *time.Location) ([]*repository.StatDayAgg, error)
	CountOverviewResources(ctx context.Context) (*repository.OverviewResourceCounts, error)
	StatsTrend(ctx context.Context, from, to time.Time, unit string, groupID int64, model string, zone *time.Location) ([]*domain.StatBucket, error)
	StatsTop(ctx context.Context, from, to time.Time, entityType string, by string, limit int) ([]*domain.EntityStatBucket, error)
	StatsEntityTrend(ctx context.Context, from, to time.Time, unit string, entityType string, entityID int64, model string, zone *time.Location) ([]*domain.EntityStatBucket, error)
	StatsTTFTSketch(ctx context.Context, from, to time.Time, model string) (*domain.TTFTSummary, error)
	StatsTTFTExact(ctx context.Context, from, to time.Time, entityType string, entityID int64, model string) (*domain.TTFTSummary, error)
}

// Invalidator 管理面变更的去抖定向失效回调（接线矩阵）：
// service 各 CRUD 在变更落库成功后调用对应方法；实现 = invalidate.Debouncer
// （去抖窗口合并 + 单 goroutine 串行执行 + 按矩阵定向重载，main 装配）。
// key/pricing 变更不走此接口（auth 增量 Upsert/Delete / 内部 reloadPricing，
// 已轻量）。Mark 路径零锁零 DB，不阻塞任何调用方。
type Invalidator interface {
	// Users 用户 CRUD（含创建）与用户余额变更（含 Redeem）：auth + 余额快照
	// 全量 Reload（去抖窗口内合并；新用户必须即刻进余额快照——评审
	// 防 ≤10s 402 窗口，回归测试 tools/e2e）。
	Users()
	// Templates 模板（base_url/models/映射）变更：sched 全量 + clients 失效
	// （base_url 变更需按新地址重建 SDK 客户端）。
	Templates()
	// Accounts 账号变更（创建/更新/删除/批量）：sched 组级定向重载受影响组
	// （gids）；keyChanged（身份类字段变更）→ clients 失效。
	Accounts(gids []int64, keyChanged bool)
	// Multipliers 组倍率 / 用户-组专属倍率（price_multiplier）变更（含组创建/
	// 删除与 group_assignment CRUD——新倍率须即刻进快照）：余额倍率快照定向
	// 刷新（EffectiveMultiplier 陈旧 ≤10s 不可接受）。
	Multipliers()
	// Settings settings 变更（UpdateSetting）：scope 声明方（auth）快照重载
	// （gate 预算按新 N 重算；≤200ms 去抖窗口与其余 Kind 一致）；settings
	// 快照由 UpdateSetting 自身同步刷新后才 mark（顺序不变量：快照刷新
	// 先于 auth.Reload）。
	Settings()
}

// NopInvalidator 无效化 no-op（测试与无关路径）。
type NopInvalidator struct{}

func (NopInvalidator) Users()                 {}
func (NopInvalidator) Templates()             {}
func (NopInvalidator) Accounts([]int64, bool) {}
func (NopInvalidator) Multipliers()           {}
func (NopInvalidator) Settings()              {}

// RuleReloader 由 rule.RuleEngine 实现：规则 CRUD 后全量重载（invalidate 钩子）。
// 独立于通用 invalidate——规则重载会重置窗口计数，不能随任意资源变更触发。
type RuleReloader interface {
	Reload(ctx context.Context) error
}

// Publisher 多实例 NOTIFY 发布面：实现 = *notify.Publisher（Publish
// 在 DB 写成功后调用，与 inv.* 调用点并排）；接口化供测试注入 fake（与
// Invalidator 同模式）。nil = 单实例/未装配（过渡），publish no-op。
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
	store Store
	// emailCodes 验证码存储（Redis 实现，spec 2026-08-25-emailcode-redis-migration
	// §2.2）：New 经 ServiceDeps.EmailCodeStore 一次性注入，Redis 必选 ⇒ 非 nil
	//（nil panic fail-fast）。
	emailCodes EmailCodeStore
	sched      RuntimeProvider
	inv        Invalidator // 管理面变更去抖失效（接线矩阵；nil = 不失效）
	pub        Publisher   // 多实例 NOTIFY 发布器（nil = 单实例/未装配，publish no-op）
	ruleReload RuleReloader
	keys       KeyRegistrar
	// settings 设置全量内存快照（默认值 + DB 覆盖）：Service 与 MailWorker
	// 同源共享单个 *settingssnap.Snapshot（根因重开——单指针，无双快照
	// 分叉；NOTIFY 只刷这一处）。公开读路径零 DB 直读；仅管理面
	// UpdateSetting 后重载（低频，无锁）。
	settings *settingssnap.Snapshot
	// priceSnapshot 统一价格快照：entries + variants
	priceSnapshot atomic.Pointer[priceSnapshot]
	// recoverProber 恢复→PROBING 健康写入面（New 经 ServiceDeps.RecoverProber
	// 注入；nil = 未装配，recover 仅完成持久恢复——调度器同步周期兜底）。
	recoverProber RecoverProber
	// recoverLatch 恢复→latch 显式释放面（New 经 ServiceDeps.RecoverLatch
	// 注入；nil = 未装配，跳过释放）。
	recoverLatch RecoverLatchReleaser
	// recoverHealthClear 恢复→健康记录显式清除面（New 经
	// ServiceDeps.RecoverHealthClear 注入；nil = 未装配，跳过清除）。
	recoverHealthClear RecoverHealthClearer
	// compileNotify 路由编译触发面（New 经 ServiceDeps.CompileNotify
	// 一次性注入；nil = 未装配，定价写面静默——仅编译道装配后有效。
	// 调用方承诺非阻塞，见 pricing.go）。
	compileNotify               func()
	mailEnqueue                 func(MailSendTask) error
	clearBalanceWarningCooldown func(context.Context, int64, int64) error
	// defaultMaxConcurrency 创建期 max_concurrency 缺省落值（main 经
	// ServiceDeps.DefaultMaxConcurrency 一次性注入；0 = 未装配，create 视为
	// 未提供由校验拒绝）。
	defaultMaxConcurrency int
	tzLoc                 *time.Location
	// statsRawSpan 浏览器时区原始行分组路径的窗口上限（usage_logs/err_logs
	// 原始行保留期决定）：New 经 ServiceDeps.StatsRawRetentionDays 换算（>0 →
	// (days+1)×24h 窗口上限 + days 保留兜底；<=0 → 0 = 不限窗口且无兜底，
	// 分区保留被禁用，既无固定 horizon 也无保证存留期；换算细节与"宁 400
	// 不静默残缺"见 New）。校验见 validateZoneSpan。
	statsRawSpan time.Duration
	// statsRawRetentionDays statsRawSpan 背后的正保留天数（main 传 usage_logs
	// 与 err_logs 的最小正保留——raw 读两表，两者都须完整覆盖）：retention
	// 兜底用——窗口起点早于保证存留的分区 cutoff 时，行已被 retention DROP，
	// 宁 400 不静默残缺；0 = 禁用/不限（跳过兜底）。
	statsRawRetentionDays int
	// statsNow 当前时间源（nil = time.Now；Service 测试注入固定时钟）。
	statsNow func() time.Time
	log      *logx.Logger
}

// ServiceDeps New 的尾部一次性依赖（SetEmailCodeStore /
// SetTimeLocation / SetStatsRawSpan / SetBalanceWarningCooldownCleaner /
// SetRecoverProber 五个事后回填折叠进构造：编译通知回填
// 折叠进构造——零语义变化，各字段 nil/零值语义与原 setter 完全一致）。
// 尾部 struct 而非位置参数：New 本就 7 参，位置参数会冲到 12+ 个（>3 参
// smell），具名字段自文档且调用点可只填所需（新增 CompileNotify 字段零
// 调用点 churn：缺省 nil = 未装配静默，与原 setter 未调用同语义）。
type ServiceDeps struct {
	// EmailCodeStore 验证码存储（Redis 实现，必选依赖）：nil 直接 panic
	// fail-fast（与原 SetEmailCodeStore 同纪律——生产误接线必须启动即炸，
	// 无降级路径）。
	EmailCodeStore EmailCodeStore
	// TimeLocation 定价时段解释用时区：nil = 进程本地（现状），
	// 非 nil = at.In(tzLoc) 后再进 domain.ResolveEntryPrices（零热路径额外 DB/锁）。
	TimeLocation *time.Location
	// StatsRawRetentionDays 原始行分组 horizon 背后的正保留天数（main 传
	// usage_logs 与 err_logs 的最小正保留——raw 读两表，两者都须完整覆盖）：
	// >0 → (days+1)×24h 窗口上限 + days 保留兜底 cutoff（1d DST/日界日历
	// 余量——默认 7d → 8d，与 MaxStatsRawSpan 缺省同值）；<=0 → 0 = 不限
	// 窗口且无兜底（分区保留被禁用）。绝不把 horizon 报得比配置保留期更长
	// ——超限窗口宁 400 不静默残缺。
	StatsRawRetentionDays int
	// ClearBalanceWarningCooldown 余额预警偏好变更后的 Redis 已知键清理：
	// nil = 禁用清理（仅做偏好持久化）。
	ClearBalanceWarningCooldown func(context.Context, int64, int64) error
	// RecoverProber 恢复→PROBING 健康写入面：nil = 跳过 PROBING 写
	// （调度器同步周期兜底收敛）。
	RecoverProber RecoverProber
	// RecoverLatch 恢复→latch 显式释放面：nil = 跳过释放。
	RecoverLatch RecoverLatchReleaser
	// RecoverHealthClear 恢复→健康记录显式清除面：nil = 跳过清除。
	RecoverHealthClear RecoverHealthClearer
	// CompileNotify 路由编译触发面（定价写面统一出口的 notify 回调）：
	// nil = 未装配（测试/降级路径），定价写面纯重载 + 仍发布 NOTIFY，
	// 同值写静默纪律见 reloadPricingAndNotifyCompiler。生产装配
	// scheduler.RequestCompile（func 值注入，无 import 环）。
	CompileNotify func()
	// MailEnqueue 邮件异步入队面（auth_email.go 经此入队；nil = 未装配 →
	// SendRegisterCode 退化为 ErrMailNotConfigured）。生产经构造一次性注入
	// mailW.Enqueue（根因重开——构造参数，零事后回填）。
	MailEnqueue func(MailSendTask) error
	// SettingsSnapshot settings 快照共享指针（根因重开）：生产由 main
	// 一次构造、Service 与 MailWorker 同源共享；nil = New 内自建（测试/
	// 字面量 Service 兼容——各测自有快照，无跨实例语义）。
	SettingsSnapshot *settingssnap.Snapshot
	// DefaultMaxConcurrency 账号创建期 max_concurrency 缺省落值：main 传
	// cfg.Scheduler.DefaultMaxConcurrency；0 = 未装配（create 显式给 0 即
	// 落 0，由校验拒绝而非静默钳制）。
	DefaultMaxConcurrency int
}

func New(store Store, sched RuntimeProvider, invalidate Invalidator, pub Publisher, ruleReload RuleReloader, keys KeyRegistrar, log *logx.Logger, deps ServiceDeps) *Service {
	if deps.EmailCodeStore == nil {
		panic("service: New(nil EmailCodeStore): Redis 是必选依赖，验证码存储无降级路径")
	}
	s := &Service{store: store, sched: sched, inv: invalidate, pub: pub, ruleReload: ruleReload, keys: keys, log: log,
		emailCodes: deps.EmailCodeStore, tzLoc: deps.TimeLocation, recoverProber: deps.RecoverProber,
		recoverLatch: deps.RecoverLatch, recoverHealthClear: deps.RecoverHealthClear,
		defaultMaxConcurrency:       deps.DefaultMaxConcurrency,
		compileNotify:               deps.CompileNotify,
		mailEnqueue:                 deps.MailEnqueue,
		clearBalanceWarningCooldown: deps.ClearBalanceWarningCooldown}
	if deps.SettingsSnapshot != nil {
		s.settings = deps.SettingsSnapshot
	} else {
		s.settings = settingssnap.New(store, log)
	}
	if deps.StatsRawRetentionDays > 0 {
		s.statsRawSpan = time.Duration(deps.StatsRawRetentionDays+1) * 24 * time.Hour
		s.statsRawRetentionDays = deps.StatsRawRetentionDays
	} else {
		s.statsRawSpan = 0
		s.statsRawRetentionDays = 0
	}
	// settings 快照构造时首载（注册表不覆盖 settings——NOTIFY 处理路径
	// ReloadSettings 保持既有行为）；pricing 快照首载统一由快照注册表
	// ReloadAll 承担（单一启动入口，消灭"构造即载 + 注册表再刷"双重加载）。
	s.reloadSettings(context.Background())
	return s
}

// publish 发布一条 NOTIFY 变更：与现有 inv.* 调用点并排，DB 写成功
// 后调用。失败忽略——NOTIFY 是事件提示，丢一条由 60s 周期兜底收敛（Publisher
// 内部已 Warn），不回滚业务。pub 为 nil（过渡：main 未装配）→ no-op；
// main 装配后必非 nil。
// 空 Change：notify.Change.IsEmpty()（7 变更位全 false 且 Groups
// 空）→ 判空跳过不 Publish（no-op）。创建无分组 / 补丁无分组变更的空载荷在此
// 统一覆盖（与 inv.Accounts 的空分组集 no-op 同语义）。
// 发布脱离请求 ctx：请求 ctx 取消（客户端断开）不吞 NOTIFY——
// context.WithoutCancel 剥离取消/超时信号仅继承值；NOTIFY 是连接写无悬挂
// 风险，发布必须到最后一个字节。
func (s *Service) publish(ctx context.Context, ch notify.Change) {
	if s.pub == nil {
		return
	}
	if ch.IsEmpty() {
		return // 空 Change：无任何变更语义
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

// dedupFormats supported_formats 非空+枚举合法+去重校验，返回去重集合（供
// FormatModels 跨字段子集校验复用）。validateTemplate / validateTemplatePatch
// 共用——两者语义一致（空列表非法；patch 侧 nil=未提供由调用方先行跳过）。
func dedupFormats(fs []domain.RequestFormat) (map[domain.RequestFormat]bool, error) {
	if len(fs) == 0 {
		return nil, ErrInvalidInput
	}
	seen := make(map[domain.RequestFormat]bool, len(fs))
	for _, f := range fs {
		if !f.Valid() || seen[f] {
			return nil, ErrInvalidInput
		}
		seen[f] = true
	}
	return seen, nil
}

func validateTemplate(t *domain.Template) error {
	// 默认值兜底在 service 层——repo 全字段 Set 会原样写空串，
	// handler 直传也可能缺省；空/缺省在此归一为 api_key，随后才校验合法性。
	if t.CredentialType == "" {
		t.CredentialType = credential.TypeAPIKey
	}
	if !t.CredentialType.Valid() {
		return ErrInvalidInput
	}
	if isCodexCredentialType(t.CredentialType) && t.BaseURL != "" {
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
	seen, err := dedupFormats(t.SupportedFormats)
	if err != nil {
		return err
	}
	// 类型-格式约束（扩展）：responses-special/codex-oauth/codex-pat
	// 类型模板支持 resp / resp-ws / openai-images / openai-search 格式（images 直连
	// 为用户裁决：responses-special 与 api_key 同支持两图片端点，codex 类型
	// 走 SDK 生图；search 为用户裁决：search 端点四类型分派全可达——
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
	if t.ModelMapping == nil {
		t.ModelMapping = domain.ModelMapping{}
	}
	if err := domain.ValidateModelMapping(t.ModelMapping); err != nil {
		return ErrInvalidInput
	}
	return nil
}

func validateCacheDomain(s string) error {
	if len(s) == 0 || len(s) > 253 {
		return ErrInvalidInput
	}
	// Simple domain validation: labels 1-63, alnum/hyphen, not start/end with hyphen, dot separated, lowercased.
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return ErrInvalidInput
	}
	if s[0] == '-' || s[0] == '.' || s[len(s)-1] == '-' || s[len(s)-1] == '.' {
		return ErrInvalidInput
	}
	if len(s) != len(strings.Trim(s, ".")) {
		return ErrInvalidInput
	}
	labels := strings.Split(s, ".")
	for _, lab := range labels {
		if len(lab) == 0 || len(lab) > 63 {
			return ErrInvalidInput
		}
		if lab[0] == '-' || lab[len(lab)-1] == '-' {
			return ErrInvalidInput
		}
	}
	return nil
}

// listSortFields 各资源允许的 sort 白名单（与 repo 层白名单一致，双保险）。
var listSortFields = map[string][]string{
	"templates": {"id", "name", "base_url", "created_at", "updated_at"},
	"accounts":  {"id", "name", "template_id", "max_concurrency", "last_used_at", "created_at", "updated_at"},
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
	// 与 repo 层 priceEntrySortFields 白名单一致（双保险；/api/admin/prices）。
	"price_entries": {"model", "updated_at"},
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
	if p.BaseURL != nil && *p.BaseURL != "" {
		if err := validateBaseURL(*p.BaseURL); err != nil {
			return err
		}
	}
	var supported map[domain.RequestFormat]bool
	if p.SupportedFormats != nil {
		var err error
		supported, err = dedupFormats(*p.SupportedFormats)
		if err != nil {
			return err
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
	if p.ModelMapping != nil {
		if err := domain.ValidateModelMapping(*p.ModelMapping); err != nil {
			return ErrInvalidInput
		}
	}
	return nil
}

// validateAccountPatch 校验 patch 提供的字段（nil = 未提供，跳过）。写面的唯一
// 校验权威：handler 只做类型转换，不重复判定（重复判定只可能与此处分歧）。
func validateAccountPatch(p repository.AccountPatch) error {
	// 空补丁：无任何字段 = 无意义的空写（仍会推进 C）——拒绝而非静默接受。
	if p.Name == nil && p.TemplateID == nil && p.UpstreamKey == nil &&
		p.BaseURL == nil && p.MaxConcurrency == nil && p.GroupIDs == nil &&
		p.Enabled == nil && p.UpstreamCostMultiplierBp == nil && p.CacheDomain == nil {
		return ErrInvalidInput
	}
	if p.Name != nil && *p.Name == "" {
		return ErrInvalidInput
	}
	if p.UpstreamKey != nil && *p.UpstreamKey == "" {
		return ErrInvalidInput
	}
	// 批量 base_url 三态：空串 = 清空（合法）；非空时复用 validateBaseURL。
	if p.BaseURL != nil && *p.BaseURL != "" {
		if err := validateBaseURL(*p.BaseURL); err != nil {
			return err
		}
	}
	if p.TemplateID != nil && *p.TemplateID <= 0 {
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
	// 采购倍率下界 0（免费）、上界 ×10（100000 bp，与组倍率天花板一致）。
	if p.UpstreamCostMultiplierBp != nil && (*p.UpstreamCostMultiplierBp < 0 || *p.UpstreamCostMultiplierBp > 100000) {
		return ErrInvalidInput
	}
	if p.CacheDomain != nil && *p.CacheDomain != "" {
		if err := validateCacheDomain(*p.CacheDomain); err != nil {
			return ErrInvalidInput
		}
	}
	return nil
}

// mapRepoErr 存储错误映射：repository.ErrNotFound → ErrNotFound（保留缺失 id
// 详情，404 响应带 "id=5 missing"）；repository.ErrConflict → ErrConflict
// （保留冲突详情，409 响应带 "name=\"x\""）。其他错误原样返回。
func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrInvalidInput):
		return ErrInvalidInput
	case errors.Is(err, repository.ErrNotFound):
		detail := strings.TrimPrefix(err.Error(), repository.ErrNotFound.Error()+": ")
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	case errors.Is(err, repository.ErrConflict):
		detail := strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": ")
		return fmt.Errorf("%w: %s", ErrConflict, detail)
	}
	return err
}

func isCodexCredentialType(typ credential.Type) bool {
	return typ == credential.TypeCodexOAuth || typ == credential.TypeCodexPAT
}
