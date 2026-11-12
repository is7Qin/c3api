// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// payment_account_repo.go 支付账户与渠道持久化（spec 支付网关 v2 §3-1/§3-2/§10-3）。
//
// 两个**独立**面（都不进 TxStore）：账户/渠道 CRUD 永远不参与支付事务——CompleteOrder
// 的事务里只允许「行锁 + 兑换码 + 状态迁移」，账户变更走自己的提交，否则一次账户
// 编辑就能把入账事务拖成锁竞争。
//
// 边界：provider 相关语义（`pay_type ∈ capabilities(provider)`、config.ParseConfig
// 推导 MerchantRef、密钥掩码）**不在 repo**——repo 不引 provider。repo 只保住数据库
// 能保住的不变量：`key` 唯一、`(provider, merchant_ref)` 唯一、`(account_id, pay_type)`
// 唯一、`config` 是合法 JSON 列。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/paymentaccount"
	"github.com/is7qin/c3api/internal/ent/paymentchannel"
)

// ErrMerchantRefImmutable 账户更新时新 config 推导出的 MerchantRef 与现列不符（N29）。
//
// 它**是** ErrConflict 的细分（errors.Is 两者都为真）：数据库层能给的语义就是冲突，
// 但 spec §10-3 要求服务层映射 **400**（"改商户身份请新建账户 + 停用旧账户"），故单列
// 一个可识别的子哨兵免得服务层按字符串匹配。
var ErrMerchantRefImmutable = fmt.Errorf("%w: merchant_ref is read-only", ErrConflict)

// ErrConfigRevisionMismatch 账户更新的乐观并发闸门未通过：调用方带的
// `expected_config_revision` 与现列不符（说明这份表单是基于过期快照填的）。
//
// 它同样是 ErrConflict 的细分（`errors.Is(err, ErrConflict)` 为真 → 服务层用
// `mapRepoErr` 归一成 409）。与 ErrMerchantRefImmutable 的区别是**方向相反**：
// 那个是"你写错了配置"（400，改商户身份），这个是"别人先改了"（409，刷新重试）。
var ErrConfigRevisionMismatch = fmt.Errorf("%w: config_revision mismatch", ErrConflict)

// PaymentAccountRepo 支付账户持久化（payment_accounts）。
type PaymentAccountRepo struct {
	client *ent.Client
}

// AccountUpdate 账户可变更字段（spec §10-3：只改 display_name/priority/enabled/config）。
//
// 指针字段 nil = 保持不变（部分更新）；Config 为 nil = 不改配置（**不动**
// config_revision）。
type AccountUpdate struct {
	DisplayName *string
	Priority    *int
	Enabled     *bool
	Config      json.RawMessage
	// MerchantRef 新 config 由协议 ParseConfig 推导出的商户标识；Config 非 nil
	// 时必填，且必须等于现列（否则 ErrMerchantRefImmutable）。
	MerchantRef string
	// ExpectedConfigRevision 乐观并发闸门（§19-1 / P57）：> 0 时更新语句带
	// `AND config_revision = ?`，0 行且行存在 → ErrConfigRevisionMismatch（409）。
	// 0 = 不校验（与 r12 行为等价；脚本化调用不必带版本）。
	//
	// 为什么需要它：服务层是「读旧 config → 合并 `"***"` → 写回」，无条件的写回会
	// 把并发轮换的新密钥静默改回旧值（两人都收到 200，下次回调验签失败）。
	ExpectedConfigRevision int64
}

