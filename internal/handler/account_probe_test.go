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

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

type probeStub struct {
	calls  int
	spec   domain.ProbeSpec
	result domain.ProbeResult
}

func (p *probeStub) Probe(_ context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	p.calls++
	p.spec = spec
	res := p.result
	if res.Format == "" {
		res.Format = spec.Format
		res.RequestedModel = spec.RequestedModel
		res.UpstreamModel = spec.UpstreamModel
	}
	return res
}

// newProbeTestHandler 构造带种子模板/账号的 AdminAPI（prober 可注入 nil 验证
// 「未装配 → 500」）。
func newProbeTestHandler(t *testing.T, prober service.AccountProber) (*AdminAPI, *fakeStore, int64) {
	t.Helper()
	store := newFakeStore()
	tpl := &domain.Template{
		ID:               1,
		Name:             "probe-tpl",
		BaseURL:          "https://upstream.example",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m1"},
	}
	store.tpls[tpl.ID] = tpl
	acc := &domain.Account{
		ID:          2,
		Name:        "probe-acc",
		TemplateID:  tpl.ID,
		UpstreamKey: "sk-http",
		Enabled:     true,
	}
	store.accs[acc.ID] = acc
	store.nextID = 3
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Auth: &fakeKeys{}, EmailCodeStore: store, Prober: prober,
	})
	return New(svc), store, acc.ID
}

// TestPostAccountsIdTest handler 契约：200 结论、400 解码/校验、404 账号缺失、
// 500 探测组件未装配。探测结论 success=false 仍是 200。
func TestPostAccountsIdTest(t *testing.T) {
	prober := &probeStub{result: domain.ProbeResult{
		Success: true, LatencyMS: 12, StatusCode: 200,
		Error: "", Detail: "",
	}}
	api, _, accID := newProbeTestHandler(t, prober)
	body := `{"model":"m1","format":"openai-chat"}`

	req := httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got AccountTestResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.True(t, got.Success)
	require.Equal(t, int64(12), got.LatencyMs)
	require.Equal(t, 200, got.StatusCode)
	require.Equal(t, "openai-chat", got.Format)
	require.Equal(t, "m1", got.RequestedModel)
	require.Equal(t, "m1", got.UpstreamModel)
	require.Empty(t, got.Error)
	require.Empty(t, got.Detail)
	require.Equal(t, 1, prober.calls)
	require.Equal(t, "sk-http", prober.spec.Credential)
	require.Equal(t, int64(1), prober.spec.TemplateID)

	// success=false 的探测结论仍是 HTTP 200。
	prober.result = domain.ProbeResult{Success: false, StatusCode: 500, Error: "boom", Detail: "detail"}
	req = httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.False(t, got.Success)
	require.Equal(t, "boom", got.Error)

	// 解码失败（未知字段 / 非法 JSON）→ 400。
	for _, bad := range []string{`{"model":"m1","format":"openai-chat","url":"http://evil"}`, `{`} {
		req = httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(bad))
		rec = httptest.NewRecorder()
		api.PostAccountsIdTest(rec, req, accID)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}

	// 服务层校验失败 → 400。
	req = httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(`{"model":"","format":"openai-chat"}`))
	rec = httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 账号缺失 → 404。
	req = httptest.NewRequest(http.MethodPost, "/accounts/999/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, 999)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Prober 未装配 → 500（装配错误显形，不降级 503）。
	nilAPI, _, nilAccID := newProbeTestHandler(t, nil)
	req = httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	nilAPI.PostAccountsIdTest(rec, req, nilAccID)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestProbeSupplierSurfacePassthrough supplier surface 与管理面同一实现（同一
// 组 component/同一 service 路径）。
func TestProbeSupplierSurfacePassthrough(t *testing.T) {
	prober := &probeStub{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	api, store, accID := newProbeTestHandler(t, prober)
	surface := supplierSurface{api: api}
	store.accs[accID].SupplierUserID = 42

	req := httptest.NewRequest(http.MethodPost, "/api/user/supplier/accounts/2/test", strings.NewReader(`{"model":"m1","format":"openai-chat"}`))
	req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(42)))
	rec := httptest.NewRecorder()
	surface.PostSupplierAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, prober.calls)

	// 越域（账号归属 42 ≠ 7）⇒ 404。
	store.accs[accID].SupplierUserID = 7
	req = httptest.NewRequest(http.MethodPost, "/api/user/supplier/accounts/2/test", strings.NewReader(`{"model":"m1","format":"openai-chat"}`))
	req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(42)))
	rec = httptest.NewRecorder()
	surface.PostSupplierAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, 1, prober.calls)
}
