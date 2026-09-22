// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/accountext"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/groupassignment"
)

// GroupRepo 承担调度器 Loader 的快照数据源（LoadGroupsAccounts/LoadGroupAccounts）。
type GroupRepo struct {
	client *ent.Client
	// driver 为成员关系全表扫描用（LoadGroupsAccounts；与 user_repo 同构——
	// 普通 client 与 tx client 均可用）。
	driver dialect.Driver
}

// accountGroupsMembershipSQL 全量成员关系（account_id, group_id）扫描。
// 零 IN 参数——见 LoadGroupsAccounts 注释（ent m2m 跳查询的 IN 上限问题）。
const accountGroupsMembershipSQL = `SELECT account_id, group_id FROM account_groups`

func (r *GroupRepo) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	// price_multiplier 恒写入（service 层把缺省归一为 10000 = ×1：
	// API 边界 nullable float64 可表达显式 0 = 免费组，repo 不再把 0 当"未指定"
	// 跳过落列）。DB 默认 10000 为兜底。
	q := r.client.Group.Create().
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		SetPriceMultiplier(g.PriceMultiplier).
		// protocol_convert 恒写入（service 层把缺省归一为空数组；JSON 列自动
		// 序列化 []string，空数组 = off = 不转换）。
		SetProtocolConvert(protocolConvertStrings(g.ProtocolConverts))
	row, err := q.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
		return nil, err
	}
	return toDomainGroup(row), nil
}

func (r *GroupRepo) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	row, err := r.client.Group.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainGroup(row), nil
}

func (r *GroupRepo) ListGroups(ctx context.Context, q ListQuery) ([]*domain.Group, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Group.Query().Where(group.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(group.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(groupSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroup(row))
	}
	return out, int64(total), nil
}

func (r *GroupRepo) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	// price_multiplier 恒写入（PUT 全量替换语义；管理面 PUT 读改写——fetch →
	// 改 name/visibility → 写回，未触及倍率时携带原值自然保留；显式 0 = 免费组）。
	row, err := r.client.Group.UpdateOneID(g.ID).
		SetName(g.Name).
		SetVisibility(group.Visibility(g.Visibility)).
		SetPriceMultiplier(g.PriceMultiplier).
		SetProtocolConvert(protocolConvertStrings(g.ProtocolConverts)).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, g.Name)
		}
		return nil, err
	}
	return toDomainGroup(row), nil
}

