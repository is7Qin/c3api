// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package repository 用 ent 实现持久化，对外只暴露 domain 类型。
package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
)

// Repository 聚合各实体仓库（绑定单一 client + driver；WithTx 复用同一构造函数
// newRepository 构造 tx 版实例）。
type Repository struct {
	Templates     *TemplateRepo
	Accounts      *AccountRepo
	Groups        *GroupRepo
	Users         *UserRepo
	Keys          *KeyRepo
	Assignments   *GroupAssignmentRepo
	Settings      *SettingRepo
	Usages        *UsageRepo  // usage_logs 明细（消费面改名：log → usage 语义）
	ErrLogs       *ErrLogRepo // err_logs 错误审计明细（分表设计）
	Stats         *StatRepo
	Rules         RuleStore
	Redemptions   *RedemptionRepo
	Pricing       *PricingRepo
	ImagePrice    *ImagePriceRepo    // 图片生成价格（Task A 数据面；images 端点计费价格来源）
	FunctionPrice *FunctionPriceRepo // 按单元计费功能类价格（search 起；对齐 image_price 形态）
	Billing       *BillingRepo       // 扣费落库（Phase 5 T3）
	Partitions    *PartitionRepo     // 分区表 bootstrap/retention（usage_logs + err_logs + usage_stats，Phase 5 T4.5 + 用户裁决 2026-08-11）
	TemplateExts  *TemplateExtRepo   // 模板类型化扩展（template_ext 1:1；W1 数据层，消费接线 W3/W4）
	AccountExts   *AccountExtRepo    // 账号类型化鉴权扩展（account_ext 1:1；W1 数据层，消费接线 W6）
	Client        *ent.Client
	// driver 为原始 dialect.Driver：原子资源方法/条件递增等 raw SQL 走它
	//（ent v0.14 生成代码无 ExecContext/QueryContext，raw SQL 无客户端入口）；
	// WithTx 内为事务驱动（txDriver），保证 raw SQL 与 ent 构建器同连接。
	driver dialect.Driver
}

// New 用既有 driver 构建仓库（PG 生产：entsql.OpenDB(dialect.Postgres, db)；测试：
// pgxmock 适配器）。不注入 pgx 连接池——Stats.Upsert 的 COPY 批量写路径需要
// NewWithPG；未注入池时 Upsert 返回显式错误（不静默降级回 raw SQL）。
// migrate 超时由生产路径经 NewWithPG 控制（测试/工具等短生命周期路径内部用
// Background——migrate 失败即返回错误，无悬挂风险）。
func New(drv dialect.Driver, migrate bool) (*Repository, error) {
	return NewWithPG(context.Background(), drv, migrate, nil)
}

// NewWithPG 同 New，附加 pgx 连接池（Stats 聚合 SQL 直查直写 + advisory lock
// 专用连接：生产 main.go 传 OpenPG 池；池与 ent driver 同 DSN 共享连接上限
// max_conns）。
// ctx 供 migrate（ent Schema.Create）使用——生产 main 传 startupCtx
// （30s 预算，超时 fatal 文案 "db bootstrap timed out after 30s" 可归因）。
func NewWithPG(ctx context.Context, drv dialect.Driver, migrate bool, pool *pgxpool.Pool) (*Repository, error) {
	client := ent.NewClient(ent.Driver(drv))
	if migrate {
		// usage_logs/err_logs 经 migrateHookExcludesPartitioned 从迁移列表过滤
		// ——分区表 DDL 由 Partitions.EnsureUsageLogPartitioned/
		// EnsureErrLogPartitioned 独占管理（atlas 对分区表 diff 规划期必失败，
		// 真实 PG 实测结论见 partition.go）。
		if err := client.Schema.Create(ctx, migrateHookExcludesPartitioned()); err != nil {
			return nil, err
		}
	}
	return newRepository(client, drv, pool), nil
}

