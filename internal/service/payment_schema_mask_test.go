// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_schema_mask_test.go P47：**schema 是秘密字段的唯一声明口**。
//
// 判据的形态由 fixture 决定：`configurableProvider.schema` 是**可注入数据**，测试
// 只改这一个字段（不换 provider、不换 service、不重注册表），掩码与 `"***"` 合并
// 就必须跟着变。若 service 里还留着一份写死的字段名表，这四条断言会同时失效——
// 这正是"fake 返回什么 service 就用什么"的假绿与真绿的分界。
//
// 反例记录：把 `maskSecretFields` 的入参换成 `[]string{"private_key"}`（M35），
// ①③④ 立刻红；把 `ConfigSchema()` 换成别的字段（②把 `token` 标回公开）而不改
// service 时，①②之间必须出现可见差异，否则说明掩码根本不看 schema。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

const (
	// schemaMaskProviderName 假协议名（注册表键；与任何真实协议无关）。
	schemaMaskProviderName = "schema_mask_proto"
	// schemaMaskAccountKey 被种下的账户 key（P47 只读/写这一行）。
	schemaMaskAccountKey = "mask_probe"
	// schemaMaskMerchantRef ParseConfig 恒返回的商户标识（P47 不判商户身份）。
	schemaMaskMerchantRef = "merchant-fixed-1"
	// schemaMaskPayType 假协议唯一的渠道能力（渠道行不在 P47 的判据内）。
	schemaMaskPayType = "alipay"
	// schemaMaskTokenValue / schemaMaskOtherValue 两个**可区分键**的原值：断言
	// "原值没被覆盖"与"原值被掩码"必须能分别命中，才不是同义反复。
	schemaMaskTokenValue = "tok_live_1"
	schemaMaskOtherValue = "other_live_1"
)

// schemaMaskConfig 假协议的凭据：P47 只判掩码/合并的空壳。
type schemaMaskConfig struct {
	domain.AccountConfigSeal
}

func (schemaMaskConfig) Provider() domain.ProviderName { return schemaMaskProviderName }
func (schemaMaskConfig) MerchantRef() string           { return schemaMaskMerchantRef }

// configurableProvider P47 的注入点：**schema 是数据**，ConfigSchema() 原样返回。
//
// 两个可区分键（token / other）让"秘密标记换到另一个字段"这件事可观测：如果
// 两个字段的值相同，④ 的位移断言就会退化成恒真。
type configurableProvider struct {
	schema domain.ConfigSchema
}

var _ domain.PaymentProvider = (*configurableProvider)(nil)

func (p *configurableProvider) Name() domain.ProviderName { return schemaMaskProviderName }

func (p *configurableProvider) Capabilities() []string { return []string{schemaMaskPayType} }

// ConfigSchema 原样返回 p.schema——**不排序、不复制出第二份清单**：P47 改的就是它。
func (p *configurableProvider) ConfigSchema() domain.ConfigSchema { return p.schema }

// ParseConfig 接受 `token` 与 `other` 两个键（严格 JSON：schema 之外的键被拒），
// 返回固定的 Provider + MerchantRef（P47 不判凭据语义）。
func (p *configurableProvider) ParseConfig(raw []byte) (domain.AccountConfig, error) {
	var doc struct {
		Token string `json:"token"`
		Other string `json:"other"`
	}
	if err := jsonUnmarshalStrictForTest(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrConfigMissing, err)
	}
	return schemaMaskConfig{}, nil
}

func (p *configurableProvider) BuildPayURL(domain.AccountConfig, domain.PayRequest) (string, error) {
	return "", fmt.Errorf("%w: schema mask test: BuildPayURL must not be called", domain.ErrConfigMissing)
}

func (p *configurableProvider) VerifyNotify(domain.AccountConfig, url.Values) (domain.NotifyEvent, error) {
	return domain.NotifyEvent{}, fmt.Errorf("%w: schema mask test: VerifyNotify must not be called",
		domain.ErrConfigMissing)
}

