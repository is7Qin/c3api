// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestCreateTemplateValidates(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	cases := []struct {
		name string
		tpl  *domain.Template
	}{
		{"name empty", &domain.Template{Name: "", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"bad url", &domain.Template{Name: "t", BaseURL: "not-a-url", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"trailing /v1 rejected", &domain.Template{Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"trailing /v1/ rejected", &domain.Template{Name: "t", BaseURL: "https://u/v1/", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"empty supported_formats", &domain.Template{Name: "t", BaseURL: "https://u"}},
		{"invalid enum", &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.RequestFormat("nope")}}},
		{"duplicate formats", &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIChat}}},
		{"format_models key not in supported", &domain.Template{
			Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatAnthropic: {"m"}},
		}},
		{"format_models empty list", &domain.Template{
			Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {}},
		}},
		{"format_models model outside serve set", &domain.Template{
			Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateTemplate(context.Background(), tc.tpl)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
	// 合法：format_models 模型 ∈ Models
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "t", BaseURL: "https://u",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"gpt-4o"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
	})
	require.NoError(t, err)
}

// TestTemplateCredentialTypeDefaultAndValid 评审 M-1：默认值兜底在 service 层
// （repo 全字段 Set 写空串的防线）：缺省 → api_key；显式 api_key → 成功；
// 未注册类型（号池生态未实现）→ 400；Update 同路径兜底。
func TestTemplateCredentialTypeDefaultAndValid(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}

	created, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "t-default", BaseURL: "https://u",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, created.CredentialType, "缺省默认 api_key")

	created2, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "t-api", BaseURL: "https://u", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, created2.CredentialType, "显式 api_key 成功")

	_, err = svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "t-bad", BaseURL: "https://u", CredentialType: credential.Type("codex_oauth"),
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "未注册类型 → 400")

	// Update 全量路径：空类型被兜底为 api_key（防全字段 Set 写空串）
	got, err := svc.GetTemplate(context.Background(), created.ID)
	require.NoError(t, err)
	got.Name = "t-renamed"
	got.CredentialType = ""
	updated, err := svc.UpdateTemplate(context.Background(), got)
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, updated.CredentialType, "update 缺省同样兜底 api_key")
}

// Phase 3a：分组 = 平台容量池（无内嵌 key）。创建返回分组本身（visibility
// 缺省 public）；key 为独立表（用户面 /user/keys 创建）。
func TestCreateGroupFlow(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	g, err := svc.CreateGroup(context.Background(), "g1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	require.Equal(t, domain.GroupVisibilityPublic, g.Visibility, "visibility 落库")
	g2, err := svc.CreateGroup(context.Background(), "g2", domain.GroupVisibilityPrivate, nil, nil)
	require.NoError(t, err)
	require.Equal(t, domain.GroupVisibilityPrivate, g2.Visibility)
	got, err := svc.GetGroup(context.Background(), g.ID)
	require.NoError(t, err)
	require.Equal(t, "g1", got.Name)
}

func TestQueryStatsGranularity(t *testing.T) {
	fs := newFakeStore()
	fs.stats = []*domain.StatBucket{
		{BucketTime: mustTime("2026-08-01T10:00:00Z"), GroupID: 1, Model: "m", RequestCount: 10, TotalTokens: 100,
			TTFTTotalMS: 200, TTFTCount: 5, TTFTMaxMS: 80, TTFTHist: []int64{3, 2, 0, 0, 0, 0, 0, 0, 0, 0}},
		{BucketTime: mustTime("2026-08-01T11:00:00Z"), GroupID: 1, Model: "m", RequestCount: 5, TotalTokens: 50,
			TTFTTotalMS: 250, TTFTCount: 5, TTFTMaxMS: 60, TTFTHist: []int64{1, 4, 0, 0, 0, 0, 0, 0, 0, 0}},
	}
	svc := &Service{store: fs, inv: &invRecorder{}}
	rows, err := svc.QueryStats(context.Background(), repository.StatQuery{}, "day")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(15), rows[0].RequestCount, "day aggregation sums requests")
	require.Equal(t, int64(150), rows[0].TotalTokens)
	// TTFT 四字段日合并（rewrite spec 2026-08-14 前置清单②）：total/count 求和、
	// max 取大、直方图逐元素加。
	require.Equal(t, int64(450), rows[0].TTFTTotalMS, "day aggregation sums ttft_total")
	require.Equal(t, int64(10), rows[0].TTFTCount, "day aggregation sums ttft_count")
	require.Equal(t, int64(80), rows[0].TTFTMaxMS, "day aggregation takes max ttft_max")
	require.Equal(t, []int64{4, 6, 0, 0, 0, 0, 0, 0, 0, 0}, rows[0].TTFTHist, "day aggregation merges hist element-wise")
}