// newRepository 用给定 client/driver 构建全量仓库（New/NewWithPG/WithTx 复用
// 同一构造函数；WithTx 注入 tx client + 事务驱动，fn 内所有方法调用都走 tx ——
// 评审 I-1）。pool 进 Stats（离线聚合 SQL 直查直写自 Acquire 独立连接，不进
// 事务；advisory lock 专用连接持锁整周期——池连接复用即丢锁）与 Billing（热点
// 修复 A 扩：DeductAndLog pgx 直连 + COPY 路径；WithTx 传 nil → 事务内回落
// ent 路径，见 billing_repo.go）。
func newRepository(client *ent.Client, drv dialect.Driver, pool *pgxpool.Pool) *Repository {
	accounts := &AccountRepo{client: client}
	return &Repository{
		Templates:     &TemplateRepo{client: client},
		Accounts:      accounts,
		Groups:        &GroupRepo{client: client, accounts: accounts, driver: drv},
		Users:         &UserRepo{client: client, driver: drv},
		Keys:          &KeyRepo{client: client, driver: drv},
		Assignments:   &GroupAssignmentRepo{client: client},
		Settings:      &SettingRepo{client: client},
		Usages:        &UsageRepo{client: client},
		ErrLogs:       &ErrLogRepo{client: client},
		Stats:         &StatRepo{client: client, pool: pool},
		Rules:         &RuleRepo{client: client},
		Redemptions:   &RedemptionRepo{client: client, driver: drv},
		Pricing:       &PricingRepo{client: client, driver: drv},
		ImagePrice:    &ImagePriceRepo{client: client, driver: drv},
		FunctionPrice: &FunctionPriceRepo{client: client, driver: drv},
		Billing:       &BillingRepo{client: client, driver: drv, pool: pool},
		Partitions:    &PartitionRepo{driver: drv},
		TemplateExts:  &TemplateExtRepo{client: client},
		AccountExts:   &AccountExtRepo{client: client},
		Client:        client,
		driver:        drv,
	}
}

// --- 事务（评审 I-1 核心） ---

// txDriver 把 dialect.Tx 包装成 dialect.Driver（镜像 ent 内部 txDriver 语义）：
// ent 构建器与 raw SQL（原子资源方法/IncrementUsed）经同一驱动 → 同一事务连接。
// Commit/Rollback 为 nop，由 WithTx 直接对 dialect.Tx 提交/回滚。
type txDriver struct {
	drv dialect.Driver
	tx  dialect.Tx
}

func (d *txDriver) Dialect() string                        { return d.drv.Dialect() }
func (d *txDriver) Close() error                           { return nil }
func (d *txDriver) Tx(context.Context) (dialect.Tx, error) { return d, nil }
func (d *txDriver) Commit() error                          { return nil }
func (d *txDriver) Rollback() error                        { return nil }
func (d *txDriver) Exec(ctx context.Context, query string, args, v any) error {
	return d.tx.Exec(ctx, query, args, v)
}
func (d *txDriver) Query(ctx context.Context, query string, args, v any) error {
	return d.tx.Query(ctx, query, args, v)
}

// TxStore WithTx 事务回调面（评审 I-1）：兑换编排/测试在单事务内仅经此面访问
// 资源更新与兑换数据。*Repository 实现（WithTx 注入 tx 版实例，全部走同一事务
// 连接）；service 层 fake 实现同面做事务语义模拟（评审 I-1 回滚断言的前提——
// 回调参数因此用接口而非 *Repository，同时约束 applier 无法绕过 tx 面）。
// 面内任一步失败 → 整体回滚。
type TxStore interface {
	CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error
	GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error)
	GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error)
	UpdateUserBalance(ctx context.Context, userID, delta int64) error
	UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error
	CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error
	CreateUse(ctx context.Context, use *domain.RedemptionUse) error
	IncrementUsed(ctx context.Context, codeID int64) (bool, error)
}

// WithTx 在单事务内执行 fn（评审 I-1）：ent `Tx().Client()` 模式构造 tx 版 Repository
// （复用 newRepository，注入 tx client + 事务驱动），fn 内所有方法调用（含原子资源
// 方法）都走 tx；fn 返回错误 → 整体回滚，nil → Commit。兑换编排（Task 2）用：
// applier 必须只经 tx 面调资源更新，任一步失败（含 use 冲突/计数用尽）全部回滚。
func (r *Repository) WithTx(ctx context.Context, fn func(TxStore) error) error {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	drv := &txDriver{tx: tx, drv: r.driver}
	tr := newRepository(ent.NewClient(ent.Driver(drv)), drv, nil) // 事务内不挂 pgx 池：Upsert COPY 自 Acquire 独立连接，不进 tx 面
	if err := fn(tr); err != nil {
		return err
	}
	return tx.Commit()
}