// CreateAccount 插入账户。`key` 或 `(provider, merchant_ref)` 唯一冲突 → ErrConflict
// （服务层分别映射 409 / 409）。
//
// config_revision 由本方法定版：调用方给 ≤0 时落 1（schema 默认值同值），"创建即
// revision 1"是配置缓存键的起点。
func (r *PaymentAccountRepo) CreateAccount(ctx context.Context, a *domain.PaymentAccount) (*domain.PaymentAccount, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil payment account", ErrInvalidInput)
	}
	if a.Key == "" || a.Provider == "" || a.MerchantRef == "" {
		return nil, fmt.Errorf("%w: payment account requires key/provider/merchant_ref", ErrInvalidInput)
	}
	rev := a.ConfigRevision
	if rev <= 0 {
		rev = 1
	}
	row, err := r.client.PaymentAccount.Create().
		SetKey(a.Key).
		SetProvider(string(a.Provider)).
		SetMerchantRef(a.MerchantRef).
		SetDisplayName(a.DisplayName).
		SetEnabled(a.Enabled).
		SetPriority(a.Priority).
		SetConfig(a.Config).
		SetConfigRevision(rev).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: payment account key=%q provider=%q merchant_ref=%q",
				ErrConflict, a.Key, a.Provider, a.MerchantRef)
		}
		return nil, err
	}
	return toDomainPaymentAccount(row), nil
}

// GetAccountByKey 按 key 取账户；缺失 → ErrNotFound（含 key）。
//
// 这是回调路径的**唯一**账户入口（§6-1：路径即账户），也是选路/配置缓存的取数点。
func (r *PaymentAccountRepo) GetAccountByKey(ctx context.Context, key string) (*domain.PaymentAccount, error) {
	row, err := r.client.PaymentAccount.Query().
		Where(paymentaccount.KeyEQ(key)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: payment account key=%q", ErrNotFound, key)
		}
		return nil, err
	}
	return toDomainPaymentAccount(row), nil
}

// ListAccounts 全量账户，按选路序（priority ASC, id ASC）返回——管理面展示顺序与
// 实际选路顺序一致，运维不必脑内再排一次。渠道由调用方按 ListChannels 逐账户拼装
// （列表页 N+1 可接受：账户数是"一家上游一行"的量级）。
func (r *PaymentAccountRepo) ListAccounts(ctx context.Context) ([]*domain.PaymentAccount, error) {
	rows, err := r.client.PaymentAccount.Query().
		Order(ent.Asc(paymentaccount.FieldPriority), ent.Asc(paymentaccount.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.PaymentAccount, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPaymentAccount(row))
	}
	return out, nil
}

