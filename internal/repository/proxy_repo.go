// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/proxy"
)

// ProxyRepo 出口代理条目的持久面（spec-proxy-pool §3.1/§6.1）。表是扁平的：没有
// edge、没有外键，绑定关系只有 account.proxy_id 一列，删除全是软删——故 DB 级联
// 永不触发，解绑由 DeleteProxy 在**同一事务**内显式完成。
type ProxyRepo struct {
	client *ent.Client
	driver dialect.Driver
}

// CreateProxy 新建条目，返回新 id。name 唯一冲突映射为 ErrConflict（与 template/
// group 同款：先读既有做法——不新造错误类型，service 侧映射 409）。
func (r *ProxyRepo) CreateProxy(ctx context.Context, p domain.Proxy) (int64, error) {
	b := r.client.Proxy.Create().
		SetName(p.Name).
		SetScheme(string(p.Scheme)).
		SetHost(p.Host).
		SetPort(p.Port).
		SetEnabled(p.Enabled)
	// 认证与备注：空 = 无（落 NULL）。
	if p.Username != "" {
		b = b.SetUsername(p.Username)
	}
	if p.Password != "" {
		b = b.SetPassword(p.Password)
	}
	if p.Remark != "" {
		b = b.SetRemark(p.Remark)
	}
	row, err := b.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return 0, fmt.Errorf("%w: name=%q", ErrConflict, p.Name)
		}
		return 0, err
	}
	return row.ID, nil
}

// ListProxies 列出全部存活条目（软删除过滤；稳定排序 ORDER BY id——列表无分页，
// 调用方按序回显）。本阶段不做 has_password 投影（API 面属 M1b）。
func (r *ProxyRepo) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := r.client.Proxy.Query().
		Where(proxy.DeletedAtIsNil()).
		Order(ent.Asc(proxy.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Proxy, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainProxy(row))
	}
	return out, nil
}

// GetProxy 取单个条目：**已软删视同不存在**（ErrNotFound）——与列表同谓词，
// 避免「列表看不到、GET 拿得到」的错位。
func (r *ProxyRepo) GetProxy(ctx context.Context, id int64) (domain.Proxy, error) {
	row, err := r.client.Proxy.Query().
		Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return domain.Proxy{}, errMissingID(err, id)
	}
	return toDomainProxy(row), nil
}

// ProxyCredential 代理条目的**凭据列**写面：两列都是三态（nil = 不变；&"" = 清空；
// &v = 落值）。
//
// 为什么两列一起三态而不是只给 password：它们是**同一对**凭据，"不变"必须同样原子。
// 只给 password 三态、让 username 走"先读旧值再整体写回"，就把 read-modify-write
// 窗口留给了 username——并发的另一次改凭据会被这次用读到的旧值静默回滚，而调用方
// 以为自己只是改了个备注。两个同类型指针并排也容易写反，故用命名结构体。
type ProxyCredential struct {
	Username *string
	Password *string
}

// applyProxySecret 把凭据三态落到 UPDATE 的 SET 列表：非 nil 的列才出现，
// nil 的列根本不进语句 ⇒ 数据库层面"不变"，与并发写不冲突。
func applyProxySecret(u *ent.ProxyUpdate, c ProxyCredential) {
	if c.Username != nil {
		if *c.Username == "" {
			u.ClearUsername()
		} else {
			u.SetUsername(*c.Username)
		}
	}
	if c.Password != nil {
		if *c.Password == "" {
			u.ClearPassword()
		} else {
			u.SetPassword(*c.Password)
		}
	}
}

// UpdateProxy 全量更新非密字段 + **凭据三态**（见 ProxyCredential）。三态在
// **同一条 UPDATE** 语句里表达（非 nil 的列才进 SET），故不需要「先读旧值再整体
// 写回」——后者有 read-modify-write 窗口：并发的另一次改凭据会被这次写回用读到的
// 旧值覆盖（静默回滚），而 spec §6.1 的 PUT 语义要求三态原子。
//
// p.Username / p.Password 在本函数里**被忽略**（只有 CreateProxy 用它们）：更新面
// 的凭据只经 ProxyCredential 表达，避免"两处都能设凭据"。
//
// 其余字段仍按补丁值整体覆盖（幂等、不依赖旧值）。软删行视同不存在；name 撞其它
// 存活/已软删条目一律 ErrConflict（唯一索引不含 deleted_at——与 template 同语义）。
// UpdatedAt 由列默认/更新默认处理，不在入参里。
//
// **不写 last_probe_\* 三列**：探测结果由 WriteProxyProbe 独占写（一个事实一个写者）。
// 若让全量更新也写它们，管理面 PUT 就会把上一次探测结论静默抹成「从未探测」——
// 而探测结论是运维唯一的现场证据，不能因为改个备注而丢。
func (r *ProxyRepo) UpdateProxy(ctx context.Context, p domain.Proxy, credential ProxyCredential) error {
	u := r.client.Proxy.Update().
		Where(proxy.IDEQ(p.ID), proxy.DeletedAtIsNil()).
		SetName(p.Name).
		SetScheme(string(p.Scheme)).
		SetHost(p.Host).
		SetPort(p.Port).
		SetEnabled(p.Enabled)
	// 凭据两列的三态在同一语句里落值（nil 的列不进 SET ⇒ 不变）。
	applyProxySecret(u, credential)
	if p.Remark != "" {
		u = u.SetRemark(p.Remark)
	} else {
		u = u.ClearRemark()
	}
	n, err := u.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return fmt.Errorf("%w: name=%q", ErrConflict, p.Name)
		}
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, p.ID)
	}
	return nil
}

// DeleteProxy 软删 + **同事务解绑连坐**（spec §6.1）：先把条目置 deleted_at，
// 再把仍指向它的账号 `proxy_id` 清成 NULL（回落直连）。两件事必须原子——否则
// 中途失败会留下「指向已删条目」的悬垂绑定（加载期虽按「软删 = 不可用」兜底，
// 但账号会永远卡在不可用状态）。连坐不区分账号是否已软删：列级解绑，语义同
// `UPDATE accounts SET proxy_id = NULL WHERE proxy_id = $1`。
func (r *ProxyRepo) DeleteProxy(ctx context.Context, id int64) error {
	return withWriteTx(ctx, r.driver, func(client *ent.Client, _ dialect.Driver) error {
		n, err := client.Proxy.Update().
			Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
			SetDeletedAt(time.Now()).
			Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			// 已软删/不存在：与 GetProxy 同哨兵（重复删除不再静默成功）。
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
		if _, err := client.Account.Update().
			Where(account.ProxyID(id)).
			ClearProxyID().
			Save(ctx); err != nil {
			return err
		}
		return nil
	})
}

// WriteProxyProbe 写回探测结果三元组（last_probe_at / ok / error）。errText 由调用方
// 脱敏（§5.2）；探测结果**不影响** Routable（§6.3：enabled 是唯一开关）。
func (r *ProxyRepo) WriteProxyProbe(ctx context.Context, id int64, at time.Time, ok bool, errText string) error {
	n, err := r.client.Proxy.Update().
		Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
		SetLastProbeAt(at).
		SetLastProbeOk(ok).
		SetLastProbeError(errText).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}