// execUpdate 执行 UPDATE 构建器，返回受影响行数（raw SQL 统一入口；
// dialect.Driver.Exec 的 v 形参为 *sql.Result，见 pgxmock 测试适配器同款）。
// 注意：裸 sql.Update 构建器无方言信息（默认反引号引号），执行前按驱动方言设置
// （PG → 双引号），否则 Postgres 报语法错误。
func execUpdate(ctx context.Context, drv dialect.Driver, u *entsql.UpdateBuilder) (int64, error) {
	u.SetDialect(drv.Dialect())
	query, args := u.Query()
	var res sql.Result
	if err := drv.Exec(ctx, query, args, &res); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Store 门面：Repository 聚合全部实体仓库，实现 service.Store（装配入口用）。 ---

func (r *Repository) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.CreateTemplate(ctx, t)
}

func (r *Repository) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	return r.Templates.GetTemplate(ctx, id)
}

func (r *Repository) GetTemplatesByIDs(ctx context.Context, ids []int64) ([]*domain.Template, error) {
	return r.Templates.GetTemplatesByIDs(ctx, ids)
}

func (r *Repository) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	return r.Templates.ListTemplates(ctx, q)
}

func (r *Repository) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	return r.Templates.UpdateTemplate(ctx, t)
}

func (r *Repository) DeleteTemplate(ctx context.Context, id int64) error {
	return r.Templates.DeleteTemplate(ctx, id)
}

func (r *Repository) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	return r.Templates.DeleteTemplatesBatch(ctx, ids)
}

func (r *Repository) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	return r.Templates.UpdateTemplatesBatch(ctx, ids, p)
}

func (r *Repository) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.CreateAccount(ctx, a)
}

func (r *Repository) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	return r.Accounts.GetAccount(ctx, id)
}

func (r *Repository) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	return r.Accounts.ListAccounts(ctx, q)
}

func (r *Repository) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	return r.Accounts.UpdateAccount(ctx, a)
}

func (r *Repository) DeleteAccount(ctx context.Context, id int64) error {
	return r.Accounts.DeleteAccount(ctx, id)
}

func (r *Repository) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	return r.Accounts.DeleteAccountsBatch(ctx, ids)
}

func (r *Repository) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) error {
	return r.Accounts.UpdateAccountsBatch(ctx, ids, p)
}

func (r *Repository) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	return r.Accounts.SetAccountGroups(ctx, accountID, groupIDs)
}

func (r *Repository) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	return r.Accounts.GetAccountGroups(ctx, accountID)
}

func (r *Repository) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.CreateGroup(ctx, g)
}

func (r *Repository) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	return r.Groups.GetGroup(ctx, id)
}

func (r *Repository) ListGroups(ctx context.Context, q ListQuery) ([]*domain.Group, int64, error) {
	return r.Groups.ListGroups(ctx, q)
}

func (r *Repository) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	return r.Groups.UpdateGroup(ctx, g)
}

func (r *Repository) DeleteGroup(ctx context.Context, id int64) error {
	return r.Groups.DeleteGroup(ctx, id)
}

func (r *Repository) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	return r.Groups.DeleteGroupsBatch(ctx, ids)
}

func (r *Repository) UpdateGroupsBatch(ctx context.Context, ids []int64, p GroupPatch) error {
	return r.Groups.UpdateGroupsBatch(ctx, ids, p)
}

// --- 模板/账号类型化扩展（W1 数据层；消费接线 W3/W4/W6） ---

func (r *Repository) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	return r.TemplateExts.UpsertTemplateExt(ctx, e)
}

func (r *Repository) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	return r.TemplateExts.GetTemplateExt(ctx, templateID)
}

func (r *Repository) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	return r.AccountExts.UpsertAccountExt(ctx, e)
}

func (r *Repository) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	return r.AccountExts.TryInsertAccountExt(ctx, e)
}

func (r *Repository) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	return r.AccountExts.GetAccountExt(ctx, accountID)
}