// UpdateAccount 更新账户可变更字段；`config_revision` 随 config 变更 +1
// （配置缓存的失效键）。缺失 → ErrNotFound。
//
// **merchant_ref 在这里永不被写**（N29/N4 的落地方式）：列不可变，所以「新 config 的
// MerchantRef() 必须等于现列」这一比对**不需要事务**——不存在"校验通过后被并发改掉"
// 的窗口（没有任何写路径能改它）。同理 provider/key 也不在本方法的能力面内。
func (r *PaymentAccountRepo) UpdateAccount(ctx context.Context, key string, upd AccountUpdate) (*domain.PaymentAccount, error) {
	cur, err := r.GetAccountByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if upd.Config != nil {
		if upd.MerchantRef == "" {
			return nil, fmt.Errorf("%w: payment account key=%q: config update requires merchant_ref", ErrInvalidInput, key)
		}
		if upd.MerchantRef != cur.MerchantRef {
			// 服务层按 spec §10-3 映射 400（不是 409）：这是"你写了错误的配置"，
			// 不是并发冲突。见 ErrMerchantRefImmutable 注释。
			return nil, fmt.Errorf("%w: payment account key=%q: current=%q new config=%q",
				ErrMerchantRefImmutable, key, cur.MerchantRef, upd.MerchantRef)
		}
	}
	// 零变更请求（全部指针 nil 且 Config nil）：不写库、不碰 updated_at，直接回现列
	// ——语义是幂等 no-op，而不是"缺行"。
	//
	// 但**带版本闸门时仍要校验**（§19-1）：否则 `PUT {expected_config_revision: 旧值}`
	// 这种"只探一下版本"的请求会得到 200，把过期快照谎报成仍是最新。
	if upd.DisplayName == nil && upd.Priority == nil && upd.Enabled == nil && upd.Config == nil {
		if upd.ExpectedConfigRevision > 0 && cur.ConfigRevision != upd.ExpectedConfigRevision {
			return nil, fmt.Errorf("%w: payment account key=%q: expected config_revision=%d actual=%d",
				ErrConfigRevisionMismatch, key, upd.ExpectedConfigRevision, cur.ConfigRevision)
		}
		return cur, nil
	}
	u := r.client.PaymentAccount.Update().Where(paymentaccount.KeyEQ(key))
	if upd.ExpectedConfigRevision > 0 {
		// CAS 闸门（§19-1 / P57）：条件与应用在同一条 UPDATE 里，不存在
		// "校验通过后被并发改掉"的窗口。
		u = u.Where(paymentaccount.ConfigRevisionEQ(upd.ExpectedConfigRevision))
	}
	if upd.DisplayName != nil {
		u = u.SetDisplayName(*upd.DisplayName)
	}
	if upd.Priority != nil {
		u = u.SetPriority(*upd.Priority)
	}
	if upd.Enabled != nil {
		u = u.SetEnabled(*upd.Enabled)
	}
	if upd.Config != nil {
		u = u.SetConfig(upd.Config).AddConfigRevision(1)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// 0 行有两种含义：行不存在（404）或 revision 不符（409）。用一次按 key 的
		// 读回区分——这不是竞态，两次之间行被删掉的极端情形会落到 ErrNotFound，
		// 与"没找到"的语义一致。
		//
		// 409 的文案带上 expected/actual 两个数字（§19-1）：运维看到"你基于 3、现在
		// 是 4"就能判断是不是别人刚改过，不必再翻一轮 GET。
		fresh, gerr := r.GetAccountByKey(ctx, key)
		if gerr != nil {
			if !errors.Is(gerr, ErrNotFound) {
				return nil, gerr
			}
			return nil, fmt.Errorf("%w: payment account key=%q", ErrNotFound, key)
		}
		return nil, fmt.Errorf("%w: payment account key=%q: expected config_revision=%d actual=%d",
			ErrConfigRevisionMismatch, key, upd.ExpectedConfigRevision, fresh.ConfigRevision)
	}
	return r.GetAccountByKey(ctx, key)
}

// SoftDeleteAccount **软停**账户（spec §10-3 / N30）：只置 enabled=false，行永不删除。
//
// key 是网关后台填过的回调 URL 段：物理删除后复用同名 key 会让**老回调打到新账户**
// （资损）。停用也不影响回调处理——钱可能已经收了，账本必须继续可读。
// 幂等：已停用仍返回 nil（200）；缺行 → ErrNotFound（404）。
func (r *PaymentAccountRepo) SoftDeleteAccount(ctx context.Context, key string) error {
	n, err := r.client.PaymentAccount.Update().
		Where(paymentaccount.KeyEQ(key)).
		SetEnabled(false).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: payment account key=%q", ErrNotFound, key)
	}
	return nil
}

// PaymentChannelRepo 支付渠道持久化（payment_channels）。按 `pay_type` 寻址的
// 子资源（spec §3-2：**不做整体替换**，否则改个展示名会删光渠道）。
type PaymentChannelRepo struct {
	client *ent.Client
}