// jsonUnmarshalStrictForTest 严格解码（未知键即失败）：证明"改了 schema 之外的键
// 会被拒"这件事在假协议上也成立，P47 的 ③（PUT 回传 "***" 后重新 ParseConfig）
// 因此不是靠一个宽容的解析器蒙混过关。
func jsonUnmarshalStrictForTest(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// maskSchema 造一份"两个键、其中 secretName 是秘密"的 schema 数据。
//
// secretName == "" → 两个键都不是秘密（②的比较基线）。
func maskSchema(secretName string) domain.ConfigSchema {
	fields := []domain.ConfigField{
		{Name: "token", Kind: domain.ConfigFieldPassword},
		{Name: "other", Kind: domain.ConfigFieldText},
	}
	for i := range fields {
		fields[i].Secret = fields[i].Name == secretName
	}
	return domain.ConfigSchema{Fields: fields}
}

// paymentAccountStore 账户/渠道的内存替身：只替换**支付账户**读写面，其它方法
// 仍走 fakeStore。种行不经接口（照 spec P33 的"直接种 DB 行"），故这里直接持有
// 行数据；UpdatePaymentAccount 复刻仓库的两条关键语义：config 变更 → revision+1、
// 只改元数据 → revision 不动。
type paymentAccountStore struct {
	*fakeStore
	mu       sync.Mutex
	accounts []*domain.PaymentAccount
	channels map[int64][]*domain.PaymentChannel
}

func (s *paymentAccountStore) ListPaymentAccounts(context.Context) ([]*domain.PaymentAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*domain.PaymentAccount, 0, len(s.accounts))
	for _, acct := range s.accounts {
		out = append(out, clonePaymentAccount(acct))
	}
	return out, nil
}

func (s *paymentAccountStore) ListPaymentChannels(_ context.Context, accountID int64) ([]*domain.PaymentChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*domain.PaymentChannel(nil), s.channels[accountID]...), nil
}

// ListAllPaymentChannels 全量渠道（§19-2 / P56 的取数口）：account_id 升序铺开。
// 不实现它会让选路在**替身**上看到"没有渠道"（继承 fakeStore 的空表），P54 的
// 「503 而非 400」判据会退化成 400——那不是实现错，是替身错。
func (s *paymentAccountStore) ListAllPaymentChannels(context.Context) ([]*domain.PaymentChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.channels))
	for id := range s.channels {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out []*domain.PaymentChannel
	for _, id := range ids {
		out = append(out, s.channels[id]...)
	}
	return out, nil
}

func (s *paymentAccountStore) GetPaymentAccountByKey(_ context.Context, key string) (*domain.PaymentAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acct := range s.accounts {
		if acct.Key == key {
			return clonePaymentAccount(acct), nil
		}
	}
	return nil, repository.ErrNotFound
}

func (s *paymentAccountStore) UpdatePaymentAccount(_ context.Context, key string, upd repository.AccountUpdate) (*domain.PaymentAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acct := range s.accounts {
		if acct.Key != key {
			continue
		}
		// CAS（§19-1 / P57）：与 repository 的 `WHERE config_revision=?` 同语义——
		// 版本不符时**整条更新都不生效**（不是"只跳过 config"），否则"半更新"会比
		// 丢更新更难查。
		if upd.ExpectedConfigRevision > 0 && acct.ConfigRevision != upd.ExpectedConfigRevision {
			return nil, fmt.Errorf("%w: payment account key=%q: expected config_revision=%d actual=%d",
				repository.ErrConfigRevisionMismatch, key, upd.ExpectedConfigRevision, acct.ConfigRevision)
		}
		if upd.DisplayName != nil {
			acct.DisplayName = *upd.DisplayName
		}
		if upd.Priority != nil {
			acct.Priority = *upd.Priority
		}
		if upd.Enabled != nil {
			acct.Enabled = *upd.Enabled
		}
		if upd.Config != nil {
			acct.Config = append(json.RawMessage(nil), upd.Config...)
			acct.ConfigRevision++ // 配置变更才推进代际（缓存键）
		}
		return clonePaymentAccount(acct), nil
	}
	return nil, repository.ErrNotFound
}