// --- 用户（Phase 3a） ---

func (r *Repository) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	return r.Users.CreateUser(ctx, u)
}

func (r *Repository) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	return r.Users.GetUser(ctx, id)
}

func (r *Repository) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return r.Users.GetUserByEmail(ctx, email)
}

// CountUsers 用户总数（注册 bootstrap：表空 = 首个注册 = platform_admin）。
func (r *Repository) CountUsers(ctx context.Context) (int64, error) {
	return r.Users.CountUsers(ctx)
}

func (r *Repository) ListUsers(ctx context.Context, q ListQuery) ([]*domain.User, int64, error) {
	return r.Users.ListUsers(ctx, q)
}

func (r *Repository) UpdateUser(ctx context.Context, p *UserPatch) (*domain.User, error) {
	return r.Users.UpdateUser(ctx, p)
}

func (r *Repository) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	return r.Users.UpdateUserPassword(ctx, id, passwordHash)
}

// CreateTempBalance 临时额度行（注册赠品；UserRepo 扩展方法，不新增独立
// repo 字段——避免与并行任务的门面改动冲突）。
func (r *Repository) CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error {
	return r.Users.CreateTempBalance(ctx, userID, amount, expiresAt, note)
}

// ListUserTempBalances 用户侧有效临时额度（UserRepo 扩展方法）。
func (r *Repository) ListUserTempBalances(ctx context.Context, userID int64) ([]*domain.TempBalance, error) {
	return r.Users.ListUserTempBalances(ctx, userID)
}

// ListTempBalances 管理侧临时额度全量列表（UserRepo 扩展方法）。
func (r *Repository) ListTempBalances(ctx context.Context, q ListQuery, userID int64) ([]*domain.TempBalance, int64, error) {
	return r.Users.ListTempBalances(ctx, q, userID)
}

// ListUserEmails 批量取邮箱（/admin/users-top TopN 回填；id IN 一次查询）。
func (r *Repository) ListUserEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	return r.Users.ListUserEmails(ctx, ids)
}

// --- 客户端 key（Phase 3a） ---

func (r *Repository) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.CreateKey(ctx, k)
}

func (r *Repository) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	return r.Keys.GetKey(ctx, id)
}

func (r *Repository) GetKeyByRaw(ctx context.Context, raw string) (*domain.Key, error) {
	return r.Keys.GetKeyByRaw(ctx, raw)
}

func (r *Repository) ListKeysByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.Key, int64, error) {
	return r.Keys.ListKeysByUser(ctx, userID, q)
}

func (r *Repository) UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	return r.Keys.UpdateKey(ctx, k)
}

func (r *Repository) RotateKey(ctx context.Context, id int64, newRaw string) (*domain.Key, error) {
	return r.Keys.RotateKey(ctx, id, newRaw)
}

func (r *Repository) DeleteKey(ctx context.Context, id int64) error {
	return r.Keys.DeleteKey(ctx, id)
}

func (r *Repository) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	return r.Keys.DeleteKeysByGroup(ctx, groupID)
}

// --- 组授予（Phase 3a） ---

func (r *Repository) GrantGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Grant(ctx, groupID, userID)
}

// SetAssignmentMultiplier 设置/清除该用户在该组的专属价格倍率（T3.5 修正：
// 按组；m = nil → 清除为未设置 → 回退组倍率）。
func (r *Repository) SetAssignmentMultiplier(ctx context.Context, groupID, userID int64, m *int) error {
	return r.Assignments.SetMultiplier(ctx, groupID, userID, m)
}

func (r *Repository) RevokeGroup(ctx context.Context, groupID, userID int64) error {
	return r.Assignments.Revoke(ctx, groupID, userID)
}

func (r *Repository) ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByUser(ctx, userID)
}

func (r *Repository) ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	return r.Assignments.ListByGroup(ctx, groupID)
}

func (r *Repository) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return r.Assignments.ListGroupsForUser(ctx, userID)
}

// --- settings（Phase 3a） ---

func (r *Repository) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	return r.Settings.Get(ctx, key)
}

func (r *Repository) GetAllSettings(ctx context.Context) ([]*domain.Setting, error) {
	return r.Settings.GetAll(ctx)
}

