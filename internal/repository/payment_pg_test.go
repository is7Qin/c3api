// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 支付账户 / 渠道的真实 PostgreSQL 面（spec 支付网关 v2 §3-1 / §3-2 / §10-3）。
//
// 基座与 redemption_repo_test.go 同款：newPGReposShared → pgtest.Clone，未设
// TEST_DATABASE_URL 时 envOrSkip 直接 t.Skip（skip ≠ green）。
//
// 为什么这些用例必须在真库上跑：D 批暴露的两个缺口都落在**repo 层的 SQL/索引**
// 上，而既有 P 用例走 fake store，压根不执行那段 SQL——
//   - M28「DELETE 物理删除行」：只有真库能判 `SoftDeleteAccount` 是软停还是删行；
//   - `(provider, merchant_ref)` 唯一：只有真库有那条复合唯一索引；
//   - `ON DELETE CASCADE`：只有真库有那条外键。

// pgPaymentAccount 建一条可插入账户的参数。config 必须是合法 jsonb（列 NOT NULL
// 无默认值，nil 会直接写 NULL 报错）；merchant_ref 与 config 的 pid 保持一致
// （真实路径上它由 protocol.ParseConfig 推导）。
func pgPaymentAccount(key, merchantRef string) *domain.PaymentAccount {
	return &domain.PaymentAccount{
		Key:         key,
		Provider:    domain.ProviderEpayV2,
		MerchantRef: merchantRef,
		DisplayName: key,
		Enabled:     true,
		Priority:    0,
		Config:      json.RawMessage(`{"pid":"` + merchantRef + `"}`),
	}
}

// TestPaymentAccountSoftDelete_PG `SoftDeleteAccount` 是**软停**：行必须仍在且
// enabled=false（M28 判定点 / P36 / B46）。物理删除会让 GetAccountByKey 直接
// ErrNotFound → 本用例红。
func TestPaymentAccountSoftDelete_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	created, err := repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_soft_stop", "mch-soft-stop"))
	require.NoError(t, err)
	require.True(t, created.Enabled)
	require.EqualValues(t, 1, created.ConfigRevision, "创建即 config_revision=1")

	require.NoError(t, repos.SoftDeletePaymentAccount(ctx, "pg_soft_stop"))

	// M28 判定点。
	got, err := repos.GetPaymentAccountByKey(ctx, "pg_soft_stop")
	require.NoError(t, err, "软停 = 行必须仍在（M28 / P36 / B46）")
	require.Equal(t, created.ID, got.ID, "同一行（不是删了再建）")
	require.False(t, got.Enabled, "软停只置 enabled=false")

	// 重复 DELETE 幂等（P36：两次都是 200，第二次不是 404）。
	require.NoError(t, repos.SoftDeletePaymentAccount(ctx, "pg_soft_stop"), "重复软停幂等")

	// 「行仍在」的直接证据（GET 走 key 唯一索引，行数走表）。
	require.EqualValues(t, 1, pgCount(t, pgSharedPool(t),
		`SELECT COUNT(*) FROM payment_accounts WHERE key = $1`, "pg_soft_stop"),
		"软停不减少行数")

	// 软停不碰只读三件套与配置代际。
	require.Equal(t, domain.ProviderEpayV2, got.Provider)
	require.Equal(t, "mch-soft-stop", got.MerchantRef)
	require.EqualValues(t, 1, got.ConfigRevision)

	// 缺失 key → ErrNotFound（404），不是静默成功。
	require.ErrorIs(t, repos.SoftDeletePaymentAccount(ctx, "pg_soft_missing"), repository.ErrNotFound)
}

// TestPaymentAccountMerchantRefUnique_PG `(provider, merchant_ref)` 复合唯一：
// 同协议同商户建第二个账户 → ErrConflict（B49 / P40）；换协议则放行。
func TestPaymentAccountMerchantRefUnique_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	first, err := repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_uniq_first", "mch-uniq"))
	require.NoError(t, err)

	// 同 (provider, merchant_ref)、不同 key → 唯一索引命中（service 映射 409）。
	_, err = repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_uniq_second", "mch-uniq"))
	require.ErrorIs(t, err, repository.ErrConflict, "(provider,merchant_ref) UNIQUE（B49/P40）")
	require.Contains(t, err.Error(), `merchant_ref="mch-uniq"`, "冲突详情含冲突值")

	_, err = repos.GetPaymentAccountByKey(ctx, "pg_uniq_second")
	require.ErrorIs(t, err, repository.ErrNotFound, "冲突行不得入库")

	// 索引是**复合**的：别的协议用同一 merchant_ref 必须放行（否则多协议共存被
	// 误锁——这正是 N5/N28 要的语义：同一协议下同一商户只能录一次）。
	otherProto := pgPaymentAccount("pg_uniq_other_proto", "mch-uniq")
	otherProto.Provider = domain.ProviderName("pg_test_proto")
	_, err = repos.CreatePaymentAccount(ctx, otherProto)
	require.NoError(t, err, "(provider,merchant_ref) 是复合唯一，不是 merchant_ref 单独唯一")

	// 第一行不受影响。
	got, err := repos.GetPaymentAccountByKey(ctx, "pg_uniq_first")
	require.NoError(t, err)
	require.Equal(t, first.ID, got.ID)
}

