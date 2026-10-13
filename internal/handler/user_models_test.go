// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service"
)

// catalogSched 模型目录注入的 Scheduler 面：运行时视图（service.RuntimeProvider）
// + 组可路由模型能力（service.RouteModelsProvider，经 s.sched 能力探测取出）。
type catalogSched interface {
	service.RuntimeProvider
	service.RouteModelsProvider
}

// routeSched 实现上述两面：组可路由模型集合（快照缺失/组失效 → ok=false）。
// 嵌入 fakeSched 提供运行时视图桩——模型目录对 s.sched 做能力探测，无独立装配参数。
type routeSched struct {
	fakeSched
	byGroup map[int64][]string
}

func (f routeSched) GroupModels(groupID int64) ([]string, bool) {
	models, ok := f.byGroup[groupID]
	return models, ok
}

// newModelCatalogRouters 双路由（admin 建组 + user 面）；route（组可路由模型面）
// 经 Deps.Scheduler 注入。用户专属倍率走 Store 的 group_assignment（既有依赖，
// 无额外注入参数）。route 的 map 为引用类型——可在建组拿到 id 后回填。
func newModelCatalogRouters(t *testing.T, route catalogSched) (
	doAdmin, doUser func(method, path, body, token string) *httptest.ResponseRecorder,
	store *fakeStore, svc *service.Service,
) {
	t.Helper()
	store = newFakeStore()
	svc = service.New(service.Deps{
		Store: store, Scheduler: route,
		Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store,
	})

	adminH := New(svc)
	ar := chi.NewRouter()
	ar.Mount("/", adminH.Router())

	iss := auth.NewIssuer("test-secret")
	ur := userapi.Router(svc, iss, fakeUserStatus{store: store}, nil, nil)

	do := func(h http.Handler) func(string, string, string, string) *httptest.ResponseRecorder {
		return func(method, path, body, token string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			} else {
				req.Header.Set("Authorization", "Bearer admin-tok")
			}
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}
	}
	return do(ar), do(ur), store, svc
}

// createGroup 管理面建组（body 为 JSON；支持 price_multiplier 组默认倍率），返回组 id。
func createGroup(t *testing.T, doAdmin func(string, string, string, string) *httptest.ResponseRecorder, body string) int64 {
	t.Helper()
	rec := doAdmin(http.MethodPost, "/api/admin/groups", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "create %s: %s", body, rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	return *g.ID
}

// grantOverride 给该用户在该组设专属倍率覆盖（万分数）——走 Store 的 assignment
// 表（与生产同一数据源：专属覆盖填充 group_assignments.price_multiplier）。
func grantOverride(t *testing.T, store *fakeStore, groupID, userID int64, mult int) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.GrantGroup(ctx, groupID, userID))
	require.NoError(t, store.SetAssignmentMultiplier(ctx, groupID, userID, &mult))
}

// TestUserModelsWiring A2/A3/A4：模型原样透出、倍率为快照值（毫分 → USD）、变体随
// 模型返回（Seq 升序）；有效倍率 = 专属覆盖 ?? 组默认；无价模型 Mode=null + 价格全
// null；同一模型跨组重复不去重。
func TestUserModelsWiring(t *testing.T) {
	routeByGroup := map[int64][]string{}
	doAdmin, doUser, store, svc := newModelCatalogRouters(t, routeSched{byGroup: routeByGroup})

	g1 := createGroup(t, doAdmin, `{"name":"g1"}`)
	g2 := createGroup(t, doAdmin, `{"name":"g2"}`)

	ctx := context.Background()
	// 价格快照：token 模型（含缓存列 + 变体），另有无价模型（快照异源）。
	_, err := store.UpsertPriceEntriesFromLiteLLM(ctx, []*domain.PriceEntry{
		{
			Model: "m-token", Mode: domain.PriceModeToken,
			InputPerM: new(int64(250000)), OutputPerM: new(int64(1000000)),
			CacheReadPerM: new(int64(25000)), CacheWritePerM: new(int64(125000)),
			Provider: new("openai"), SupportsPromptCaching: new(true), Source: domain.PricingSourceLitellm,
		},
	})
	require.NoError(t, err)
	// 故意乱序写入：断言返回按 Seq 升序。
	_, err = store.ReplacePriceVariants(ctx, "m-token", []*domain.PriceVariant{
		{Model: "m-token", Seq: 2, CtxMin: new(int64(128000)), MultBP: new(int(15000))},
		{Model: "m-token", Seq: 1, ServiceTier: new("priority"), SetInputPerM: new(int64(300000))},
	})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadPricingCtx(ctx))

	routeByGroup[g1] = []string{"m-token", "m-noprice"}
	routeByGroup[g2] = []string{"m-token"}

	token, uid := registerAndGet(t, doUser, "models@example.com")
	grantOverride(t, store, g1, uid, 20000) // g1 专属覆盖 ×2；g2 无覆盖 → 组默认 ×1

	rec := doUser(http.MethodGet, "/api/user/models", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "models: %s", rec.Body.String())
	var got []userapi.CatalogGroup
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)

	byName := map[string]userapi.CatalogGroup{}
	for _, g := range got {
		byName[*g.Group.Name] = g
	}

	// 有效倍率：专属覆盖 ×2 / 无覆盖默认 ×1。
	require.Equal(t, 2.0, *byName["g1"].Group.EffectiveMultiplier, "专属覆盖优先")
	require.Equal(t, 1.0, *byName["g2"].Group.EffectiveMultiplier, "无覆盖 → 组默认")

	modelByName := func(g userapi.CatalogGroup, name string) userapi.CatalogModel {
		for _, m := range g.Models {
			if m.Model == name {
				return m
			}
		}
		t.Fatalf("model %q not found in group %v", name, *g.Group.Name)
		return userapi.CatalogModel{}
	}

	// token 模型：价格毫分 → USD；Provider/SupportsPromptCaching 透出；变体升序。
	mt := modelByName(byName["g1"], "m-token")
	require.Equal(t, "token", *mt.Mode)
	require.Equal(t, 2.5, *mt.InputPerM)
	require.Equal(t, 10.0, *mt.OutputPerM)
	require.Equal(t, 0.25, *mt.CacheReadPerM)
	require.Equal(t, 1.25, *mt.CacheWritePerM)
	require.Equal(t, "openai", *mt.Provider)
	require.True(t, *mt.SupportsPromptCaching)
	require.Len(t, mt.Variants, 2)
	require.Equal(t, 1, mt.Variants[0].Seq)
	require.Equal(t, "priority", *mt.Variants[0].ServiceTier)
	require.Equal(t, 3.0, *mt.Variants[0].SetInputPerM)
	require.Equal(t, 2, mt.Variants[1].Seq)
	require.Equal(t, 1.5, *mt.Variants[1].Multiplier)
	require.Equal(t, int64(128000), *mt.Variants[1].CtxMin)

	// 无价模型：Mode=null + 价格字段全 null + 无变体。
	mn := modelByName(byName["g1"], "m-noprice")
	require.Nil(t, mn.Mode)
	require.Nil(t, mn.InputPerM)
	require.Nil(t, mn.OutputPerM)
	require.Nil(t, mn.PricePerImage)
	require.Empty(t, mn.Variants)

	// 同一模型跨组重复不去重：g2 亦有 m-token，价同。
	require.Equal(t, 2.5, *modelByName(byName["g2"], "m-token").InputPerM)
}