func (r *Repository) SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	return r.Settings.Set(ctx, key, typ, value)
}

func (r *Repository) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	return r.Rules.ListRules(ctx, enabled)
}

func (r *Repository) CreateRule(ctx context.Context, rl domain.Rule) (int64, error) {
	return r.Rules.CreateRule(ctx, rl)
}

func (r *Repository) UpdateRule(ctx context.Context, rl domain.Rule) error {
	return r.Rules.UpdateRule(ctx, rl)
}

func (r *Repository) DeleteRule(ctx context.Context, id int64) error {
	return r.Rules.DeleteRule(ctx, id)
}

func (r *Repository) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	return r.Rules.DeleteRulesBatch(ctx, ids)
}

func (r *Repository) CountRules(ctx context.Context) (int64, error) {
	return r.Rules.CountRules(ctx)
}

func (r *Repository) QueryUsages(ctx context.Context, q UsageQuery) ([]*domain.UsageLog, error) {
	return r.Usages.QueryUsages(ctx, q)
}

// InsertErrLogBatch 批量插入错误明细（err_logs；errlog worker 写路径）。
func (r *Repository) InsertErrLogBatch(ctx context.Context, logs []*domain.UsageLog) error {
	return r.ErrLogs.InsertBatch(ctx, logs)
}

// QueryErrLogs err_logs 错误明细分页查询（/err_logs API）。
func (r *Repository) QueryErrLogs(ctx context.Context, q ErrLogQuery) ([]*domain.UsageLog, error) {
	return r.ErrLogs.QueryErrLogs(ctx, q)
}

func (r *Repository) ScanStats(ctx context.Context, q StatQuery) ([]*domain.StatBucket, error) {
	return r.Stats.ScanStats(ctx, q)
}

// --- /admin/overview 聚合面（spec 2026-08-14；SQL 侧聚合 + 冷面计数） ---

func (r *Repository) SummarizeStats(ctx context.Context, from, to time.Time, groupID int64) (*StatSummary, error) {
	return r.Stats.SummarizeStats(ctx, from, to, groupID)
}

func (r *Repository) ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64) ([]*StatDayAgg, error) {
	return r.Stats.ScanStatsDays(ctx, from, to, groupID)
}

func (r *Repository) CountOverviewResources(ctx context.Context) (*OverviewResourceCounts, error) {
	return r.Stats.CountOverviewResources(ctx)
}

// --- 兑换码（Phase 5 计费前基础设施） ---

func (r *Repository) CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	return r.Redemptions.CreateCodes(ctx, codes)
}

func (r *Repository) GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error) {
	return r.Redemptions.GetByCode(ctx, code)
}

func (r *Repository) GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error) {
	return r.Redemptions.GetCode(ctx, id)
}

func (r *Repository) ListCodes(ctx context.Context, q ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error) {
	return r.Redemptions.ListCodes(ctx, q, typ, status)
}

func (r *Repository) ListCodeUses(ctx context.Context, codeID int64, q ListQuery) ([]*domain.RedemptionUse, int64, error) {
	return r.Redemptions.ListCodeUses(ctx, codeID, q)
}

func (r *Repository) ListUsesByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.RedemptionRecord, int64, error) {
	return r.Redemptions.ListUsesByUser(ctx, userID, q)
}

func (r *Repository) GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error) {
	return r.Redemptions.GetUse(ctx, codeID, userID)
}

func (r *Repository) CreateUse(ctx context.Context, use *domain.RedemptionUse) error {
	return r.Redemptions.CreateUse(ctx, use)
}

func (r *Repository) IncrementUsed(ctx context.Context, codeID int64) (bool, error) {
	return r.Redemptions.IncrementUsed(ctx, codeID)
}

func (r *Repository) DeactivateCodes(ctx context.Context, ids []int64) (int64, error) {
	return r.Redemptions.DeactivateCodes(ctx, ids)
}

// --- 模型价格（Phase 5 计费价格来源） ---

func (r *Repository) UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error) {
	return r.Pricing.UpsertFromLiteLLM(ctx, rows)
}

func (r *Repository) UpsertManual(ctx context.Context, m *PricingManual) (*domain.Pricing, error) {
	return r.Pricing.UpsertManual(ctx, m)
}