// UpsertChannel 写入/覆盖单渠道（冲突键 `(account_id, pay_type)`）。
//
// `pay_type ∈ capabilities(provider)` 由 **service** 校验（repo 不引 provider）；
// 这里只做非空/归属与 DB 约束。account_id 指向不存在的账户 → FK 违约错误（服务层
// 先按 key 取账户，正常路径不会走到）。
func (r *PaymentChannelRepo) UpsertChannel(ctx context.Context, accountID int64, ch *domain.PaymentChannel) (*domain.PaymentChannel, error) {
	if ch == nil {
		return nil, fmt.Errorf("%w: nil payment channel", ErrInvalidInput)
	}
	if accountID <= 0 || ch.PayType == "" {
		return nil, fmt.Errorf("%w: payment channel requires account_id/pay_type", ErrInvalidInput)
	}
	now := time.Now()
	_, err := r.client.PaymentChannel.Create().
		SetAccountID(accountID).
		SetPayType(ch.PayType).
		SetEnabled(ch.Enabled).
		SetCurrency(ch.Currency).
		SetRateMicro(ch.RateMicro).
		SetMinUsd(ch.MinUSD).
		SetMaxUsd(ch.MaxUSD).
		OnConflictColumns(paymentchannel.FieldAccountID, paymentchannel.FieldPayType).
		Update(func(u *ent.PaymentChannelUpsert) {
			// created_at 不在更新面内（upsert 的 UPDATE 分支只碰这 6 列）：
			// 渠道的"首次上线时刻"不会因为改汇率/限额而被抹掉。
			u.SetEnabled(ch.Enabled).
				SetCurrency(ch.Currency).
				SetRateMicro(ch.RateMicro).
				SetMinUsd(ch.MinUSD).
				SetMaxUsd(ch.MaxUSD).
				SetUpdatedAt(now)
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.getChannel(ctx, accountID, ch.PayType)
}

// DeleteChannel 删除单渠道；**幂等**（渠道本就不存在 → nil，不算错误）。
//
// 账户不存在不在这里判（调用方先 GetAccountByKey → 404）；本方法只删匹配行。
func (r *PaymentChannelRepo) DeleteChannel(ctx context.Context, accountID int64, payType string) error {
	if accountID <= 0 || payType == "" {
		return fmt.Errorf("%w: payment channel requires account_id/pay_type", ErrInvalidInput)
	}
	_, err := r.client.PaymentChannel.Delete().
		Where(
			paymentchannel.AccountIDEQ(accountID),
			paymentchannel.PayTypeEQ(payType),
		).
		Exec(ctx)
	return err
}

// ListChannels 该账户的全部渠道（含 disabled），按 pay_type 升序（管理面稳定顺序）。
func (r *PaymentChannelRepo) ListChannels(ctx context.Context, accountID int64) ([]*domain.PaymentChannel, error) {
	rows, err := r.client.PaymentChannel.Query().
		Where(paymentchannel.AccountIDEQ(accountID)).
		Order(ent.Asc(paymentchannel.FieldPayType)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.PaymentChannel, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPaymentChannel(row))
	}
	return out, nil
}

// ListAllChannels 全量渠道行（含 disabled），按 `(account_id, pay_type)` 升序。
//
// 存在的唯一理由是**反 N+1**（§19-2 / P56）：选路与 `methods` 需要"所有账户的所有
// 渠道"，逐账户读在 5 个账户 × 4 个 pay_type 的规模上会放大成 30 条 SQL（
// `1 + N + P×(1+N)`）。调用方拿到全量后**在内存里按 account_id 分组**，过滤逻辑
// （§5-1 的 8 步）一行都不搬。
//
// 定序与 ListChannels 一致（`pay_type` 升序），让"同一 pay_type 取第一条"这类
// 既有语义在快照上仍然可复现（多行的形状只出现在 store 替身里，见 P56 的说明）。
func (r *PaymentChannelRepo) ListAllChannels(ctx context.Context) ([]*domain.PaymentChannel, error) {
	rows, err := r.client.PaymentChannel.Query().
		Order(ent.Asc(paymentchannel.FieldAccountID), ent.Asc(paymentchannel.FieldPayType)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.PaymentChannel, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPaymentChannel(row))
	}
	return out, nil
}

// getChannel 单渠道读回（upsert 后返回权威行；缺失 → ErrNotFound）。
func (r *PaymentChannelRepo) getChannel(ctx context.Context, accountID int64, payType string) (*domain.PaymentChannel, error) {
	row, err := r.client.PaymentChannel.Query().
		Where(
			paymentchannel.AccountIDEQ(accountID),
			paymentchannel.PayTypeEQ(payType),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: payment channel account_id=%d pay_type=%q", ErrNotFound, accountID, payType)
		}
		return nil, err
	}
	return toDomainPaymentChannel(row), nil
}