// storedConfig 读出**库内原值**（不经掩码）：③ 的判据必须落在这一层，否则
// "读面回 ***、库里也成了 ***"这种静默丢密钥会被漏掉。
func (s *paymentAccountStore) storedConfig(t *testing.T, key string) (map[string]any, int64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acct := range s.accounts {
		if acct.Key != key {
			continue
		}
		var doc map[string]any
		require.NoError(t, json.Unmarshal(acct.Config, &doc))
		return doc, acct.ConfigRevision
	}
	t.Fatalf("account %q not found", key)
	return nil, 0
}

func clonePaymentAccount(acct *domain.PaymentAccount) *domain.PaymentAccount {
	out := *acct
	out.Config = append(json.RawMessage(nil), acct.Config...)
	return &out
}

// newSchemaMaskService 构造只走账户读/写面的 Service：注册表里只有那个**可注入
// schema** 的假协议，账户行已种好（config 里 token / other 两个键都有值）。
func newSchemaMaskService(t *testing.T, provider *configurableProvider, rawConfig string) (*Service, *paymentAccountStore) {
	t.Helper()
	registry, err := domain.NewProviderRegistry([]domain.PaymentProvider{provider})
	require.NoError(t, err)
	store := &paymentAccountStore{
		fakeStore: newFakeStore(),
		accounts: []*domain.PaymentAccount{{
			ID: 1, Key: schemaMaskAccountKey,
			Provider: schemaMaskProviderName, MerchantRef: schemaMaskMerchantRef,
			DisplayName: "schema mask", Enabled: true, Priority: 0,
			Config: json.RawMessage(rawConfig), ConfigRevision: 1,
		}},
	}
	svc := &Service{
		store:           store,
		inv:             &invRecorder{},
		paymentCfgs:     make(map[string]cachedPaymentCfg, 1),
		paymentRegistry: registry,
	}
	return svc, store
}