// DeleteGroup 软删除：deleted_at 置值（行保留留审计；调度器快照按
// deleted_at IS NULL 过滤，GET 单个仍可查已删项）。bulk Update（无 re-SELECT）
// 单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *GroupRepo) DeleteGroup(ctx context.Context, id int64) error {
	n, err := r.client.Group.Update().Where(group.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// LoadGroupsAccounts 全量组→账号快照（调度器启动/定时/全量失效的数据源）。
//
// 崩溃修复（实证：847k 组 → 启动 fatal / 运行中静默空结果）：ent
// WithAccounts eager-load 对 m2m 边生成两跳参数化 IN——
//  1. `SELECT account_id, group_id FROM account_groups WHERE group_id IN (全部组 id)`
//  2. `SELECT * FROM accounts WHERE id IN (跳1的账号 id)`
//
// 跳1 参数数 = 组实体数，超过 PG 上限 65535（错误 54001 "too many parameters"）
// 即崩溃；且 ent 邻接跳恒为参数化 IN（无法经分片控制），故本方法弃用 eager-load，
// 改为**全表扫描 + 内存 join**（任务决策：语义允许时改 JOIN）：
//  1. `Account.Query().WithTemplate().All`——账号全表扫描；模板 IN 参数数受
//     模板表实体数约束（管理面小表 压测仅 6 个），非账号规模驱动。
//     模板侧嵌套 WithExt（template_ext 1:1 边缘表）——快照合并
//     StripImageTools 用；ext 的 IN 参数数同为模板实体数约束（同一小表界）。
//  2. `Group.Query().IDs`——组 id 全表扫描（零参数；为无账号组保留空条目——
//     与旧 eager-load 语义一致，调度器 Select 区分"组不存在"与"组无账号"）。
//  3. `SELECT account_id, group_id FROM account_groups`——成员关系全表扫描，
//     零参数。
//  4. `SELECT * FROM account_exts`——账号 ext 全表扫描 + 内存 join（此后
//     codex 路由按 Account.Ext 派生 AccountCredential）。不 eager-load 的原因
//     同成员关系：ext 的 FK 是 account_id，eager-load 生成 `WHERE account_id
//     IN (全部账号 id)`——参数数 = 账号实体数，>65,535 触顶；全表扫描零参数，
//     与账号全表扫描同一量级带宽。
//
// 四条语句参数数都与实体数量无关，任何规模（组/账号 >65,535）都不会触顶。
// 结果语义与旧 eager-load 一致：所有组都在 map 中（无账号组为空切片），
// 账号只出现在其所属组且带模板。（唯一窗口差异：组 id 扫描后被并发删除的
// 组不会出现在结果中——见下方白名单守卫；旧 eager-load 同窗口行为。）
func (r *GroupRepo) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	// 软删除：已删 account/group 不进调度器快照（成员关系白名单守卫同语义）。
	// 模板侧嵌套 WithExt：快照合并 StripImageTools（；ext IN 参数数受模板
	// 表实体数约束——同一小表界，见上方注释）；账号侧 ext 不 eager-load
	// （FK=account_id 的 IN 参数数受账号规模驱动——触顶约束，见步骤 4）。
	accs, err := r.client.Account.Query().Where(account.DeletedAtIsNil()).
		WithTemplate(func(q *ent.TemplateQuery) { q.WithExt() }).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups-accounts (accounts scan): %w", err)
	}
	exts, err := r.client.AccountExt.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups-accounts (account_ext scan): %w", err)
	}
	extByAccount := make(map[int64]*ent.AccountExt, len(exts))
	for _, e := range exts {
		extByAccount[e.AccountID] = e
	}
	byID := make(map[int64]*domain.Account, len(accs))
	for _, a := range accs {
		if e := extByAccount[a.ID]; e != nil {
			a.Edges.Ext = []*ent.AccountExt{e}
		}
		byID[a.ID] = toDomainAccount(a)
	}
	gids, err := r.client.Group.Query().Where(group.DeletedAtIsNil()).IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups-accounts (groups scan): %w", err)
	}
	out := make(map[int64][]*domain.Account, len(gids))
	for _, gid := range gids {
		out[gid] = nil // 无账号组保留空条目（与旧 eager-load 同语义）
	}
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, accountGroupsMembershipSQL, []any{}, rows); err != nil {
		return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, groupID int64
		if err := rows.Scan(&accountID, &groupID); err != nil {
			return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
		}
		a, ok := byID[accountID]
		if !ok {
			continue // 成员关系引用已删账号：忽略（与 eager-load 同语义）
		}
		if _, ok := out[groupID]; !ok {
			// 白名单守卫：组 id 扫描后被并发删除的组不留幽灵条目——
			// 否则 buildSnapshots 会把幽灵组建成真实组，且其账号的共享
			// 实例可能以幽灵组为 gid（与 eager-load 同语义：邻接查询按
			// 已取组 id 过滤，未知父组被 ent 丢弃）。
			continue
		}
		out[groupID] = append(out[groupID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load groups-accounts (membership scan): %w", err)
	}
	return out, nil
}

// compileStalenessSQL is the single §9-A1 read-only aggregate pass: one
// statement, nine scalar subqueries, no new columns/tables/keys/indexes
// (schema frozen). Soft deletes mirror the snapshot inclusion (deleted_at IS
// NULL); membership/ext rows are count-only. Known limit: an exact membership
// swap (remove one, add one — same COUNT) is probe-invisible; membership moves
// always ride NOTIFY → InvalidateGroup in-app, and disconnect gaps ride
// reconnect FullRefresh, so the probe stays a backstop, never the primary
// carrier. Freshness maxima ride
// MAX(updated_at) on exactly the three tables whose ent schemas maintain it
// (account.go:36, group.go:29, template.go:26 — UpdateDefault(time.Now), so
// every ent write path bumps it, including the writes that change account
// content WITHOUT bumping lifecycle_revision (the runtime failure verb, and
// out-of-band direct-DB edits). The other
// two tables have no maintained timestamp — verified, not assumed: account_groups
// is an ent m2m edge table (no schema file, no time columns; edge rows are
// immutable (account_id, group_id) pairs — insert/delete only, so COUNT is
// complete and every move additionally rides NOTIFY → InvalidateGroup in-app);
// account_exts carries no time columns at all (account_ext.go:23-44) and every
// service ext write co-bumps the parent account's lifecycle_revision in the
// same transaction (ext_codex_repo.go), so ext content edits are rev-covered
// and COUNT covers ext insert/delete. COALESCE pins empty tables to epoch
// (never NULL — the tuple must compare exact).
const compileStalenessSQL = `SELECT` +
	` (SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL),` +
	` (SELECT COALESCE(MAX(lifecycle_revision), 0) FROM accounts WHERE deleted_at IS NULL),` +
	` (SELECT COALESCE(MAX(updated_at), 'epoch'::timestamptz) FROM accounts WHERE deleted_at IS NULL),` +
	` (SELECT COUNT(*) FROM groups WHERE deleted_at IS NULL),` +
	` (SELECT COALESCE(MAX(updated_at), 'epoch'::timestamptz) FROM groups WHERE deleted_at IS NULL),` +
	` (SELECT COUNT(*) FROM templates WHERE deleted_at IS NULL),` +
	` (SELECT COALESCE(MAX(updated_at), 'epoch'::timestamptz) FROM templates WHERE deleted_at IS NULL),` +
	` (SELECT COUNT(*) FROM account_groups),` +
	` (SELECT COUNT(*) FROM account_exts)`

// CompileStalenessSnapshot runs the §9-A1 probe tuple (counts + freshness
// maxima) for the scheduler backstop. Colocated with LoadGroupsAccounts: same
// tables, same driver-seam convention (zero IN parameters — the 65,535
// discipline), same soft-delete predicates. The scheduler consumes the tuple
// only — no SQL, no pool ever enters the lane.
func (r *GroupRepo) CompileStalenessSnapshot(ctx context.Context) (domain.CompileStaleness, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, compileStalenessSQL, []any{}, rows); err != nil {
		return domain.CompileStaleness{}, fmt.Errorf("compile staleness snapshot: %w", err)
	}
	defer rows.Close()
	var out domain.CompileStaleness
	var accountsUpdated, groupsUpdated, templatesUpdated time.Time
	if !rows.Next() {
		return domain.CompileStaleness{}, fmt.Errorf("compile staleness snapshot: no rows")
	}
	if err := rows.Scan(&out.Accounts, &out.MaxLifecycleRevision, &accountsUpdated,
		&out.Groups, &groupsUpdated, &out.Templates, &templatesUpdated,
		&out.Memberships, &out.Exts); err != nil {
		return domain.CompileStaleness{}, fmt.Errorf("compile staleness snapshot (scan): %w", err)
	}
	if err := rows.Err(); err != nil {
		return domain.CompileStaleness{}, fmt.Errorf("compile staleness snapshot: %w", err)
	}
	out.AccountsUpdatedAtNano = accountsUpdated.UnixNano()
	out.GroupsUpdatedAtNano = groupsUpdated.UnixNano()
	out.TemplatesUpdatedAtNano = templatesUpdated.UnixNano()
	return out, nil
}