// TestUserModelsProviderNotInjected 降级：Scheduler 不提供组模型能力（未装配）→
// 组 Models=[]（不整页报错）；有效倍率回落组默认。
func TestUserModelsProviderNotInjected(t *testing.T) {
	doAdmin, doUser, _, _ := newModelCatalogRouters(t, nil)
	createGroup(t, doAdmin, `{"name":"g1"}`)
	token, _ := registerAndGet(t, doUser, "degraded@example.com")

	rec := doUser(http.MethodGet, "/api/user/models", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "models: %s", rec.Body.String())
	var got []userapi.CatalogGroup
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	require.Empty(t, got[0].Models, "无组模型能力 → 该组空模型")
	require.Equal(t, 1.0, *got[0].Group.EffectiveMultiplier, "无覆盖 → 组默认 ×1")
}

// TestUserModelsEffectiveMultiplier 有效倍率 = 专属覆盖 ?? 组默认：无覆盖取组默认
// （含 ≠×1 的情形），覆盖存在则覆盖优先。
func TestUserModelsEffectiveMultiplier(t *testing.T) {
	doAdmin, doUser, store, _ := newModelCatalogRouters(t, routeSched{byGroup: map[int64][]string{}})
	gid := createGroup(t, doAdmin, `{"name":"g15","price_multiplier":1.5}`)
	token, uid := registerAndGet(t, doUser, "gdef@example.com")

	effective := func() float64 {
		rec := doUser(http.MethodGet, "/api/user/models", "", token)
		require.Equal(t, http.StatusOK, rec.Code, "models: %s", rec.Body.String())
		var got []userapi.CatalogGroup
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		require.NotNil(t, got[0].Group.EffectiveMultiplier)
		return *got[0].Group.EffectiveMultiplier
	}

	require.Equal(t, 1.5, effective(), "无覆盖 → 组默认 ×1.5")

	grantOverride(t, store, gid, uid, 5000) // 专属覆盖 ×0.5
	require.Equal(t, 0.5, effective(), "专属覆盖优先 ×0.5")
}

// TestUserGroupsEffectiveMultiplier A4：/api/user/groups 附加 EffectiveMultiplier
// （专属覆盖 / 无覆盖默认）；既有 Group.PriceMultiplier 语义不变（组默认）。
func TestUserGroupsEffectiveMultiplier(t *testing.T) {
	doAdmin, doUser, store, _ := newModelCatalogRouters(t, nil)
	g1 := createGroup(t, doAdmin, `{"name":"g1","price_multiplier":1.5}`)
	createGroup(t, doAdmin, `{"name":"g2"}`)

	token, uid := registerAndGet(t, doUser, "groups-eff@example.com")
	grantOverride(t, store, g1, uid, 20000) // 覆盖 ×2；g2 无 → 默认 ×1

	rec := doUser(http.MethodGet, "/api/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "groups: %s", rec.Body.String())
	var groups []userapi.UserGroup
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 2)

	byName := map[string]userapi.UserGroup{}
	for _, g := range groups {
		byName[*g.Name] = g
	}
	require.Equal(t, 2.0, *byName["g1"].EffectiveMultiplier, "专属覆盖优先")
	require.Equal(t, 1.0, *byName["g2"].EffectiveMultiplier, "无覆盖 → 组默认")
	require.Equal(t, 1.5, *byName["g1"].PriceMultiplier, "组默认语义不变（与有效倍率非冗余）")
}
