// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_unit_test.go 支付编排的纯函数用例（无需 PG）：金额整数换算（§4-3
// 手算表）、溢出判据与生效上限（F14/F46）、重试判据（F38）、fail_reason 归因
// （§8）、raw_notify 清洗（B18/B35）、单号形状（§4-1）；末尾一组是
// `PaymentMethods` 的探针金额选法（§5-2 展示面 / §5-4 人工切流），只吃 store
// 替身，不需要 PG。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/settingssnap"
)

// TestQuoteMoney §4-3 的手算表：全整数，禁 float（M8 的判别点）。
func TestQuoteMoney(t *testing.T) {
	cases := []struct {
		usdCents int64
		rate     int64
		want     int64
	}{
		{1000, 7_200_000, 7200}, // 10 USD @7.2 → 7200 分
		{1, 7_200_000, 7},       // 0.01 USD @7.2 → 7 分
		{1, 3_777_777, 4},       // 0.01 USD @3.777777 → 4 分（P22：反算 1059≠1000）
		{1, 10_000, 0},          // 0.01 USD @0.01 → 0 分（B4 拒绝）
		{115, 7_200_000, 828},   // 1.15 USD @7.2 → 828 分（half-up）
		{115, 7_187_500, 827},   // 1.15 USD @7.1875 → (826.56+0.5)=827.06 → 827
	}
	for _, tc := range cases {
		got, err := quotePayMinor(quoteCreditMilli(tc.usdCents), tc.rate)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%d 分 @%d", tc.usdCents, tc.rate)
	}
	// credit_milli = usd_cents*1000（F13：差 10 倍是最容易犯的错）。
	require.Equal(t, int64(1000), quoteCreditMilli(1))
	require.Equal(t, int64(1_000_000), quoteCreditMilli(1000))

	// 汇率非正 → 不可表示（渠道不可用）。
	_, err := quotePayMinor(1000, 0)
	require.ErrorIs(t, err, errAmountOverflow)
	// 溢出判据：credit_milli 超过 (MaxInt64/2)/rate → 拒绝（不做乘法）。
	_, err = quotePayMinor(math.MaxInt64/2+1, 2)
	require.ErrorIs(t, err, errAmountOverflow)
}

// TestEffectiveMaxUSD F46：生效上限 = min(配置, (MaxInt64/2)/rate_micro/100000)。
func TestEffectiveMaxUSD(t *testing.T) {
	rate := int64(7_200_000)
	capByRate := (maxInt64 / 2) / rate / milliPerUSD
	require.Equal(t, capByRate, effectiveMaxUSD(10_000_000, rate),
		"配置 1e7 超出汇率可表示范围 → 收敛到算术上限（B19 的 400 依据）")
	require.Equal(t, int64(1000), effectiveMaxUSD(1000, rate), "配置低于算术上限 → 取配置值")
	require.Equal(t, int64(0), effectiveMaxUSD(1000, 0), "汇率非正 → 0（渠道不可用）")
	require.Equal(t, int64(0), effectiveMaxUSD(0, rate), "配置非正 → 0")
	// 极小汇率（rate_micro=1）时算术上限远大于配置值 → 仍取配置值（不误伤）。
	require.Equal(t, int64(10_000_000), effectiveMaxUSD(10_000_000, 1))
}