// LoadGroupMultipliers 全量组倍率快照（id → 万分数；groups.price_multiplier
// NOT NULL 默认 10000——每行都有值；billing.Balances.Reload 调用）。独立方法
// 不并入 LoadGroupsAccounts（后者是账号路由快照，语义/带宽不同）。
func (r *GroupRepo) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	rows, err := r.client.Group.Query().Select(group.FieldID, group.FieldPriceMultiplier).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int, len(rows))
	for _, row := range rows {
		out[row.ID] = row.PriceMultiplier
	}
	return out, nil
}

// LoadAssignmentMultipliers 全量用户-组专属倍率快照（(user_id, group_id) →
// 万分数；仅 group_assignments.price_multiplier 非 NULL 行——缺失 = 未设置 →
// 用组倍率；billing.Balances.Reload/ReloadMultipliers 调用 修正：用户
// 专属倍率按组挂载）。
func (r *GroupRepo) LoadAssignmentMultipliers(ctx context.Context) (map[billing.AssignmentKey]int, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.PriceMultiplierNotNil()).
		Select(groupassignment.FieldUserID, groupassignment.FieldGroupID, groupassignment.FieldPriceMultiplier).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[billing.AssignmentKey]int, len(rows))
	for _, row := range rows {
		if row.PriceMultiplier == nil {
			continue // 非 NULL 谓词兜底（Select 子集查询仍可能带 nil？不——谓词已过滤，防御）
		}
		out[billing.AssignmentKey{UserID: row.UserID, GroupID: row.GroupID}] = *row.PriceMultiplier
	}
	return out, nil
}

