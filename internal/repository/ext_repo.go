// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/templateext"
)

// TemplateExtRepo 模板类型化扩展（template_ext 1:1 边缘表；通用框架——codex
// 专属账号 ext 见 ext_codex_repo.go）：credential_type ∈ {responses-special,
// codex-oauth, codex-pat} 的模板才有 ext 行。模板是共享配置面：只存类型声明 +
// strip_image_tools 公共开关——凭据列组（oauth/pat）一律在 account_ext。
// 类型一致性由 service 校验；FK 由 ent 边缘表保证（父行缺失 → 约束错误，
// 调用方先查父行）。
type TemplateExtRepo struct {
	client *ent.Client
	// driver 为原始 dialect.Driver：守卫写的父行锁（`SELECT ... FOR UPDATE`）走
	// raw SQL——ent 生成器未开 sql/lock 特性，`TemplateQuery` 没有 `ForUpdate()`
	// 入口（与 ext_codex_repo.go 的 owner 锁同一范式）；WithTx/tx 面事务驱动保证
	// raw SQL 与 ent 构建器同连接。
	driver dialect.Driver
}

// TemplateExtDecide 守卫写的事务内判定回调：入参是**事务内**读到的旧行
// （首次建行 = nil）。返回 down = 本次写是否真的完成了 ws→非 ws 的降级（供
// service 决定是否写审计 WARN）；err != nil ⇒ 整事务回滚，该 err **原样**上抛
// （不得 `%w` 包成 DB 错误，否则调用方 `errors.Is(err, ErrConflict)` 失效）。
//
// 必须是纯函数：入参只有事务内读到的 old（+ 调用方闭包捕获的请求参数与出口），
// **不得**访问 Store/DB/日志——它被调用在已持有父行锁的事务里，回头读库等于等
// 自己持有的锁，直到 ctx 超时挂住。
type TemplateExtDecide func(old *domain.TemplateExt) (down bool, err error)