// TestParseUSDAmount B1：`^\d{1,7}(\.\d{1,2})?$` 且 > 0。
func TestParseUSDAmount(t *testing.T) {
	ok := map[string]int64{
		"1":       100,
		"0.01":    1,
		"1.15":    115,
		"010.00":  1000, // 前导零合法（整数等值比）
		"10.5":    1050,
		"9999999": 999_999_900,
	}
	for in, want := range ok {
		got, err := parseUSDAmount(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "0", "0.00", "-1", "1.234", "1e3", "1.", ".5", "1.2.3", "abc", "10000000", " 1"} {
		_, err := parseUSDAmount(bad)
		require.ErrorIs(t, err, ErrInvalidInput, bad)
	}
}

func TestFormatMinor(t *testing.T) {
	require.Equal(t, "72.00", formatMinor(7200))
	require.Equal(t, "1.15", formatMinor(115))
	require.Equal(t, "0.05", formatMinor(5))
	require.Equal(t, "0.00", formatMinor(0))
}

// TestRandomOrderNo §4-1：P{UTC yyyyMMddHHmmss}{10 位 base32}，且形状必须通过
// domain.ValidOutTradeNo（生成与校验同一套模式）。
func TestRandomOrderNo(t *testing.T) {
	now := time.Date(2026, 11, 12, 1, 1, 1, 0, time.UTC)
	seen := make(map[string]struct{}, 50)
	for range 50 {
		no, err := randomOrderNo(now)
		require.NoError(t, err)
		require.True(t, domain.ValidOutTradeNo(no), "单号形状非法: %s", no)
		require.Len(t, no, 25)
		seen[no] = struct{}{}
	}
	require.Greater(t, len(seen), 1, "crypto/rand 单号必须互异（同秒并发建单，B28）")
}

// TestIsRetryableTxErr F38：ErrConflict ∨ 40P01/55P03（其余 5xx 不重试）。
func TestIsRetryableTxErr(t *testing.T) {
	require.False(t, isRetryableTxErr(nil))
	require.False(t, isRetryableTxErr(errors.New("boom")))
	require.True(t, isRetryableTxErr(fmt.Errorf("%w: code unique", repository.ErrConflict)))
	require.True(t, isRetryableTxErr(&pgconn.PgError{Code: "40P01"}))
	require.True(t, isRetryableTxErr(fmt.Errorf("tx: %w", &pgconn.PgError{Code: "55P03"})))
	require.False(t, isRetryableTxErr(&pgconn.PgError{Code: "23505"}), "唯一约束裸错误不在判据内")
}

// TestOrderFailReason §8：每个出口的 fail_reason 归因；未知单号不写审计。
func TestOrderFailReason(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("%w: P1", domain.ErrOrderNotFound), ""}, // 未知单号：不写
		{fmt.Errorf("%w: x", ErrProviderMismatch), domain.OrderFailReasonProvider},
		{fmt.Errorf("%w: x", ErrAmountMismatch), domain.OrderFailReasonAmount},
		{fmt.Errorf("%w: x", ErrOrderClosed), domain.OrderFailReasonClosed},
		{fmt.Errorf("%w: x", ErrStatusNotPaid), domain.OrderFailReasonStatus},
		{fmt.Errorf("%w: user 1", domain.ErrUserUnavailable), domain.OrderFailReasonUser},
		{fmt.Errorf("%w: x", repository.ErrConflict), domain.OrderFailReasonInternal},
		{fmt.Errorf("%w: x", domain.ErrInternal), domain.OrderFailReasonInternal},
		{fmt.Errorf("%w: x", ErrUpstream), domain.OrderFailReasonInternal},
		{errors.New("unknown"), ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, OrderFailReason(tc.err), "%v", tc.err)
	}
}

// TestSanitizeRawNotify B18/B35：NUL 与非法 UTF-8 清洗后仍入账；超长截断为
// ≤4 KiB 且**仍是合法 JSON**（审计可解析），多字节字符不被切断。
func TestSanitizeRawNotify(t *testing.T) {
	require.Nil(t, SanitizeRawNotify(nil), "空载荷不写该列")

	got := SanitizeRawNotify(map[string]string{
		"trade_no": "GW\x00-9",
		"buyer":    "张三",
		"bin":      string([]byte{0xff, 0xfe, 'a'}),
	})
	require.NotNil(t, got)
	require.NotContains(t, *got, "\x00")
	require.True(t, json.Valid([]byte(*got)), "清洗后必须是合法 JSON: %s", *got)
	require.Contains(t, *got, "张三")

	// 超长（多字节）：≤4 KiB、合法 UTF-8、合法 JSON。
	payload := map[string]string{}
	for i := range 400 {
		payload[fmt.Sprintf("k%03d", i)] = "充值订单-" + fmt.Sprintf("%03d", i)
	}
	got = SanitizeRawNotify(payload)
	require.NotNil(t, got)
	require.LessOrEqual(t, len(*got), rawNotifyMaxBytes, "raw_notify 必须 ≤4 KiB")
	require.True(t, json.Valid([]byte(*got)), "截断后必须仍是合法 JSON")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(*got), &decoded))
	require.Equal(t, true, decoded["truncated"], "超长时带 truncated 标记")
}