// TestPaymentAccountUpdateMerchantRefImmutable_PG `UpdateAccount` 的 merchant_ref
// 不可变（N29 / N4 / B56 / P45），以及「config 变更才推进 config_revision」。
//
// 注：repo 只在**带 config 的更新**里比对 merchant_ref（不带 config 的更新压根
// 不碰凭据，MerchantRef 字段被忽略），所以换商户号的用例必须同时带 config——
// 这正是真实调用方的形状（改 pid 只能发生在改 config 时）。
func TestPaymentAccountUpdateMerchantRefImmutable_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	created, err := repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_upd_immutable", "mch-upd-1"))
	require.NoError(t, err)

	// 改商户号（config 里的 pid 跟着变）→ 必须被拒。
	_, err = repos.UpdatePaymentAccount(ctx, "pg_upd_immutable", repository.AccountUpdate{
		Config:      json.RawMessage(`{"pid":"mch-upd-2"}`),
		MerchantRef: "mch-upd-2",
	})
	require.ErrorIs(t, err, repository.ErrMerchantRefImmutable, "merchant_ref 只读（spec §10-3）")
	require.ErrorIs(t, err, repository.ErrConflict, "细分哨兵仍归 ErrConflict 族（errors.Is 两者为真）")

	cur, err := repos.GetPaymentAccountByKey(ctx, "pg_upd_immutable")
	require.NoError(t, err)
	require.Equal(t, "mch-upd-1", cur.MerchantRef, "被拒后 merchant_ref 不变")
	require.JSONEq(t, `{"pid":"mch-upd-1"}`, string(cur.Config), "被拒后 config 不变")
	require.EqualValues(t, created.ConfigRevision, cur.ConfigRevision, "被拒后 revision 不推进")

	// 不改 merchant_ref 的 config 更新 → 成功且 config_revision 递增（缓存键变化）。
	updated, err := repos.UpdatePaymentAccount(ctx, "pg_upd_immutable", repository.AccountUpdate{
		Config:      json.RawMessage(`{"pid":"mch-upd-1","fee_rate":"0.6"}`),
		MerchantRef: "mch-upd-1",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"pid":"mch-upd-1","fee_rate":"0.6"}`, string(updated.Config))
	require.EqualValues(t, created.ConfigRevision+1, updated.ConfigRevision, "config 变更 → revision++")

	// 只改 enabled（Config == nil）→ 不动 revision（B55 / P44 的同款不变量）。
	disabled := false
	toggled, err := repos.UpdatePaymentAccount(ctx, "pg_upd_immutable", repository.AccountUpdate{Enabled: &disabled})
	require.NoError(t, err)
	require.False(t, toggled.Enabled)
	require.EqualValues(t, updated.ConfigRevision, toggled.ConfigRevision, "非 config 更新不推进 revision")
	require.Equal(t, "mch-upd-1", toggled.MerchantRef)
}

// TestPaymentChannelCascadeOnAccountRowDelete_PG 渠道是账户的子资源：
// 账户行被物理删除时渠道随之消失，不留孤儿（spec §3-2 的 ON DELETE CASCADE）。
//
// 正常路径没有「物理删账户」接口（DELETE 恒为软停，N30 / §10-3）；这里直接用
// SQL 删账户行，模拟 spec 点名的 DBA 手工清理，验的是**外键动作**本身。
func TestPaymentChannelCascadeOnAccountRowDelete_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	acc, err := repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_cascade", "mch-cascade"))
	require.NoError(t, err)

	for _, payType := range []string{"alipay", "wxpay"} {
		_, err := repos.UpsertPaymentChannel(ctx, acc.ID, &domain.PaymentChannel{
			PayType: payType, Enabled: true, Currency: "CNY",
			RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 100,
		})
		require.NoError(t, err, "pay_type=%s", payType)
	}
	chans, err := repos.ListPaymentChannels(ctx, acc.ID)
	require.NoError(t, err)
	require.Len(t, chans, 2)

	pool := pgSharedPool(t)
	tag, err := pool.Exec(ctx, `DELETE FROM payment_accounts WHERE id = $1`, acc.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "账户行被物理删除")

	chans, err = repos.ListPaymentChannels(ctx, acc.ID)
	require.NoError(t, err)
	require.Empty(t, chans, "账户行物理删除后渠道随之消失（ON DELETE CASCADE）")
	require.EqualValues(t, 0, pgCount(t, pool,
		`SELECT COUNT(*) FROM payment_channels WHERE account_id = $1`, acc.ID),
		"不留孤儿渠道行")

	_, err = repos.GetPaymentAccountByKey(ctx, "pg_cascade")
	require.ErrorIs(t, err, repository.ErrNotFound, "账户行确实已删除（与前一条软停用例互为对照）")
}

// TestPaymentAccountUpdateRevisionCAS_PG P57 / §19-1：带 `expected_config_revision`
// 的更新是**比较并交换**（`WHERE key=? AND config_revision=?`）。
//
// 为什么必须在真库上跑：CAS 的全部语义都在那条 WHERE 里——fake store 里的比较是
// 另一份实现，钉不住"服务端真的把版本条件放进了 UPDATE"（M40 的杀手）。
func TestPaymentAccountUpdateRevisionCAS_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	created, err := repos.CreatePaymentAccount(ctx, pgPaymentAccount("pg_cas", "mch-cas"))
	require.NoError(t, err)
	require.EqualValues(t, 1, created.ConfigRevision, "创建即 revision 1")

	// ① 命中：expected=1 → 生效，revision 1→2。
	cfgV2 := json.RawMessage(`{"pid":"mch-cas","nonce":"v2"}`)
	upd, err := repos.UpdatePaymentAccount(ctx, "pg_cas", repository.AccountUpdate{
		Config: cfgV2, MerchantRef: "mch-cas", ExpectedConfigRevision: 1,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, upd.ConfigRevision)

	// ② 过期：另一份同样基于 revision 1 的写 → 409 族，且**行未被改动**。
	_, err = repos.UpdatePaymentAccount(ctx, "pg_cas", repository.AccountUpdate{
		Config: json.RawMessage(`{"pid":"mch-cas","nonce":"stale"}`), MerchantRef: "mch-cas",
		ExpectedConfigRevision: 1,
	})
	require.ErrorIs(t, err, repository.ErrConfigRevisionMismatch, "过期版本必须被拒（P57）")
	require.ErrorIs(t, err, repository.ErrConflict, "细分哨兵必须仍是 ErrConflict（→ 409）")
	require.Contains(t, err.Error(), "expected config_revision=1", "409 文案带期望版本")
	require.Contains(t, err.Error(), "actual=2", "409 文案带当前版本（运维据此判断是不是别人刚改过）")

	got, err := repos.GetPaymentAccountByKey(ctx, "pg_cas")
	require.NoError(t, err)
	require.EqualValues(t, 2, got.ConfigRevision, "被拒的更新不得推进 revision")
	require.JSONEq(t, string(cfgV2), string(got.Config), "被拒的更新不得改动 config（密钥回滚的判据）")

	// 只改元数据 + 旧 revision：revision 未变 ⇒ 条件仍成立，不得误报 409。
	name := "renamed"
	_, err = repos.UpdatePaymentAccount(ctx, "pg_cas", repository.AccountUpdate{
		DisplayName: &name, ExpectedConfigRevision: 2,
	})
	require.NoError(t, err, "只改展示名时 revision 不变，CAS 必须放行（不逼前端刷新整表）")

	// ③ expected=0 = 不校验（r12 行为，脚本兼容口径）。
	third, err := repos.UpdatePaymentAccount(ctx, "pg_cas", repository.AccountUpdate{
		Config: json.RawMessage(`{"pid":"mch-cas","nonce":"v3"}`), MerchantRef: "mch-cas",
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, third.ConfigRevision)

	// ④ 缺行 + expected → 404（不是 409）：0 行有两种含义，必须能区分。
	enabled := true
	_, err = repos.UpdatePaymentAccount(ctx, "pg_missing", repository.AccountUpdate{
		Enabled: &enabled, ExpectedConfigRevision: 1,
	})
	require.ErrorIs(t, err, repository.ErrNotFound, "缺行是 404，不是并发冲突")
}