// TestListQueryValidation service 层 sort/order 白名单校验：非法值 → ErrInvalidInput
// （handler 依赖此 400；fake store 不校验，故校验必须在 service 层前置）。
func TestListQueryValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}}

	_, _, err := svc.ListTemplates(context.Background(), repository.ListQuery{Order: "sideways"})
	require.ErrorIs(t, err, ErrInvalidInput, "非法 order")
	_, _, err = svc.ListTemplates(context.Background(), repository.ListQuery{Sort: "bogus"})
	require.ErrorIs(t, err, ErrInvalidInput, "非法 sort")
	_, _, err = svc.ListGroups(context.Background(), repository.ListQuery{Sort: "weight"})
	require.ErrorIs(t, err, ErrInvalidInput, "账号专属 sort 对分组无效")
	_, _, err = svc.ListAccountViews(context.Background(), repository.ListQuery{Sort: "bogus"})
	require.ErrorIs(t, err, ErrInvalidInput, "ListAccountViews 同样校验")

	rows, total, err := svc.ListTemplates(context.Background(), repository.ListQuery{Sort: "name", Order: "asc"})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
	views, total, err := svc.ListAccountViews(context.Background(), repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, views)
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// --- Task 4a: 批量删除/更新 ---

func seedTemplate(t *testing.T, svc *Service, name string) *domain.Template {
	t.Helper()
	created, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "https://" + name + ".example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	return created
}