// TestSanitizeRawNotifyKeepsShortPayload 未超长时原样保留业务字段（不包装）。
func TestSanitizeRawNotifyKeepsShortPayload(t *testing.T) {
	got := SanitizeRawNotify(map[string]string{"trade_no": "GW-9", "money": "1.15"})
	require.NotNil(t, got)
	var decoded map[string]string
	require.NoError(t, json.Unmarshal([]byte(*got), &decoded))
	require.Equal(t, "GW-9", decoded["trade_no"])
	require.Equal(t, "1.15", decoded["money"])
}

// TestFakeOrderTxSemantics 钉**测试替身**的事务语义（brief B1 第 0 步）：行锁读单与
// 条件迁移先落暂存——提交才可见、回滚整体丢弃。三者不成立时 P14/P4b/P25 的回滚与
// 重试断言都是假的。
func TestFakeOrderTxSemantics(t *testing.T) {
	ctx := context.Background()
	newOrder := func(t *testing.T, fs *fakeStore, no string) *domain.Order {
		t.Helper()
		o, err := fs.CreateOrder(ctx, &domain.Order{
			OutTradeNo: no, UserID: 7, Provider: domain.ProviderEpayV2,
			Status: domain.OrderStatusPending, CreditMilli: 16_000, PayMinor: 115,
			Currency: "CNY", PayType: "alipay",
		})
		require.NoError(t, err)
		return o
	}

	t.Run("commit marks paid", func(t *testing.T) {
		fs := newFakeStore()
		o := newOrder(t, fs, "P20261112010101ABCDEFGHJK")
		raw := `{"trade_no":"GW-9"}`
		require.NoError(t, fs.WithTx(ctx, func(tx repository.TxStore) error {
			locked, err := tx.LockOrderByOutTradeNo(ctx, o.OutTradeNo)
			require.NoError(t, err)
			require.Equal(t, domain.OrderStatusPending, locked.Status)
			return tx.MarkOrderPaid(ctx, locked.ID, 42, "GW-9", &raw)
		}))
		got, err := fs.GetOrderByID(ctx, o.ID)
		require.NoError(t, err)
		require.Equal(t, domain.OrderStatusPaid, got.Status)
		require.NotNil(t, got.RedemptionCodeID)
		require.Equal(t, int64(42), *got.RedemptionCodeID)
		require.NotNil(t, got.PaidAt)
	})

	t.Run("rollback discards mark paid", func(t *testing.T) {
		fs := newFakeStore()
		o := newOrder(t, fs, "P20261112010101ZZZZZZZZZZ")
		boom := errors.New("boom")
		err := fs.WithTx(ctx, func(tx repository.TxStore) error {
			locked, err := tx.LockOrderByOutTradeNo(ctx, o.OutTradeNo)
			require.NoError(t, err)
			require.NoError(t, tx.MarkOrderPaid(ctx, locked.ID, 43, "GW-10", nil))
			return boom
		})
		require.ErrorIs(t, err, boom)
		got, err := fs.GetOrderByID(ctx, o.ID)
		require.NoError(t, err)
		require.Equal(t, domain.OrderStatusPending, got.Status, "回滚后必须仍 pending")
		require.Nil(t, got.RedemptionCodeID)
	})

	// P4b 的 fake 面：MarkOrderPaid 的条件 `WHERE status='pending'` 命中 0 行 → ErrConflict。
	t.Run("mark paid on closed order conflicts", func(t *testing.T) {
		fs := newFakeStore()
		o := newOrder(t, fs, "P20261112010101YYYYYYYYYY")
		closed, err := fs.CloseOrder(ctx, o.ID, nil)
		require.NoError(t, err)
		require.True(t, closed)
		err = fs.WithTx(ctx, func(tx repository.TxStore) error {
			return tx.MarkOrderPaid(ctx, o.ID, 44, "GW-11", nil)
		})
		require.ErrorIs(t, err, repository.ErrConflict)
	})

	// P25 的注入面：orderErrs 按 FIFO 消费，跨多次事务重试仍有效（2 次 40P01 → 第 3 次成功）。
	t.Run("orderErrs fifo injection", func(t *testing.T) {
		fs := newFakeStore()
		o := newOrder(t, fs, "P20261112010101XXXXXXXXXX")
		fs.orderErrs = newErrQueue(
			&pgconn.PgError{Code: "40P01"},
			&pgconn.PgError{Code: "40P01"},
		)
		for attempt := 1; attempt <= 3; attempt++ {
			err := fs.WithTx(ctx, func(tx repository.TxStore) error {
				_, err := tx.LockOrderByOutTradeNo(ctx, o.OutTradeNo)
				return err
			})
			if attempt <= 2 {
				require.True(t, isRetryableTxErr(err), "第 %d 次应为可重试死锁", attempt)
				continue
			}
			require.NoError(t, err, "第 3 次（队列耗尽）应成功")
		}
	})
}

