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
	"github.com/is7qin/c3api/internal/ent/predicate"
	"github.com/is7qin/c3api/internal/ent/proxy"
)

// ProxyRepo 出口代理条目的持久面（spec-proxy-pool §3.1/§6.1）。表是扁平的：没有
// edge、没有外键，绑定关系只有 account.proxy_id 一列，删除全是软删——故 DB 级联
// 永不触发，解绑由 DeleteProxy 在**同一事务**内显式完成。
type ProxyRepo struct {
	client *ent.Client
	driver dialect.Driver
}

// proxyOwnerPred 归属作用域谓词（spec-proxy-pool §3.6，D7 = C 模式）：供应商面
// （scope.Set）只看得见**自己的**条目（`owner_uid = scope.OwnerUID`）；管理面缺省
// （Set=false）⇒ 空谓词（全量，行为不变）。与账号面的 accountOwnerPred 同形态：作用域
// **显式入参**、调用方从 ctx 提取一次，AND 进 SQL——而不是「先按 id 取行、再应用层比
// 归属」（§2.5 禁的 TOCTOU）。
//
// NULL 语义正好是 spec 要的：`owner_uid = X` 对 NULL 恒 unknown ⇒ 平台代理在任何
// Set 作用域下都**不匹配**（平台池不是公共池：供应商既看不到也绑不上），不需要额外
// 写 IS NULL 分支。
func proxyOwnerPred(scope domain.AccountScope) []predicate.Proxy {
	if scope.Set {
		return []predicate.Proxy{proxy.OwnerUID(scope.OwnerUID)}
	}
	return nil
}

// CreateProxy 新建条目，返回新 id。name 唯一冲突映射为 ErrConflict（与 template/
// group 同款：先读既有做法——不新造错误类型，service 侧映射 409）。
//
// 归属（§3.6）由**作用域钉死**，而不是由入参决定：供应商面（scope.Set）恒落
// `owner_uid = scope.OwnerUID`（请求体里根本没有这个字段，`domain.Proxy.OwnerUID`
// 即使被调用方填了也不采信——否则 forge 一个 owner 就能把私池变成别人的/写成平台
// 代理）；管理面（无 scope）恒落 NULL = 平台代理。即「谁能创建，谁就拥有」这唯一事实。
func (r *ProxyRepo) CreateProxy(ctx context.Context, p domain.Proxy) (int64, error) {
	b := r.client.Proxy.Create().
		SetName(p.Name).
		SetScheme(string(p.Scheme)).
		SetHost(p.Host).
		SetPort(p.Port).
		SetEnabled(p.Enabled)
	if scope := domain.AccountScopeFrom(ctx); scope.Set {
		b = b.SetOwnerUID(scope.OwnerUID)
	}
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

// ListProxies 列出作用域内全部存活条目（软删除过滤；稳定排序 ORDER BY id——列表无
// 分页，调用方按序回显）。作用域见 proxyOwnerPred：管理面看全部，供应商面只看自己的。
func (r *ProxyRepo) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := r.client.Proxy.Query().
		Where(proxy.DeletedAtIsNil()).
		Where(proxyOwnerPred(domain.AccountScopeFrom(ctx))...).
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
// 避免「列表看不到、GET 拿得到」的错位。作用域同样 AND 进 WHERE：越域与不存在
// **不可区分**，一律 ErrNotFound（404 不泄露「别人有这条代理」）。
func (r *ProxyRepo) GetProxy(ctx context.Context, id int64) (domain.Proxy, error) {
	row, err := r.client.Proxy.Query().
		Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
		Where(proxyOwnerPred(domain.AccountScopeFrom(ctx))...).
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
		Where(proxyOwnerPred(domain.AccountScopeFrom(ctx))...).
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
// 但账号会永远卡在不可用状态）。
//
// 作用域与**解绑范围**（§3.6）：
//   - 作用域：越域（供应商删别人的）⇒ ErrNotFound，与 GetProxy 同一哨兵。
//   - 解绑连坐**随归属**：`owner_uid IS NULL`（平台代理）⇒ 既有行为（解绑所有引用）；
//     `owner_uid = X`（私池）⇒ 仅解绑 `supplier_user_id = X` 的账号。否则删一条私池条目会
//     把别的供应商/平台账号的可用绑定一起抖掉（跨租户副作用）。
//
// 归属必须**在软删之前**读：软删后本查询的 `deleted_at IS NULL` 就不再命中它了，
// 而解绑范围正需要这个事实。连坐不区分账号是否已软删：列级解绑。
func (r *ProxyRepo) DeleteProxy(ctx context.Context, id int64) error {
	scope := domain.AccountScopeFrom(ctx)
	return withWriteTx(ctx, r.driver, func(client *ent.Client, _ dialect.Driver) error {
		row, err := client.Proxy.Query().
			Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
			Where(proxyOwnerPred(scope)...).
			Only(ctx)
		if err != nil {
			// 已软删/不存在/越域：与 GetProxy 同哨兵（重复删除不再静默成功）。
			return errMissingID(err, id)
		}
		n, err := client.Proxy.Update().
			Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
			SetDeletedAt(time.Now()).
			Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			// check→exec 竞态窗口（并发删除）：同一哨兵。
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
		unbind := client.Account.Update().Where(account.ProxyID(id))
		if row.OwnerUID != nil {
			// 私池：只解绑其主的账号（owner 非 NULL ⇒ 这些账号属于该供应商）。
			unbind = unbind.Where(account.SupplierUserID(*row.OwnerUID))
		}
		if _, err := unbind.ClearProxyID().Save(ctx); err != nil {
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
		Where(proxyOwnerPred(domain.AccountScopeFrom(ctx))...).
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

// ProxyBindingInfo 绑定校验所需的代理事实（spec-proxy-pool §3.6 的唯一绑定谓词）。
//
// 只有两个字段，因为绑定校验只认这两个事实：归属（决定「能不能绑」）与可用性开关
// （enabled 是**唯一**可用性开关，§6.3）。不返回凭据/地址：绑定校验不需要出口规格，
// 多给一个字段就多一处泄漏面。
type ProxyBindingInfo struct {
	OwnerUID *int64 // nil = 平台代理（只有管理员可分配）
	// Enabled **不参与谓词**（谓词只判存在与归属）：停用是运行期可用性，绑定是权限事实
	// ——「先绑好、后启用」是正常运维顺序，可用性由 Routable 在装载期派生。它在这里是
	// 因为「判据所需的事实」应当一次读全（调用方不必为同一个判断再查一次行）。
	Enabled bool
}

// LookupProxyForBinding 读存活行的归属与可用性；不存在/已软删 ⇒ ErrNotFound。
//
// **跨 scope 读取**（故意不带 proxyOwnerPred）：这里的语义是「能不能绑」，不是
// 「能不能看」。绑定校验要能判断「这条代理是谁的」，否则管理员无 scope 时永远只看
// 得到平台代理、供应商也永远判不出「这条不是我的」——两者都会退化成一条静默的
// 越权通路（前者：管理员绑不了私池；后者：供应商绑得上别人的）。可见性由
// ListProxies/GetProxy 的 scope 谓词负责，两条面各司其职。
func (r *ProxyRepo) LookupProxyForBinding(ctx context.Context, id int64) (ProxyBindingInfo, error) {
	row, err := r.client.Proxy.Query().
		Where(proxy.IDEQ(id), proxy.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return ProxyBindingInfo{}, errMissingID(err, id)
	}
	return ProxyBindingInfo{OwnerUID: row.OwnerUID, Enabled: row.Enabled}, nil
}