func seedAccount(t *testing.T, svc *Service, tplID int64, name string) *domain.Account {
	t.Helper()
	created, err := svc.CreateAccount(context.Background(), &domain.Account{
		Name: name, UpstreamKey: "k-" + name, TemplateID: tplID, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	return created
}

func TestBatchDeleteTemplates(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	t1 := seedTemplate(t, svc, "a")
	t2 := seedTemplate(t, svc, "b")
	before := rec.total()
	require.NoError(t, svc.DeleteTemplatesBatch(ctx, []int64{t1.ID, t2.ID}))
	require.Greater(t, rec.total(), before, "批量删除成功后必须 invalidate")
	_, err := svc.GetTemplate(ctx, t1.ID)
	require.ErrorIs(t, err, ErrNotFound, "批量删除后模板必须消失")
}

func TestBatchUpdateTemplates(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	t1 := seedTemplate(t, svc, "a")
	name := "renamed"
	before := rec.total()
	require.NoError(t, svc.UpdateTemplatesBatch(ctx, []int64{t1.ID}, repository.TemplatePatch{Name: &name}))
	require.Greater(t, rec.total(), before, "批量更新成功后必须 invalidate")
	got, err := svc.GetTemplate(ctx, t1.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
}

func TestBatchDeleteAccounts(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	a1 := seedAccount(t, svc, tpl.ID, "a1")
	a2 := seedAccount(t, svc, tpl.ID, "a2")
	before := rec.total()
	require.NoError(t, svc.DeleteAccountsBatch(ctx, []int64{a1.ID, a2.ID}))
	require.Greater(t, rec.total(), before, "批量删除成功后必须 invalidate")
	_, err := svc.GetAccount(ctx, a1.ID)
	require.ErrorIs(t, err, ErrNotFound, "批量删除后账号必须消失")
}

// TestCreateAccountGroups 账号侧分组：创建带分组 / 更新替换/清空/不变 /
// 组缺失 404 / GetAccountGroups 缺账号 404。
func TestCreateAccountGroups(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g2, err := svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)

	// 创建带分组
	before := rec.total()
	acc, err := svc.CreateAccount(ctx, &domain.Account{
		Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-a1", GroupIDs: &[]int64{g1.ID, g2.ID},
	})
	require.NoError(t, err)
	require.Greater(t, rec.total(), before, "创建带分组必须 invalidate")
	got, err := svc.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{g1.ID, g2.ID}, got)

	// 更新替换：只剩 g2
	before = rec.total()
	_, err = svc.UpdateAccount(ctx, &domain.Account{
		ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-a1", GroupIDs: &[]int64{g2.ID},
	})
	require.NoError(t, err)
	require.Greater(t, rec.total(), before, "更新分组必须 invalidate")
	got, err = svc.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{g2.ID}, got)

	// 更新清空（空数组）
	_, err = svc.UpdateAccount(ctx, &domain.Account{
		ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-a1", GroupIDs: &[]int64{},
	})
	require.NoError(t, err)
	got, err = svc.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.Empty(t, got, "[] = 清空")

	// 更新不变（nil）
	_, err = svc.UpdateAccount(ctx, &domain.Account{
		ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-a1", GroupIDs: nil,
	})
	require.NoError(t, err)
	got, err = svc.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.Empty(t, got, "nil = 不变")

	// 创建带缺失组 → 404 含 id
	_, err = svc.CreateAccount(ctx, &domain.Account{
		Name: "a2", TemplateID: tpl.ID, UpstreamKey: "sk-a2", GroupIDs: &[]int64{999},
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "999")

	// 更新带缺失组 → 404
	_, err = svc.UpdateAccount(ctx, &domain.Account{
		ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-a1", GroupIDs: &[]int64{999},
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "999")

	// GetAccountGroups 缺账号 → 404
	_, err = svc.GetAccountGroups(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestBatchUpdateAccountsGroupIDs 批量 group_ids：替换生效 + lastPatch 记录
// （M3）+ 校验（长度/去重/元素 <= 0 → ErrInvalidInput，nil/空数组合法）。
func TestBatchUpdateAccountsGroupIDs(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	a1 := seedAccount(t, svc, tpl.ID, "a1")
	a2 := seedAccount(t, svc, tpl.ID, "a2")

	// 批量替换：两个账号都进 g1
	before := rec.total()
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
		repository.AccountPatch{GroupIDs: &[]int64{g1.ID}}))
	require.Greater(t, rec.total(), before)
	for _, id := range []int64{a1.ID, a2.ID} {
		got, err := svc.GetAccountGroups(ctx, id)
		require.NoError(t, err)
		require.Equal(t, []int64{g1.ID}, got)
	}
	require.NotNil(t, fs.lastPatch.GroupIDs, "lastPatch 记录 group_ids 提供状态")
	require.Equal(t, []int64{g1.ID}, *fs.lastPatch.GroupIDs)

	// 批量清空（[]）→ 提供 + 清空
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
		repository.AccountPatch{GroupIDs: &[]int64{}}))
	require.NotNil(t, fs.lastPatch.GroupIDs)
	require.Empty(t, *fs.lastPatch.GroupIDs, "[] 也算提供（清空）")
	for _, id := range []int64{a1.ID, a2.ID} {
		got, err := svc.GetAccountGroups(ctx, id)
		require.NoError(t, err)
		require.Empty(t, got)
	}

	// 批量组缺失 → 404 含 id（校验在 store 调用前？否——存在性在 repo 层，service 映射）
	err = svc.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{GroupIDs: &[]int64{999}})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "999")

	// 校验失败 → ErrInvalidInput（store 不被调用）
	before = rec.total()
	dup := []int64{g1.ID, g1.ID}
	require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{GroupIDs: &dup}), ErrInvalidInput, "重复 group_ids")
	neg := []int64{-1}
	require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{GroupIDs: &neg}), ErrInvalidInput, "元素 <= 0")
	over := make([]int64, 101)
	require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{GroupIDs: &over}), ErrInvalidInput, "超长")
	require.Equal(t, before, rec.total(), "校验失败不 invalidate")
	// nil 合法（不变）
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{Name: ptr("renamed")}))
	require.Nil(t, fs.lastPatch.GroupIDs, "nil = 未提供")
}

func ptr(s string) *string { return &s }