// ---------- 可售方式列表的探针金额（§5-2 展示面 / §5-4 人工切流） ----------
//
// 本组用例钉住 `PaymentMethods` 的**探针金额选法**。下限不是"所有渠道行的最小
// min_usd"（旧写法），而是"升序去重后的每个 min_usd 逐个试 ResolveChannel，第一
// 个能卖出去的就是答案"。旧写法在 §5-4 的人工切流下会把支付方式卖没了：
//
//	① 主账户 alipay min_usd=1（管理员刚按 spec 软停：enabled=false）
//	② 备用账户 alipay min_usd=10（enabled=true）
//	③ 探针金额 = "全部行的最小 min_usd" = 1
//	④ ResolveChannel("alipay", 1) 跳过停用主账户，备用要求 ≥10 → ErrNoEligibleAccount
//	⑤ continue → alipay 整个从 methods 消失（备用明明可卖 ≥10）
//
// 所以判别点是"投影里的 MinUSD/Provider 来自哪个账户"，而不是"列表非空"。

const (
	testPayTypeAlipay = "alipay"
	testPayTypeWxpay  = "wxpay"
	// 渠道行的公共形状：CNY @7.2、上限 1000 USD（边界不在本组用例的判据内）。
	testMethodsRateMicro = int64(7_200_000)
	testMethodsMaxUSD    = int64(1_000)
)

// paymentMethodsChannelStore 只替换账户/渠道两个读口：本组用例要直接控制
// enabled/min_usd 的形状（(account_id, pay_type) 在真实 schema 上是 UNIQUE，
// 停用账户/停用渠道的 min_usd 组合也只能这样喂进来）。
type paymentMethodsChannelStore struct {
	*fakeStore
	accounts []*domain.PaymentAccount
	channels map[int64][]*domain.PaymentChannel
}

func (s *paymentMethodsChannelStore) ListPaymentAccounts(context.Context) ([]*domain.PaymentAccount, error) {
	return append([]*domain.PaymentAccount(nil), s.accounts...), nil
}

func (s *paymentMethodsChannelStore) ListPaymentChannels(_ context.Context, accountID int64) ([]*domain.PaymentChannel, error) {
	return append([]*domain.PaymentChannel(nil), s.channels[accountID]...), nil
}