func (r *Repository) DeleteManual(ctx context.Context, model string) error {
	return r.Pricing.DeleteManual(ctx, model)
}

func (r *Repository) ListPricing(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error) {
	return r.Pricing.ListPricing(ctx, q, source, provider, model)
}

func (r *Repository) GetPricing(ctx context.Context, model string) (*domain.Pricing, error) {
	return r.Pricing.GetPricing(ctx, model)
}

// --- 图片生成价格（Task A 数据面；机制与 pricings 同款） ---

func (r *Repository) UpsertImageFromLiteLLM(ctx context.Context, rows []*domain.ImagePrice) (int, error) {
	return r.ImagePrice.UpsertFromLiteLLM(ctx, rows)
}

func (r *Repository) UpsertImageManual(ctx context.Context, m *ImagePriceManual) (*domain.ImagePrice, error) {
	return r.ImagePrice.UpsertManual(ctx, m)
}

func (r *Repository) DeleteImageManual(ctx context.Context, model string) error {
	return r.ImagePrice.DeleteManual(ctx, model)
}

func (r *Repository) ListImagePrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error) {
	return r.ImagePrice.ListImagePrice(ctx, q, source, provider, model)
}

func (r *Repository) GetImagePrice(ctx context.Context, model string) (*domain.ImagePrice, error) {
	return r.ImagePrice.GetImagePrice(ctx, model)
}

// --- 按单元计费功能类价格（Task 价格表三件套；机制与 pricings/image_price 同款） ---

func (r *Repository) UpsertFunctionFromLiteLLM(ctx context.Context, rows []*domain.FunctionPrice) (int, error) {
	return r.FunctionPrice.UpsertFromLiteLLM(ctx, rows)
}

func (r *Repository) UpsertFunctionManual(ctx context.Context, m *FunctionPriceManual) (*domain.FunctionPrice, error) {
	return r.FunctionPrice.UpsertManual(ctx, m)
}

func (r *Repository) DeleteFunctionManual(ctx context.Context, model string) error {
	return r.FunctionPrice.DeleteManual(ctx, model)
}

func (r *Repository) ListFunctionPrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error) {
	return r.FunctionPrice.ListFunctionPrice(ctx, q, source, provider, model)
}

func (r *Repository) GetFunctionPrice(ctx context.Context, model string) (*domain.FunctionPrice, error) {
	return r.FunctionPrice.GetFunctionPrice(ctx, model)
}

// EnsureFunctionPriceSeed 幂等插入 codex-search 初始化行（bootstrap 钩子；main
// 启动期 ent migrate 之后调用，失败即 fatal——默认按次价不可缺）。ON CONFLICT
// DO NOTHING：已存在（含管理端改价后的行）恒不覆盖。
func (r *Repository) EnsureFunctionPriceSeed(ctx context.Context) error {
	return r.FunctionPrice.EnsureCodexSearchSeed(ctx)
}

// --- 原子资源更新（评审 I-1：UserStore 扩展；普通 client 与 tx client 均可用） ---

func (r *Repository) UpdateUserBalance(ctx context.Context, userID, delta int64) error {
	return r.Users.UpdateUserBalance(ctx, userID, delta)
}

// DeductAndLog 批量扣费 + 计费日志落库（Phase 5 T3 计费 flusher 写路径）：
// FEFO 临时额度优先 + 条件扣费（允许透支）+ 同事务批量日志，见 BillingRepo。
func (r *Repository) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (overdrafted bool, balanceAfter int64, err error) {
	return r.Billing.DeductAndLog(ctx, userID, cost, logs)
}

// --- usagelog 按日分区（Phase 5 T4.5；main 装配 bootstrap + retention worker） ---

// EnsureUsageLogPartitioned 分区 bootstrap（幂等）：未分区 → DROP 重建分区表
// + 预建当日/明日分区 + 索引；已分区 → 仅补齐分区。
func (r *Repository) EnsureUsageLogPartitioned(ctx context.Context, now time.Time) error {
	return r.Partitions.EnsureUsageLogPartitioned(ctx, now)
}