func TestBatchUpdateAccounts(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	a := seedAccount(t, svc, tpl.ID, "a1")
	st := domain.StatusDisabled
	before := rec.total()
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{Status: &st}))
	require.Greater(t, rec.total(), before, "批量更新成功后必须 invalidate")
	got, err := svc.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusDisabled, got.Status)

	// 缺 id → repository.ErrNotFound 包装 → mapRepoErr → service.ErrNotFound（消息含缺失 id）
	err = svc.UpdateAccountsBatch(ctx, []int64{999}, repository.AccountPatch{Status: &st})
	require.ErrorIs(t, err, ErrNotFound, "缺 id 必须映射 404")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")
}

type fakeKeyRegistrar struct {
	mu       sync.Mutex
	upserted []string
	metas    []domain.KeyMeta // 快照断言（A-2：ProtocolConverts 增量注册）
	deleted  []string
}

func (k *fakeKeyRegistrar) Upsert(hash string, meta domain.KeyMeta) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.upserted = append(k.upserted, hash)
	k.metas = append(k.metas, meta)
}

// lastMeta 最近一次 Upsert 的 KeyMeta（nil = 无）。
func (k *fakeKeyRegistrar) lastMeta() *domain.KeyMeta {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.metas) == 0 {
		return nil
	}
	m := k.metas[len(k.metas)-1]
	return &m
}

func (k *fakeKeyRegistrar) Delete(hash string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deleted = append(k.deleted, hash)
}

func TestBatchDeleteGroupsKeyCleanup(t *testing.T) {
	fs := newFakeStore()
	keys := &fakeKeyRegistrar{}
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, keys: keys, log: nil}
	ctx := context.Background()
	g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g2, err := svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	before := rec.total()
	require.NoError(t, svc.DeleteGroupsBatch(ctx, []int64{g1.ID, g2.ID}))
	require.Greater(t, rec.total(), before, "批量删除成功后必须 invalidate")
	// Phase 3a：组删除前置清理组内 key（无 key 时无 hash 可清理）
	require.Empty(t, keys.deleted, "无 key 的组删除不触发 Auth 增量清理")
	// 软删语义（对齐真实 repo）：GET 单个仍可查已删项（deleted_at 置值）；
	// 列表过滤软删组
	got, err := svc.GetGroup(ctx, g1.ID)
	require.NoError(t, err, "软删组 GET 单个仍可查（管理面详情 200，既有语义）")
	require.NotNil(t, got.DeletedAt, "批量删除后 deleted_at 置值")
	rows, total, err := svc.ListGroups(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), total, "批量删除后列表过滤软删组")
	require.Empty(t, rows)
}

func TestBatchUpdateGroups(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	name := "renamed"
	before := rec.total()
	require.NoError(t, svc.UpdateGroupsBatch(ctx, []int64{g.ID}, repository.GroupPatch{Name: &name}))
	// O2 矩阵：GroupPatch 无 price_multiplier 字段 → 组名/可见性不触发任何快照重载。
	require.Equal(t, before, rec.total(), "批量组更新（仅 name）不 invalidate（倍率未变）")
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
}

// TestBatchDeleteInvalidIDs 空/超长/重复 ids → ErrInvalidInput（三资源同型）。
func TestBatchDeleteInvalidIDs(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	cases := map[string][]int64{
		"nil ids":           nil,
		"空 ids":             {},
		"超长 ids（101 个）": make([]int64, 101),
		"重复 ids":           {1, 1},
	}
	for name, ids := range cases {
		err := svc.DeleteTemplatesBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "templates %s", name)
		err = svc.DeleteAccountsBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "accounts %s", name)
		err = svc.DeleteGroupsBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "groups %s", name)
	}
}