// paymentMethodsCfg 测试凭据：本组用例只判探针金额的选法，凭据形状与签名无关
// （epay 的凭据校验另有专门用例），故 ParseConfig 恒成功。
type paymentMethodsCfg struct {
	domain.AccountConfigSeal
	provider domain.ProviderName
}

func (c paymentMethodsCfg) Provider() domain.ProviderName { return c.provider }
func (c paymentMethodsCfg) MerchantRef() string           { return "merchant:" + string(c.provider) }

// paymentMethodsProvider 测试协议实现：一条都不联网。
type paymentMethodsProvider struct{ name domain.ProviderName }

func (p paymentMethodsProvider) Name() domain.ProviderName { return p.name }
func (p paymentMethodsProvider) Capabilities() []string {
	return []string{testPayTypeAlipay, testPayTypeWxpay}
}
func (p paymentMethodsProvider) ConfigSchema() domain.ConfigSchema { return domain.ConfigSchema{} }
func (p paymentMethodsProvider) ParseConfig([]byte) (domain.AccountConfig, error) {
	return paymentMethodsCfg{provider: p.name}, nil
}
func (p paymentMethodsProvider) BuildPayURL(domain.AccountConfig, domain.PayRequest) (string, error) {
	return "", errors.New("payment methods test: BuildPayURL must not be called")
}
func (p paymentMethodsProvider) VerifyNotify(domain.AccountConfig, url.Values) (domain.NotifyEvent, error) {
	return domain.NotifyEvent{}, errors.New("payment methods test: VerifyNotify must not be called")
}

func testMethodsChannel(accountID int64, payType string, enabled bool, minUSD int64) *domain.PaymentChannel {
	return &domain.PaymentChannel{
		AccountID: accountID, PayType: payType, Enabled: enabled,
		Currency: "CNY", RateMicro: testMethodsRateMicro, MinUSD: minUSD, MaxUSD: testMethodsMaxUSD,
	}
}