func toDomainPaymentAccount(row *ent.PaymentAccount) *domain.PaymentAccount {
	return &domain.PaymentAccount{
		ID:             row.ID,
		Key:            row.Key,
		Provider:       domain.ProviderName(row.Provider),
		MerchantRef:    row.MerchantRef,
		DisplayName:    row.DisplayName,
		Enabled:        row.Enabled,
		Priority:       row.Priority,
		Config:         row.Config,
		ConfigRevision: row.ConfigRevision,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func toDomainPaymentChannel(row *ent.PaymentChannel) *domain.PaymentChannel {
	return &domain.PaymentChannel{
		ID:        row.ID,
		AccountID: row.AccountID,
		PayType:   row.PayType,
		Enabled:   row.Enabled,
		Currency:  row.Currency,
		RateMicro: row.RateMicro,
		MinUSD:    row.MinUsd,
		MaxUSD:    row.MaxUsd,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// ---------- Repository 门面转发（service.Store 的 Payment 前缀面，§17-9） ----------
//
// service 只认这一组名字（Go 不能重载，且必须与上游账号的 CreateAccount /
// GetAccount 区分开）。转发保持**零语义**：所有不变量都在两个 repo 里，门面只
// 负责把调用送到对应的 repo 对象（Repository 仍是构造期的单一装配点）。

// CreatePaymentAccount 创建支付账户（key / (provider,merchant_ref) 唯一冲突 → ErrConflict）。
func (r *Repository) CreatePaymentAccount(ctx context.Context, a *domain.PaymentAccount) (*domain.PaymentAccount, error) {
	return r.PaymentAccounts.CreateAccount(ctx, a)
}

// GetPaymentAccountByKey 按 key 取支付账户（回调路径的唯一账户入口）。
func (r *Repository) GetPaymentAccountByKey(ctx context.Context, key string) (*domain.PaymentAccount, error) {
	return r.PaymentAccounts.GetAccountByKey(ctx, key)
}

// ListPaymentAccounts 全量支付账户（priority ASC, id ASC —— 与选路同序）。
func (r *Repository) ListPaymentAccounts(ctx context.Context) ([]*domain.PaymentAccount, error) {
	return r.PaymentAccounts.ListAccounts(ctx)
}

// UpdatePaymentAccount 更新可变更字段；config 变更时 config_revision++。
func (r *Repository) UpdatePaymentAccount(ctx context.Context, key string, upd AccountUpdate) (*domain.PaymentAccount, error) {
	return r.PaymentAccounts.UpdateAccount(ctx, key, upd)
}

// SoftDeletePaymentAccount 软停账户（enabled=false，行永不删除，幂等）。
func (r *Repository) SoftDeletePaymentAccount(ctx context.Context, key string) error {
	return r.PaymentAccounts.SoftDeleteAccount(ctx, key)
}

// UpsertPaymentChannel 按 (account_id, pay_type) upsert 单渠道。
func (r *Repository) UpsertPaymentChannel(ctx context.Context, accountID int64, ch *domain.PaymentChannel) (*domain.PaymentChannel, error) {
	return r.PaymentChannels.UpsertChannel(ctx, accountID, ch)
}

// DeletePaymentChannel 删除单渠道（幂等）。
func (r *Repository) DeletePaymentChannel(ctx context.Context, accountID int64, payType string) error {
	return r.PaymentChannels.DeleteChannel(ctx, accountID, payType)
}

// ListPaymentChannels 该账户的全部渠道（含 disabled，pay_type 升序）。
func (r *Repository) ListPaymentChannels(ctx context.Context, accountID int64) ([]*domain.PaymentChannel, error) {
	return r.PaymentChannels.ListChannels(ctx, accountID)
}

// ListAllPaymentChannels 全量渠道行（含 disabled；account_id, pay_type 升序）——
// **选路/`methods` 的唯一取数口**（§19-2 / P56：一次请求固定 2 条支付表查询）。
func (r *Repository) ListAllPaymentChannels(ctx context.Context) ([]*domain.PaymentChannel, error) {
	return r.PaymentChannels.ListAllChannels(ctx)
}
