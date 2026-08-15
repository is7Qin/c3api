// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/key"
)

// KeyRepo 客户端 API key（独立表）持久化。
type KeyRepo struct {
	client *ent.Client
	// driver 为 raw SQL（AddQuotaUsed 单语句 CASE 批量更新）用：与 txDriver
	// 组合保证 raw SQL 与 ent 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
}

func (r *KeyRepo) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	row, err := r.client.Key.Create().
		SetUserID(k.UserID).
		SetGroupID(k.GroupID).
		SetName(k.Name).
		SetKeyHash(k.KeyHash).
		SetKeyPrefix(k.KeyPrefix).
		SetStatus(key.Status(k.Status)).
		SetMaxConcurrency(k.MaxConcurrency).
		SetQuota(k.Quota).
		SetQuotaUsed(k.QuotaUsed).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: key_hash=%q", ErrConflict, k.KeyHash)
		}
		return nil, err
	}
	return toDomainKey(row), nil
}

func (r *KeyRepo) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	row, err := r.client.Key.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainKey(row), nil
}

// QuotaUsed 读单 key 当前已用额度（#14 预算复核点读：本地预算耗尽时
// SELECT quota_used——DB 权威值，usage.Recorder 批量增量回写后滞后 ≤ flush
// 间隔；复核成功才重新分配本地预算）。热路径不调（仅复核慢路径）。
func (r *KeyRepo) QuotaUsed(ctx context.Context, id int64) (int64, error) {
	v, err := r.client.Key.Query().Where(key.IDEQ(id)).Select(key.FieldQuotaUsed).Int(ctx)
	if err != nil {
		return 0, errMissingID(err, id)
	}
	return int64(v), nil
}

// GetKeyByHash 按 hash 取 key（鉴权路径：已软删 key 按未找到处理——
// deleted_at IS NULL 过滤 → 返回 (nil, nil) → 鉴权拒绝）；未找到返回 (nil, nil)。
func (r *KeyRepo) GetKeyByHash(ctx context.Context, hash string) (*domain.Key, error) {
	row, err := r.client.Key.Query().Where(key.KeyHashEQ(hash), key.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainKey(row), nil
}

func (r *KeyRepo) ListKeysByUser(ctx context.Context, userID int64, q ListQuery) ([]*domain.Key, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Key.Query().Where(key.UserIDEQ(userID), key.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(key.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(keySortFields)
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
	out := make([]*domain.Key, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainKey(row))
	}
	return out, int64(total), nil
}

// UpdateKey 更新 name/status/max_concurrency/quota（hash 走 RotateKey）。
// quota_used 不写——Recorder 派生计数器（p2-12 核实：service 层无任何路径
// 意图写该列，全字段写回会覆盖 AddQuotaUsed 增量 → 永久少记、gate 超用
// 不 429）；ent Save re-SELECT 返回行 → 调用方拿到的 QuotaUsed 反为 DB 新鲜
// 值，upsertKeyMeta 顺带同步最新。
func (r *KeyRepo) UpdateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	row, err := r.client.Key.UpdateOneID(k.ID).
		SetName(k.Name).
		SetStatus(key.Status(k.Status)).
		SetMaxConcurrency(k.MaxConcurrency).
		SetQuota(k.Quota).
		Save(ctx)
	if err != nil {
		return nil, errMissingID(err, k.ID)
	}
	return toDomainKey(row), nil
}

// RotateKey 轮换 key：换 hash/key_prefix（raw 明文仅服务层返回一次）。
func (r *KeyRepo) RotateKey(ctx context.Context, id int64, newHash, newPrefix string) (*domain.Key, error) {
	row, err := r.client.Key.UpdateOneID(id).SetKeyHash(newHash).SetKeyPrefix(newPrefix).Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: key_hash=%q", ErrConflict, newHash)
		}
		return nil, errMissingID(err, id)
	}
	return toDomainKey(row), nil
}

// DeleteKey 软删除：deleted_at 置值（行保留留审计；鉴权快照按 deleted_at
// IS NULL 过滤 → 已删 key 鉴权拒绝，GET 单个仍可查已删项）。bulk Update
//（无 re-SELECT）单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *KeyRepo) DeleteKey(ctx context.Context, id int64) error {
	n, err := r.client.Key.Update().Where(key.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// DeleteKeysByGroup 软删除组的全部 key（组删除级联——deleted_at 置值，行保留
// 不破坏 key.group_id 外键），返回本次被软删的 hash 列表（Auth 增量清理用；
// 已软删 key 过滤——其 hash 此前已从 Auth 移除，重复返回无意义）。
func (r *KeyRepo) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	rows, err := r.client.Key.Query().
		Where(key.GroupIDEQ(groupID), key.DeletedAtIsNil()).
		Select(key.FieldKeyHash).
		All(ctx)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(rows))
	for _, row := range rows {
		hashes = append(hashes, row.KeyHash)
	}
	if _, err := r.client.Key.Update().Where(key.GroupIDEQ(groupID)).SetDeletedAt(time.Now()).Save(ctx); err != nil {
		return nil, err
	}
	return hashes, nil
}