// TestBatchUpdatePatchValidation patch 校验失败 → ErrInvalidInput（校验在 store 调用前）。
func TestBatchUpdatePatchValidation(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	empty := ""
	badURL := "not-a-url"
	badFormat := domain.RequestFormat("nope")

	t.Run("templates", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{Name: &empty}), ErrInvalidInput, "空 name")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{BaseURL: &badURL}), ErrInvalidInput, "非法 BaseURL")
		trailingV1 := "https://u/v1"
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{BaseURL: &trailingV1}), ErrInvalidInput, "尾 /v1（裸根约定）")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{badFormat}}), ErrInvalidInput, "非法 SupportedFormats")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{FormatModels: &map[domain.RequestFormat][]string{badFormat: {"m"}}}), ErrInvalidInput, "非法 FormatModels key")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{FormatModels: &map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {}}}), ErrInvalidInput, "空 FormatModels 列表")
	})
	t.Run("accounts", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{Name: &empty}), ErrInvalidInput, "空 name")
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{UpstreamKey: &empty}), ErrInvalidInput, "空 UpstreamKey")
		badTID := int64(0)
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{TemplateID: &badTID}), ErrInvalidInput, "TemplateID <= 0")
		badWeight := -1
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{Weight: &badWeight}), ErrInvalidInput, "Weight < 0")
		badMC := 0
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{MaxConcurrency: &badMC}), ErrInvalidInput, "MaxConcurrency < 1")
		// group_ids：超长 / 重复 / 元素 <= 0 → ErrInvalidInput；nil 与空数组合法
		over := make([]int64, 101)
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{GroupIDs: &over}), ErrInvalidInput, "GroupIDs 超长")
		dup := []int64{1, 1}
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{GroupIDs: &dup}), ErrInvalidInput, "GroupIDs 重复")
		zero := []int64{0}
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{GroupIDs: &zero}), ErrInvalidInput, "GroupIDs 元素 <= 0")
	})
	t.Run("groups", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateGroupsBatch(ctx, []int64{1}, repository.GroupPatch{Name: &empty}), ErrInvalidInput, "空 name")
	})
	t.Run("合法 patch 不触发校验错误", func(t *testing.T) {
		valid := domain.FormatAnthropic
		err := svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{SupportedFormats: &[]domain.RequestFormat{valid}})
		require.NotErrorIs(t, err, ErrInvalidInput)
		require.ErrorIs(t, err, ErrNotFound, "fake 缺 id → 404 映射")
	})
}

// TestBatchNotFoundMapping 缺失 id → repo.ErrNotFound → service.ErrNotFound（含缺失 id 详情）。
func TestBatchNotFoundMapping(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	err := svc.DeleteTemplatesBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound, "repo.ErrNotFound 映射为 service.ErrNotFound")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")

	err = svc.DeleteAccountsBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "999")

	err = svc.UpdateTemplatesBatch(ctx, []int64{999}, repository.TemplatePatch{})
	require.ErrorIs(t, err, ErrNotFound, "批量更新同样映射 404")
	require.Contains(t, err.Error(), "999")

	// 分组缺 id 由 GetGroup 前置检查拦截（直接返回 service.ErrNotFound）
	err = svc.DeleteGroupsBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound)
}

// --- Task 5：单资源删除缺 id → 404 映射（与批量语义对齐） ---

// TestSingleDeleteNotFoundMapping 单资源 Delete 缺 id：repo 单删缺 id 与
// DeleteGroup 前置 Get 均经 mapRepoErr → service.ErrNotFound（消息含缺失 id，
// 与批量 404 一致），且失败不 invalidate。
func TestSingleDeleteNotFoundMapping(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	ctx := context.Background()

	err := svc.DeleteTemplate(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound, "templates 单删缺 id → 404")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")

	err = svc.DeleteAccount(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound, "accounts 单删缺 id → 404")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")

	err = svc.DeleteGroup(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound, "groups 单删缺 id → 404（GetGroup 前置拦截）")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")

	require.Zero(t, rec.total(), "缺 id 失败不得 invalidate")
}

// TestCreateAccountMissingTemplate CreateAccount 前置 GetTemplate 缺 id →
// service.ErrNotFound（消息含缺失 id；此前裸透传 repository 错误 → 生产 500）。
func TestCreateAccountMissingTemplate(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	_, err := svc.CreateAccount(context.Background(), &domain.Account{
		Name: "a", UpstreamKey: "k", TemplateID: 999, MaxConcurrency: 4,
	})
	require.ErrorIs(t, err, ErrNotFound, "模板缺 id → 404")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")
}

// TestDeleteGroupMissing 组删除前置 GetGroup 缺 id → service.ErrNotFound。
func TestDeleteGroupMissing(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	err := svc.DeleteGroup(context.Background(), 999)
	require.ErrorIs(t, err, ErrNotFound, "分组缺 id → 404")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")
}