// UpsertTemplateExt 是 template_ext 的**唯一写入口**（spec §4.3 原子性约束）：
//
//	Tx → SELECT templates.id FOR UPDATE（0 行 ⇒ 父模板已删 ⇒ ErrNotFound）
//	   → 读 ext 行（无行 ⇒ old = nil）
//	   → decide(old)（非 nil err ⇒ 回滚并按原样返回该 err）
//	   → 私有 upsert（用 tx.Client()）
//	   → **事务内**回读 → 提交 → 返回
//
// 为什么必须有这道门：`old` 与实际被覆盖的值必须绑定。事务外读到的旧值只允许
// 用于快速失败/回显——A 读到 `http` → B 抢先把值升到 `ws` → A 无条件 upsert 会
// 实际完成 `ws→http`，既不 409 也不 WARN（TOCTOU）。锁**父行**而不是锁 ext 行，
// 因为首次建行时根本无 ext 行可锁；顺带把 UpdateTemplate（改父类型）串行化。
//
// 返回落库后的完整行 + down（decide 的降级判定）+ error。
//
// decide **不得为 nil**（spec v4.26 写死）：nil 不是"不判定"，而是"把 409 门整个
// 关掉"，故入口处 panic——绝不静默放行。
func (r *TemplateExtRepo) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt, decide TemplateExtDecide) (*domain.TemplateExt, bool, error) {
	if decide == nil {
		// nil 不是"不判定"，而是"把判定整个关掉"——那正是 409 门要防的事，所以它是
		// 编程错误（不是可重试的运行时状态），入口处直接 panic：绝不静默放行。
		panic("repository: template ext guard write requires a non-nil decide callback")
	}
	var (
		down  bool
		saved *domain.TemplateExt
	)
	err := withWriteTx(ctx, r.driver, func(tx *ent.Client, txDrv dialect.Driver) error {
		if err := lockTemplateExtParentForUpdate(ctx, txDrv, e.TemplateID); err != nil {
			return err
		}
		old, err := templateExtRow(ctx, tx, e.TemplateID)
		if err != nil {
			return err
		}
		d, err := decide(old)
		if err != nil {
			// 原样上抛：service 的 409 哨兵经此到达 handler 的 errors.Is。
			return err
		}
		down = d
		if err := upsertTemplateExtUnlocked(ctx, tx, e); err != nil {
			return err
		}
		// 回读必须留在**事务内**（spec v4.27）：upsert 的 RETURNING 只给 id，全列
		// 还得再读一次。放到提交之后去读会开出两个真故障窗口：
		//   ① 写已提交但回读失败 ⇒ 调用方拿到 error ⇒ service 提前返回 ⇒ 既不写
		//      审计 WARN、也不执行 Templates()/NOTIFY —— 写入成功而快照不失效，
		//      开关静默失效（本项目最忌的那类故障）；
		//   ② 另一条守卫写在回读之前提交 ⇒ saved 是**别人的**值 ⇒ WARN 会记成
		//      "downgraded, old=ws, new=ws" 这种自相矛盾的证据。
		// 锁还在自己手里时读到的就是本次写入的提交前状态，两个窗口都不存在。
		row, err := templateExtRow(ctx, tx, e.TemplateID)
		if err != nil {
			return err
		}
		if row == nil {
			// 自己刚写的行自己都读不回来 ⇒ 不提交（不提交一个读不回的行）。
			return fmt.Errorf("%w: template_id=%d missing after upsert", ErrNotFound, e.TemplateID)
		}
		saved = row
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return saved, down, nil
}

// lockTemplateExtParentForUpdate 在**守卫写事务内**以 `SELECT ... FOR UPDATE` 锁
// 父模板行（spec §4.3 原子性约束第一条）：命中 0 行 ⇒ 父模板已删 ⇒ ErrNotFound
// （与既有「父模板不存在时 FK 保证必无行」口径一致）。持锁期间并发写同一模板的
// ext 行、以及改父模板类型的 UpdateTemplate 都会阻塞，直到本事务结束——故
// 「事务内读 old」与「写入」之间没有丢失更新的窗口。
func lockTemplateExtParentForUpdate(ctx context.Context, driver dialect.Driver, templateID int64) error {
	rows := &entsql.Rows{}
	// 表名是字面量而不是 schema 常量：这里写的是裸 SQL 的物理表，与 ent 迁移
	// 产物同名（写面约定见 spec §11-6：唯一写入口靠 code review 守，无机器判据）。
	if err := driver.Query(ctx, `SELECT id FROM templates WHERE id = $1 FOR UPDATE`, []any{templateID}, rows); err != nil {
		return err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: template_id=%d missing", ErrNotFound, templateID)
	}
	return nil
}

// templateExtRow 读 ext 行；无行 ⇒ (nil, nil)——事务内读不到行即"首次建行"，
// old 由调用方按默认面处理（不是 404）。
func templateExtRow(ctx context.Context, c *ent.Client, templateID int64) (*domain.TemplateExt, error) {
	row, err := c.TemplateExt.Query().Where(templateext.TemplateIDEQ(templateID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainTemplateExt(row), nil
}

// upsertTemplateExtUnlocked 无锁幂等写入（Create/Update 合一）：template_id 冲突
// → 全列更新（含 NULL 清空——调用方显式传 nil 即清列），无冲突 → 插入。
// **私有**：它只能被守卫写在持锁事务内调用，否则 FOR UPDATE 形同虚设（写面约定见
// spec §11-6：唯一写入口靠 code review 守，无机器判据）。
// 注意 ent 语义：SetNillableX(nil) 是 no-op（不进 INSERT 列），NULL 清空必须在
// 冲突 UPDATE 里显式 ClearX（与 pricing repo 同模式）。
func upsertTemplateExtUnlocked(ctx context.Context, c *ent.Client, e *domain.TemplateExt) error {
	_, err := c.TemplateExt.Create().
		SetTemplateID(e.TemplateID).
		SetCredentialType(string(e.CredentialType)).
		SetNillableStripImageTools(e.StripImageTools).                 // nil = no-op（插入路径缺列 → NULL）
		SetUpstreamPaths(e.UpstreamPaths).                             // nil → NULL（JSON 列零值即 nil）
		SetCodexResponsesTransport(string(e.CodexResponsesTransport)). // 非可空列：零值 "" 即"未配置 = 默认 http"
		OnConflictColumns(templateext.FieldTemplateID).
		Update(func(u *ent.TemplateExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			// 非可空列：恒 Set（不 Clear——空串本身就是默认面的完整表达）
			u.SetCodexResponsesTransport(string(e.CodexResponsesTransport))
			// 可空列：非 nil → Set 覆盖；nil → Clear（NULL 清空）
			if e.StripImageTools != nil {
				u.SetStripImageTools(*e.StripImageTools)
			} else {
				u.ClearStripImageTools()
			}
			if e.UpstreamPaths != nil {
				u.SetUpstreamPaths(e.UpstreamPaths)
			} else {
				u.ClearUpstreamPaths()
			}
		}).
		ID(ctx)
	return err
}

// GetTemplateExt 按模板 id 取扩展行；无行 → ErrNotFound（template_id 缺失——
// 父模板不存在时 FK 保证必无行，404 语义一致）。
func (r *TemplateExtRepo) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	e, err := templateExtRow(ctx, r.client, templateID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("%w: template_id=%d missing", ErrNotFound, templateID)
	}
	return e, nil
}