// EnsureUsageLogPartitions 预建 [trunc(now), trunc(until)] 每日分区（retention
// worker 防日界竞态；start 边界由传入 now 推导，幂等）。
func (r *Repository) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.Partitions.EnsureUsageLogPartitions(ctx, now, until)
}

// DropUsageLogPartitionsBefore DROP 分区下界 < cutoff 的分区（O(1)；返回删除
// 个数）。retention worker 按 LogRetentionDays 调。
func (r *Repository) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.Partitions.DropUsageLogPartitionsBefore(ctx, cutoff)
}

// EnsureErrLogPartitioned err_logs 分区 bootstrap（幂等；main 装配在 ent
// migrate 之后调用，与 usage_logs 同路线）。
func (r *Repository) EnsureErrLogPartitioned(ctx context.Context, now time.Time) error {
	return r.Partitions.EnsureErrLogPartitioned(ctx, now)
}

// EnsureErrLogPartitions err_logs 预建 [trunc(now), trunc(until)] 每日分区
// （retention worker 防日界竞态；幂等）。
func (r *Repository) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	return r.Partitions.EnsureErrLogPartitions(ctx, now, until)
}

// DropErrLogPartitionsBefore err_logs DROP 分区下界 < cutoff 的分区（O(1)；
// 独立保留期——retention worker 按 ErrLogRetentionDays 调）。
func (r *Repository) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.Partitions.DropErrLogPartitionsBefore(ctx, cutoff)
}

// EnsureUsageStatsPartitioned usage_stats 分区 bootstrap（幂等；main 装配在
// ent migrate 之后调用——migrate 经钩子跳过分区表，三表同路线）。
func (r *Repository) EnsureUsageStatsPartitioned(ctx context.Context, now time.Time) error {
	return r.Partitions.EnsureUsageStatsPartitioned(ctx, now)
}

// EnsureUsageStatsPartitions usage_stats 预建 [trunc(now), trunc(until)] 每日
// 分区（retention worker 防日界竞态；分区键 bucket_time，幂等）。
func (r *Repository) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	return r.Partitions.EnsureUsageStatsPartitions(ctx, now, until)
}

// DropUsageStatsPartitionsBefore usage_stats DROP 分区下界 < cutoff 的分区
// （O(1)；用户裁决 2026-08-11：PG DELETE 不释放空间，180 天保留清理必须分区
// DROP——retention worker 按 StatsRetentionDays 调）。
func (r *Repository) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.Partitions.DropUsageStatsPartitionsBefore(ctx, cutoff)
}

// DeleteRedemptionUsesBefore redemption_uses 有界批删（F3-2：普通表无分区可
// DROP，每轮至多删 5000 行；TTL 定死 90 天——retention worker 每轮按
// redemptionUseRetentionDays 调）。
func (r *Repository) DeleteRedemptionUsesBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return r.Partitions.DeleteRedemptionUsesBefore(ctx, cutoff)
}

// LoadBalances 全量余额快照（Phase 5 计费余额预检数据源）。
func (r *Repository) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	return r.Users.LoadBalances(ctx)
}

// LoadGroupMultipliers 全量组倍率快照（Phase 5 T3.5 价格倍率数据源）。
func (r *Repository) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	return r.Groups.LoadGroupMultipliers(ctx)
}

// LoadAssignmentMultipliers 全量用户-组专属倍率快照（T3.5 修正：用户专属倍率
// 按组挂载——billing.Balances.Reload/ReloadMultipliers 数据源）。
func (r *Repository) LoadAssignmentMultipliers(ctx context.Context) (map[billing.AssignmentKey]int, error) {
	return r.Groups.LoadAssignmentMultipliers(ctx)
}

func (r *Repository) UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error {
	return r.Users.UpdateUserMaxConcurrency(ctx, userID, value)
}