// newPaymentMethodsSvc 构造只走展示/选路面的 Service：`payment.enabled=true`，
// 协议注册表按账户实际用到的 provider 自动装齐（每个账户一个独立 provider，
// 这样断言 Provider 就能指出投影来自哪个账户）。
func newPaymentMethodsSvc(t *testing.T, accounts []*domain.PaymentAccount, channels map[int64][]*domain.PaymentChannel) *Service {
	t.Helper()
	ctx := context.Background()
	fs := newFakeStore()
	store := &paymentMethodsChannelStore{fakeStore: fs, accounts: accounts, channels: channels}
	_, err := fs.SetSetting(ctx, domain.SettingPaymentEnabled, domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	snap := settingssnap.New(store, nil)
	require.NoError(t, snap.Load(ctx))

	providers := make([]domain.PaymentProvider, 0, len(accounts))
	seen := make(map[domain.ProviderName]bool, len(accounts))
	for _, acct := range accounts {
		if seen[acct.Provider] {
			continue
		}
		seen[acct.Provider] = true
		providers = append(providers, paymentMethodsProvider{name: acct.Provider})
	}
	registry, err := domain.NewProviderRegistry(providers)
	require.NoError(t, err)
	svc := &Service{
		store:           store,
		inv:             &invRecorder{},
		settings:        snap,
		paymentCfgs:     make(map[string]cachedPaymentCfg, len(providers)),
		paymentRegistry: registry,
	}
	return svc
}

// findPaymentMethod 取该 pay_type 的唯一一行（0 行或多行都直接失败）。
func findPaymentMethod(t *testing.T, methods []PaymentMethod, payType string) PaymentMethod {
	t.Helper()
	found := make([]PaymentMethod, 0, 1)
	for _, m := range methods {
		if m.PayType == payType {
			found = append(found, m)
		}
	}
	require.Len(t, found, 1, "pay_type %q 应恰好出现一行", payType)
	return found[0]
}

// requireNoPaymentMethod 反向判据：不可售的方式**不得**出现（守住"不误报"）。
func requireNoPaymentMethod(t *testing.T, methods []PaymentMethod, payType string) {
	t.Helper()
	for _, m := range methods {
		require.NotEqual(t, payType, m.PayType, "不可售的 pay_type 不得出现在列表里")
	}
}

// TestPaymentMethodsFallbackAfterPrimaryDisabled §5-4 的核心回归：管理员按 spec 的
// 人工切流手段把主账户 enabled=false 之后，备用账户必须**立刻**接管展示。
//
// 旧写法上必红：探针金额取"全部行的最小 min_usd" = 1（来自已软停的主账户），
// ResolveChannel("alipay", 1) 跳过主账户、备用要求 ≥10 → ErrNoEligibleAccount
// → continue → alipay 整个消失。照 spec 切流反而把支付方式卖没了。
func TestPaymentMethodsFallbackAfterPrimaryDisabled(t *testing.T) {
	accounts := []*domain.PaymentAccount{
		{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: false, Priority: 0},
		{ID: 2, Key: "epay_backup", Provider: "epay_backup_proto", Enabled: true, Priority: 1},
	}
	channels := map[int64][]*domain.PaymentChannel{
		1: {testMethodsChannel(1, testPayTypeAlipay, true, 1)},
		2: {testMethodsChannel(2, testPayTypeAlipay, true, 10)},
	}
	methods, err := newPaymentMethodsSvc(t, accounts, channels).PaymentMethods(context.Background())
	require.NoError(t, err)

	got := findPaymentMethod(t, methods, testPayTypeAlipay)
	require.Equal(t, int64(10), got.MinUSD,
		"下限必须来自**可用**的备用账户（1 只挂在已软停的主账户上）")
	require.Equal(t, domain.ProviderName("epay_backup_proto"), got.Provider,
		"投影必须来自备用账户")
	require.Equal(t, "CNY", got.Currency)
	require.Equal(t, testMethodsMaxUSD, got.MaxUSD)
	require.Len(t, methods, 1, "同一 pay_type 只出一行")
}

// TestPaymentMethodsDisabledChannelDoesNotMask 停用渠道行的 min_usd 不得成为**唯一**
// 探针金额（旧写法把它统计进来，探到 1 就以为整个方式不可卖）。
//
// 两个形状：
//   - same_account：同账户同 pay_type 两行（启用 min=10 / 停用 min=1）。真实 schema
//     上 (account_id, pay_type) 是 UNIQUE，这个形状只能在 store 替身上构造；它直接
//     钉住"停用行的 min_usd 也进了候选集"。注意选路 §5-1 ④ 只看第一条匹配行，
//     所以启用的那行必须排在前面。
//   - cross_account：可达的生产形状（A 的 alipay 停用 min=1，B 的 alipay 启用 min=10）。
func TestPaymentMethodsDisabledChannelDoesNotMask(t *testing.T) {
	t.Run("same_account", func(t *testing.T) {
		accounts := []*domain.PaymentAccount{
			{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: true, Priority: 0},
		}
		channels := map[int64][]*domain.PaymentChannel{
			1: {
				testMethodsChannel(1, testPayTypeAlipay, true, 10),
				testMethodsChannel(1, testPayTypeAlipay, false, 1),
			},
		}
		methods, err := newPaymentMethodsSvc(t, accounts, channels).PaymentMethods(context.Background())
		require.NoError(t, err)

		got := findPaymentMethod(t, methods, testPayTypeAlipay)
		require.Equal(t, int64(10), got.MinUSD, "下限必须来自启用的那行（1 只挂在停用渠道上）")
		require.Equal(t, domain.ProviderName("epay_main_proto"), got.Provider)
	})

	t.Run("cross_account", func(t *testing.T) {
		accounts := []*domain.PaymentAccount{
			{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: true, Priority: 0},
			{ID: 2, Key: "epay_backup", Provider: "epay_backup_proto", Enabled: true, Priority: 1},
		}
		channels := map[int64][]*domain.PaymentChannel{
			1: {testMethodsChannel(1, testPayTypeAlipay, false, 1)},
			2: {testMethodsChannel(2, testPayTypeAlipay, true, 10)},
		}
		methods, err := newPaymentMethodsSvc(t, accounts, channels).PaymentMethods(context.Background())
		require.NoError(t, err)

		got := findPaymentMethod(t, methods, testPayTypeAlipay)
		require.Equal(t, int64(10), got.MinUSD, "下限必须来自**启用**的渠道（1 只挂在停用渠道上）")
		require.Equal(t, domain.ProviderName("epay_backup_proto"), got.Provider,
			"投影必须来自启用了该渠道的账户")
	})
}

// TestPaymentMethodsOmitsUnavailable 反向判据：确实不可售时**不得**出现。
// 这条守住"不误报"的一侧，防止修过头变成"永远出现"。
func TestPaymentMethodsOmitsUnavailable(t *testing.T) {
	cases := []struct {
		name     string
		accounts []*domain.PaymentAccount
		channels map[int64][]*domain.PaymentChannel
	}{
		{
			name: "only disabled account",
			accounts: []*domain.PaymentAccount{
				{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: false, Priority: 0},
			},
			channels: map[int64][]*domain.PaymentChannel{
				1: {testMethodsChannel(1, testPayTypeAlipay, true, 5)},
			},
		},
		{
			name: "only disabled channel",
			accounts: []*domain.PaymentAccount{
				{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: true, Priority: 0},
			},
			channels: map[int64][]*domain.PaymentChannel{
				1: {testMethodsChannel(1, testPayTypeAlipay, false, 5)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			methods, err := newPaymentMethodsSvc(t, tc.accounts, tc.channels).PaymentMethods(context.Background())
			require.NoError(t, err)
			requireNoPaymentMethod(t, methods, testPayTypeAlipay)
			require.Empty(t, methods, "没有任何可售方式时返回空列表（不是错误）")
		})
	}
}

// TestResolveChannelClampsOverflowingMaxUSD §7：max_usd 配得比可表示范围大时，
// 生效上限收敛后必须**仍然可售**（不是整条渠道失效）；且收敛只发生在比较处，
// 不改写行数据（effectiveMaxUSD 由调用方算，Channel.MaxUSD 仍是配置原值）。
//
// 两张 max_usd 钉住两个不同的失效面（`max_usd*100` 是 int64 回绕，符号取决于
// 模 2^64 的余数，不是"只要大于 MaxInt64/100 就必为负"）：
//   - 1e17：乘积回绕成**负数**（-8446744073709551616）→ 裸比较 `amountCents >
//     负数` 恒真 → 渠道被跳过 → 未修版本上必红（ErrNoEligibleAccount）。
//   - 1e18：乘积回绕后仍是**正数**（7766279631452241920）→ 单靠它不会红；
//     它钉住另一侧：收敛不得落到行数据上。
func TestResolveChannelClampsOverflowingMaxUSD(t *testing.T) {
	cases := []struct {
		name   string
		maxUSD int64
	}{
		{name: "product wraps negative", maxUSD: 100_000_000_000_000_000},
		{name: "product wraps positive", maxUSD: 1_000_000_000_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accounts := []*domain.PaymentAccount{
				{ID: 1, Key: "epay_main", Provider: "epay_main_proto", Enabled: true, Priority: 0},
			}
			ch := testMethodsChannel(1, testPayTypeAlipay, true, 1)
			ch.MaxUSD = tc.maxUSD
			channels := map[int64][]*domain.PaymentChannel{1: {ch}}

			res, err := newPaymentMethodsSvc(t, accounts, channels).
				ResolveChannel(context.Background(), testPayTypeAlipay, 1)
			require.NoError(t, err, "生效上限收敛后必须可售（不是 ErrNoEligibleAccount）")
			require.Equal(t, tc.maxUSD, res.Channel.MaxUSD,
				"收敛发生在比较处，不得改写行数据")
		})
	}
}