// LoadGroupAccounts 单组账号（组级定向重载数据源）。崩溃修复：旧实现
// `QueryAccounts()` 的 m2m 邻接跳生成 `WHERE id IN (该组全部账号 id)`——
// 单组账号数 >65,535 即超 PG 参数上限。改用 EXISTS 谓词
// `HasGroupsWith(group.IDEQ(groupID))`：ent 生成
// `WHERE accounts.id IN (SELECT ag.account_id FROM account_groups ag JOIN
// groups g ON ag.group_id = g.id WHERE g.id = $1)`——外层 IN 是子查询
// （非参数列表），语句参数数恒为 1（+ 模板 IN，受模板表实体数约束）。
// 返回无序（调用方构建快照/路由，不依赖顺序）。
func (r *GroupRepo) LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error) {
	// 软删除：已删 account 不进调度器快照（与 LoadGroupsAccounts 同语义）。
	// 账号侧 ext 同触顶约束（单组账号 >65,535）：不 eager-load，经 account 边
	// 子查询过滤——ent 对 m2o 边生成 `account_id IN (SELECT id FROM accounts
	// WHERE ...)`（子查询非参数列表，语句参数数恒为常量——与上方 EXISTS 谓词
	// 同界）；account_ext 表按组定向取，不做全表扫描（组级定向重载语义）。
	accs, err := r.client.Account.Query().
		Where(account.DeletedAtIsNil(), account.HasGroupsWith(group.IDEQ(groupID))).
		WithTemplate(func(q *ent.TemplateQuery) { q.WithExt() }). // ext 边快照合并 StripImageTools
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load group accounts (group %d): %w", groupID, err)
	}
	exts, err := r.client.AccountExt.Query().
		Where(accountext.HasAccountWith(account.DeletedAtIsNil(), account.HasGroupsWith(group.IDEQ(groupID)))).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load group accounts (account_ext scan, group %d): %w", groupID, err)
	}
	extByAccount := make(map[int64]*ent.AccountExt, len(exts))
	for _, e := range exts {
		extByAccount[e.AccountID] = e
	}
	out := make([]*domain.Account, 0, len(accs))
	for _, a := range accs {
		if e := extByAccount[a.ID]; e != nil {
			a.Edges.Ext = []*ent.AccountExt{e}
		}
		out = append(out, toDomainAccount(a))
	}
	return out, nil
}