// poolTimeoutParams 计费路径防卡死参数（F-P2-4，P2，spec 2026-08-13）：
//   - lock_timeout=5s：锁等待上限——管理员 pg_dump/长事务持锁期间一条 FEFO
//     UPDATE 曾无限卡锁等待（PG 默认 lock_timeout=0）→ 8 个 flush worker 全
//     阻塞 → flushMu 永持 → 全局计费停摆 + pending 内存无界；5s 后该语句报错
//     （SQLSTATE 55P03），失败 chunk 回灌下轮重试（不丢不重），flushMu 释放。
//
// 明确不含 statement_timeout（评审 P3-A 扩面实测降级，spec 授权"若冲突 → 降级
// 为计费路径 per-query 超时并注明"）：会话级 statement_timeout=10s 与 admin 面
// ScanStats 大窗口聚合冲突——720 万行/30 天窗口实测 >10s 被 57014 杀死（从"慢
// 返回"变"超时错误"，用户面失败语义更糟；DB 侧聚合实测仅 ~1-2s，超时主因是
// 7.2M 行客户端 ent 解码，管理面 DB 端 GROUP BY 优化留 F-P2-2 单独评估）。降级
// 形态：计费路径 per-query 10s 超时由 BillingRepo.DeductAndLog 的 ctx 截止
// 实现（deductTimeout），执行时长与锁等待双有界；其余路径维持现状语义（慢
// 返回，无回归）。全部实测数字见 docs/superpowers/reports/f1-impl-report.md。
//
// 形态：pgx DSN query 参数 → 连接启动包 → 会话级 GUC（lock_timeout 为 USERSET
// context，可启动期设置），作用于本池全部连接（含 ent 路径）。SELECT 路径不
// 取行锁不受 lock_timeout 影响（快照全量扫描/ScanStats//user/stats 各面实测
// ms 级，见实现报告）。
var poolTimeoutParams = []string{"lock_timeout=5s"}

// appendPoolTimeouts 用户 DSN 未显式含同名参数时统一追加（OpenPG 统一补丁
// 形态——不依赖各 config.toml 手工写；用户已显式配置该参数 → 尊重用户配置，
// 不覆盖）。分隔符按 DSN 形态裁定：URL 形态（postgres://...）无 query 参数 →
// "?"，已有（如 ?sslmode=disable）→ "&"；keyword/value 形态（host=... dbname=...）
// → 空格。追加第一参后形态变化（出现 ?），后续参数自动切 "&"。
func appendPoolTimeouts(dsn string) string {
	key := func(kv string) string { return kv[:strings.IndexByte(kv, '=')+1] }
	missing := 0
	for _, kv := range poolTimeoutParams {
		if !strings.Contains(dsn, key(kv)) {
			missing++
		}
	}
	if missing == 0 {
		return dsn
	}
	var b strings.Builder
	b.Grow(len(dsn) + 48)
	b.WriteString(dsn)
	for _, kv := range poolTimeoutParams {
		if strings.Contains(dsn, key(kv)) {
			continue // 用户已显式配置该参数 → 不覆盖
		}
		switch {
		case strings.Contains(b.String(), "?"):
			b.WriteString("&") // URL 形态且已有 query 参数（?sslmode=disable 等）
		case strings.Contains(dsn, "://"):
			b.WriteString("?") // URL 形态无 query 参数
		default:
			b.WriteString(" ") // keyword/value 形态（host=... dbname=...）
		}
		b.WriteString(kv)
	}
	return b.String()
}

// OpenPG 打开 pgx 连接池（生产入口；ent driver 由调用方用
// entsql.OpenDB(dialect.Postgres, stdlib.OpenDBFromPool(pool)) 构建）。
//
// 计费路径防卡死（F-P2-4）：OpenPG 统一给 DSN 补 lock_timeout=5s（用户 DSN
// 未含时，appendPoolTimeouts）+ MaxConnLifetime=30m 滚动轮换。statement_timeout
// 因全局副作用实测冲突不设会话级（见 poolTimeoutParams 注释）——计费路径执行
// 时长由 BillingRepo.DeductAndLog 的 per-query 10s ctx 超时兜底。
func OpenPG(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(appendPoolTimeouts(dsn))
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	// MaxConnLifetime 30m 滚动轮换（用户裁决 2026-08-13 实施）：每条连接到期后
	// 按需重建（非全量轮换——PG 侧连接数峰值不变），防网络抖动后长连接脏状态；
	// HealthCheckPeriod 30s 健康检查保留为双保险（不设）。
	cfg.MaxConnLifetime = 30 * time.Minute
	return pgxpool.NewWithConfig(ctx, cfg)
}