// accountView P47 的读面：管理面账户列表里那一行（`GET /payment/accounts` 的投影源）。
func accountView(t *testing.T, svc *Service) PaymentAccountView {
	t.Helper()
	views, err := svc.ListPaymentAccounts(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Empty(t, views[0].ConfigError, "fixture 的配置必须可解析（否则掩码断言会被 config_error 掩盖）")
	return views[0]
}

// TestPaymentAccountMaskFollowsSchemaSecret P47①②③④：schema 数据变 → 掩码/合并变。
//
// 四条断言**共用一个 provider 实例**（注册表里始终是同一个指针），只有
// `provider.schema` 被改写。任何"service 侧写死字段名"的实现都无法同时满足这四条。
func TestPaymentAccountMaskFollowsSchemaSecret(t *testing.T) {
	ctx := context.Background()
	provider := &configurableProvider{schema: maskSchema("token")}
	svc, store := newSchemaMaskService(t, provider,
		`{"token":"`+schemaMaskTokenValue+`","other":"`+schemaMaskOtherValue+`"}`)

	// ① token.secret=true → 读面把 token 换成 "***"，另一个键原值返回。
	view := accountView(t, svc)
	require.Equal(t, secretPlaceholder, view.Config["token"],
		"schema 标了 secret 的字段读面必须掩码（唯一的秘密声明口）")
	require.Equal(t, schemaMaskOtherValue, view.Config["other"],
		"未标 secret 的字段必须原值返回（掩码不得连坐）")

	// ② 同一个 fake，**只改 p.schema 的数据**：token.secret=false → 原值返回。
	provider.schema = maskSchema("")
	view = accountView(t, svc)
	require.Equal(t, schemaMaskTokenValue, view.Config["token"],
		"secret=false 的字段必须原值返回：掩码行为由 schema 派生，不是写死的字段名")
	require.Equal(t, schemaMaskOtherValue, view.Config["other"])

	// ③ PUT 回传 "***" → 按 schema 合并**旧值**（原值不被覆盖），revision 递增。
	provider.schema = maskSchema("token")
	updated, err := svc.UpdatePaymentAccount(ctx, schemaMaskAccountKey, PaymentAccountUpdateInput{
		Config: json.RawMessage(`{"token":"` + secretPlaceholder + `","other":"other_live_2"}`),
	})
	require.NoError(t, err)
	require.Equal(t, secretPlaceholder, updated.Config["token"], "读面仍然是掩码（回显不得泄露旧值）")
	require.Equal(t, "other_live_2", updated.Config["other"], "非秘密字段按提交值更新")

	stored, revision := store.storedConfig(t, schemaMaskAccountKey)
	require.Equal(t, schemaMaskTokenValue, stored["token"],
		`PUT 的 "***" 必须按 schema 合并旧值：库内原值不得被占位符覆盖（M25）`)
	require.Equal(t, "other_live_2", stored["other"])
	require.Greater(t, revision, int64(1), "配置变更必须推进 config_revision（缓存键失效）")

	// ④ 把 Secret 标记从 token 移到 other → 掩码随之转移（M35 的杀手）。
	provider.schema = maskSchema("other")
	view = accountView(t, svc)
	require.Equal(t, schemaMaskTokenValue, view.Config["token"],
		"token 不再是秘密 → 原值返回（写死 private_key/token 的实现这里必红）")
	require.Equal(t, secretPlaceholder, view.Config["other"],
		"秘密标记换到 other → other 必须变成掩码")
}

// TestUpdatePaymentAccountRejectsStaleRevision P57 / §19-1：带 `config` 的更新是
// **比较并交换**——同一份 `expected_config_revision` 只能用一次；不带的保持 r12
// 行为（不校验）。
//
// 这条钉的是 lost update 最坏形态：A 轮换了密钥（revision 1→2），B 基于 revision 1
// 的旧表单再提交时，服务端必须拒绝，而不是把 A 的新密钥静默改回旧值（两人都 200，
// 下次回调验签失败）。
func TestUpdatePaymentAccountRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	provider := &configurableProvider{schema: maskSchema("token")}
	svc, store := newSchemaMaskService(t, provider,
		`{"token":"`+schemaMaskTokenValue+`","other":"`+schemaMaskOtherValue+`"}`)

	// ① A 的轮换：基于 revision 1 → 成功，revision 1→2。
	first, err := svc.UpdatePaymentAccount(ctx, schemaMaskAccountKey, PaymentAccountUpdateInput{
		Config:                 json.RawMessage(`{"token":"` + secretPlaceholder + `","other":"rotated_by_a"}`),
		ExpectedConfigRevision: 1,
	})
	require.NoError(t, err)
	require.Equal(t, "rotated_by_a", first.Config["other"])
	stored, rev := store.storedConfig(t, schemaMaskAccountKey)
	require.EqualValues(t, 2, rev, "config 变更推进 revision")
	require.Equal(t, schemaMaskTokenValue, stored["token"], "PUT 回传的占位符必须合并旧值（密钥不被写坏）")

	// ② B 的旧表单（同样基于 revision 1）→ 409，且**库里什么都没变**。
	_, err = svc.UpdatePaymentAccount(ctx, schemaMaskAccountKey, PaymentAccountUpdateInput{
		Config:                 json.RawMessage(`{"token":"stale_key_from_b","other":"stale_by_b"}`),
		ExpectedConfigRevision: 1,
	})
	require.ErrorIs(t, err, ErrConflict, "过期的 expected_config_revision 必须映射 409（不是 400/500）")

	stored, rev = store.storedConfig(t, schemaMaskAccountKey)
	require.Equal(t, "rotated_by_a", stored["other"], "被拒的更新不得改动库内任何字段")
	require.Equal(t, schemaMaskTokenValue, stored["token"], "被拒的更新不得覆盖密钥")
	require.EqualValues(t, 2, rev, "被拒的更新不得推进 revision")

	// ③ 不带该字段 = 不校验（脚本兼容口径，与 r12 行为等价）。
	third, err := svc.UpdatePaymentAccount(ctx, schemaMaskAccountKey, PaymentAccountUpdateInput{
		Config: json.RawMessage(`{"token":"` + secretPlaceholder + `","other":"no_cas"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "no_cas", third.Config["other"])

	// ④ 负数 → 400（不是 409）：这是"你写错了请求"。
	_, err = svc.UpdatePaymentAccount(ctx, schemaMaskAccountKey, PaymentAccountUpdateInput{
		ExpectedConfigRevision: -1,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}