// LoadKeys 构建 Auth 鉴权快照：key_hash → KeyMeta（含归属用户状态/并发/
// 额度）。热路径数据源（reload 时一次查询；请求路径零 DB）。
//
// 崩溃修复：旧实现 `WithUser()` eager-load 的 m2o 邻接跳生成
// `SELECT * FROM users WHERE id IN (全部 key 的归属用户 id)`——key 数
// >65,535（用户数也随之超限）即超 PG 参数上限 65535。改为分片：先全表扫
// key id（零参数），按 ≤inChunkSize 切块逐块 `Where(key.IDIn(块))` 加载
// （单块 key ≤8192，其 m2o 邻接 IN ≤8192 个用户 id——每 key 恰一个归属
// 用户，邻接恒被块大小约束）。
func (r *KeyRepo) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	// 软删除：已删 key 不进鉴权快照（id 扫描即过滤；分块查询按 id 白名单继承）。
	keyIDs, err := r.client.Key.Query().Where(key.DeletedAtIsNil()).Order(ent.Asc(key.FieldID)).IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load keys (scan ids): %w", err)
	}
	out := make(map[string]domain.KeyMeta, len(keyIDs))
	chunks := chunkIDs(keyIDs, inChunkSize)
	for i, chunk := range chunks {
		rows, err := r.client.Key.Query().
			Where(key.IDIn(chunk...)).
			WithUser().
			WithGroup().
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("load keys (chunk %d/%d, %d ids): %w", i+1, len(chunks), len(chunk), err)
		}
		for _, row := range rows {
			meta := domain.KeyMeta{
				KeyID:      row.ID,
				UserID:     row.UserID,
				GroupID:    row.GroupID,
				KeyStatus:  domain.KeyStatus(row.Status),
				KeyMaxConc: row.MaxConcurrency,
				HasQuota:   row.Quota > 0,
				Quota:      row.Quota,
				QuotaUsed:  row.QuotaUsed,
			}
			if row.Edges.User != nil {
				meta.UserStatus = domain.UserStatus(row.Edges.User.Status)
				meta.UserMaxConc = row.Edges.User.MaxConcurrency
			}
			// 组级 protocol_convert 快照（W5 热路径分支数据源；组软删窗口内
			// 行仍在 → 值照旧，快照一致性由 Reload 收敛）。
			if row.Edges.Group != nil {
				meta.ProtocolConverts = toDomainProtocolConverts(row.Edges.Group.ProtocolConvert)
			}
			out[row.KeyHash] = meta
		}
	}
	return out, nil
}

// AddQuotaUsed 批量回写 key 额度消耗（增量；Recorder 节奏，内存权威，
// DB 滞后 ≤ flush 间隔）。单条 SQL CASE 批量更新替代逐 key UpdateOneID 轮询
//（#15 验收：10k 逐 key 额度写回是统计面慢 flush 3-5min 周期根因之一）。
// key 已删（不在 IN 列表）静默跳过——回写无意义（与旧逐 key
// ent.IsNotFound 跳过语义一致）。调用方（usage.Recorder.flushStats）按
// quotaBatchSize 分块并以块为失败回灌原子单位——本方法单语句全成或全败。
func (r *KeyRepo) AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`UPDATE "keys" SET "quota_used" = "quota_used" + CASE "id" `)
	args := make([]any, 0, len(deltas)*2)
	ids := make([]string, 0, len(deltas))
	for id, d := range deltas {
		if d == 0 {
			continue // 零增量无回写价值
		}
		idx := len(args) + 1
		b.WriteString("WHEN $")
		b.WriteString(strconv.Itoa(idx))
		b.WriteString(" THEN $")
		b.WriteString(strconv.Itoa(idx + 1))
		b.WriteByte(' ')
		args = append(args, id, d)
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	if len(ids) == 0 {
		return nil
	}
	b.WriteString(`ELSE "quota_used" END, "updated_at" = now() WHERE "id" IN (`)
	b.WriteString(strings.Join(ids, ", "))
	b.WriteByte(')')
	var res sql.Result
	return r.driver.Exec(ctx, b.String(), args, &res)
}
