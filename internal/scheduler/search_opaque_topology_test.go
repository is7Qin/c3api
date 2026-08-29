// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// TestSearchRouteTopologyPreserved verifies removal of Search bucket and fallback:
// buildRoutes must not produce FormatOpenAISearch routes; Select with Search fails;
// SelectOpaque reuses Responses bucket opaquely.
func TestSearchRouteTopologyPreserved(t *testing.T) {
	tpl := &domain.Template{
		ID: 1, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		ModelMapping:     map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}},
	}
	a := &domain.Account{ID: 10, TemplateID: 1, Template: tpl, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	s := newTestScheduler(t, []*domain.Account{a})

	// 同一快照内：Select(Responses) 应用映射，SelectOpaque(Responses) 透明
	sel, err := s.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, "upstream-b", sel.Model, "Select 命中映射")
	require.Equal(t, domain.ModelMappingModeImplicit, sel.ModelMappingMode)
	s.Release(sel.AccountID)

	sel2, err := s.SelectOpaque(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, "gpt-4o", sel2.Model, "SelectOpaque 跳过映射，保持客户端模型")
	require.Equal(t, domain.ModelMappingModeInvalid, sel2.ModelMappingMode)
	require.Equal(t, "", sel2.LogMappedModel("gpt-4o"), "opaque MappedModel 为空")
	s.Release(sel2.AccountID)

	// Search 桶不存在：直接 Select(Search) 恒 404，不回退
	_, err = s.Select(10, domain.FormatOpenAISearch, "gpt-4o")
	require.ErrorIs(t, err, ErrFormatUnavailable, "移除 Search 桶后 Search 格式不可用，无回退")

	// SelectOpaque(Search) 同样 404（无 Search 桶），证明 topology 未扩增
	_, err = s.SelectOpaque(10, domain.FormatOpenAISearch, "gpt-4o")
	require.ErrorIs(t, err, ErrFormatUnavailable)

	// 历史可用性：Search 经 Responses 桶仍可选
	sel3, err := s.SelectOpaque(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err, "历史模板仅声明 responses，Search 复用 responses 桶应成功")
	require.Equal(t, domain.FormatOpenAIResponses, sel3.Format, "桶格式为 responses")
	s.Release(sel3.AccountID)

	// 非 Search 行为不变：Responses 映射仍生效
	sel4, err := s.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, "upstream-b", sel4.Model)
	s.Release(sel4.AccountID)
}

// TestSearchOpaqueDoesNotAlterEligibility ensures mapping does not affect Search eligibility
// via opaque path: alias that only exists as mapping key should not grant extra eligibility.
func TestSearchOpaqueDoesNotAlterEligibility(t *testing.T) {
	// 模板仅有 mapping 别名，无 Models/FormatModels 对该别名的显式支持？但 HasModelSpace 仍 true
	// 此用例验证：Search 经 SelectOpaque 仍走 Responses 桶的现有路由，不因 mapping 新建 Search 路由
	tpl := &domain.Template{
		ID: 1, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		ModelMapping:     map[string]domain.ModelMappingEntry{"alias": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeExplicit}},
	}
	a := &domain.Account{ID: 10, TemplateID: 1, Template: tpl, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	s := newTestScheduler(t, []*domain.Account{a})

	// 普通 Responses 对 alias 命中 mapping 的 tier1 (Serves via mapping)
	sel, err := s.Select(10, domain.FormatOpenAIResponses, "alias")
	require.NoError(t, err)
	require.Equal(t, "gpt-4o", sel.Model)
	s.Release(sel.AccountID)

	// Search 透明同样经 Responses 桶对 alias 可选，但返回 alias（不映射到 gpt-4o）
	sel2, err := s.SelectOpaque(10, domain.FormatOpenAIResponses, "alias")
	require.NoError(t, err)
	require.Equal(t, "alias", sel2.Model, "Search 透明不映射")
	s.Release(sel2.AccountID)

	// buildRoutes must not have Search entry regardless of mapping
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	loader := newMemLoader(map[int64][]*domain.Account{10: {a}})
	ns := New(testCfg(), loader, re, nil)
	require.NoError(t, ns.reload(context.Background()))
	groups, _ := ns.store.groups.Load().(map[int64]*groupSnapshot)
	gs := groups[10]
	_, hasSearch := gs.routes[routeKey{domain.FormatOpenAISearch, "gpt-4o"}]
	require.False(t, hasSearch, "buildRoutes 不应产生 Search 桶")
	_, hasSearchDefault := gs.routes[routeKey{domain.FormatOpenAISearch, ""}]
	require.False(t, hasSearchDefault)
}
