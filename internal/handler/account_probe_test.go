// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

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

// TestPostAccountsIdTestContentFields 三态 input/image_mode/image_url 的线格式
// 投影与 result 新字段：缺席/null 同义、显式空串保留给 service 判 400；响应
// 恒有 output_preview 与 image_previews（数组，不得 null）。
func TestPostAccountsIdTestContentFields(t *testing.T) {
	prober := &probeStub{result: domain.ProbeResult{
		Success: true, StatusCode: 200, OutputPreview: "hello",
		ImagePreviews: []string{"data:image/png;base64,AA=="},
	}}
	api, store, accID := newProbeTestHandler(t, prober)
	tpl := store.tpls[1]
	tpl.SupportedFormats = append(tpl.SupportedFormats, domain.FormatOpenAIImages, domain.FormatOpenAISearch)
	tpl.FormatModels = map[domain.RequestFormat][]string{
		domain.FormatOpenAIChat:   {"m1"},
		domain.FormatOpenAIImages: {"m1"},
		domain.FormatOpenAISearch: {"m1"},
	}
	const dataURL = "data:image/png;base64,iVBORw0KGgo="

	call := func(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		api.PostAccountsIdTest(rec, req, accID)
		var decoded map[string]any
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded), rec.Body.String())
		}
		return rec, decoded
	}

	// images generations 显式 input + mode；result 新字段原样回显。
	rec, got := call(t, `{"model":"m1","format":"openai-images","input":"a cat","image_mode":"generations"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "a cat", prober.spec.Input)
	require.Equal(t, domain.ProbeImageModeGenerations, prober.spec.ImageMode)
	require.Empty(t, prober.spec.ImageURL)
	require.Equal(t, "hello", got["output_preview"])
	require.Equal(t, []any{"data:image/png;base64,AA=="}, got["image_previews"])

	// 缺席/null → 格式默认值；image_mode 缺省 → generations。
	rec, _ = call(t, `{"model":"m1","format":"openai-images"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, domain.ProbeDefaultInputImages, prober.spec.Input)
	require.Equal(t, domain.ProbeImageModeGenerations, prober.spec.ImageMode)
	rec, _ = call(t, `{"model":"m1","format":"openai-images","input":null,"image_mode":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, domain.ProbeDefaultInputImages, prober.spec.Input)
	require.Equal(t, domain.ProbeImageModeGenerations, prober.spec.ImageMode)

	// 显式空串 input / image_mode → service 400（不被压成缺省）。
	rec, _ = call(t, `{"model":"m1","format":"openai-images","input":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorInputEmpty)
	rec, _ = call(t, `{"model":"m1","format":"openai-images","image_mode":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorImageModeInvalid)

	// edits：image_url 必填且原样进入 spec。
	rec, _ = call(t, `{"model":"m1","format":"openai-images","image_mode":"edits","image_url":"`+dataURL+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, domain.ProbeImageModeEdits, prober.spec.ImageMode)
	require.Equal(t, dataURL, prober.spec.ImageURL)
	require.Equal(t, domain.ProbeDefaultInputEdits, prober.spec.Input)
	rec, _ = call(t, `{"model":"m1","format":"openai-images","image_mode":"edits"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorImageURLRequired)

	// 非 images：任何非 null image_mode/image_url → 400；显式 null 视为缺省。
	rec, _ = call(t, `{"model":"m1","format":"openai-chat","image_mode":"generations"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorImageModeUnexpected)
	rec, _ = call(t, `{"model":"m1","format":"openai-chat","image_url":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorImageURLInvalid)
	rec, _ = call(t, `{"model":"m1","format":"openai-chat","image_mode":null,"image_url":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, prober.spec.ImageMode)

	// search 缺省 input。
	rec, _ = call(t, `{"model":"m1","format":"openai-search"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, domain.FormatOpenAISearch, prober.spec.Format)
	require.Equal(t, domain.ProbeDefaultInputSearch, prober.spec.Input)

	// image_previews 恒非 null（空数组）。
	prober.result = domain.ProbeResult{Success: true, StatusCode: 200}
	rec, got = call(t, `{"model":"m1","format":"openai-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "", got["output_preview"])
	require.Equal(t, []any{}, got["image_previews"], "空预览必须是 [] 而不是 null")
}

// TestPostAccountsIdTestBodyLimit 探测端点 8 MiB 上限（其他端点仍 1 MiB）：
// 2 MiB 的 input 能通过解码进入 service（400 超长），9 MiB 在解码层即 400。
func TestPostAccountsIdTestBodyLimit(t *testing.T) {
	prober := &probeStub{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	api, _, accID := newProbeTestHandler(t, prober)

	bigInput := strings.Repeat("a", 2<<20)
	body := `{"model":"m1","format":"openai-chat","input":"` + bigInput + `"}`
	req := httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), domain.ProbeErrorInputTooLarge,
		"2 MiB 体必须越过 1 MiB 解码上限、进入 service 的长度校验")
	require.Zero(t, prober.calls)

	hugeInput := strings.Repeat("a", 9<<20)
	body = `{"model":"m1","format":"openai-chat","input":"` + hugeInput + `"}`
	req = httptest.NewRequest(http.MethodPost, "/accounts/2/test", strings.NewReader(body))
	rec = httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid json", "9 MiB 体必须在 8 MiB 解码上限被拒")
	require.Zero(t, prober.calls)

	// trailing data 仍被 DecodeLimit 拒绝。
	req = httptest.NewRequest(http.MethodPost, "/accounts/2/test",
		strings.NewReader(`{"model":"m1","format":"openai-chat"} null`))
	rec = httptest.NewRecorder()
	api.PostAccountsIdTest(rec, req, accID)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "trailing")
}

// TestProbeSchemaContract OpenAPI 是探测 API 形状的唯一来源：锁定 nullable、
// enum、maxLength、required 与数组类型（生成物不得手改）。
func TestProbeSchemaContract(t *testing.T) {
	raw, err := os.ReadFile("../../openapi/openapi.yaml")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	reqSchema := schemas["AccountTestRequest"].(map[string]any)
	resultSchema := schemas["AccountTestResult"].(map[string]any)

	requiredStrings := func(schema map[string]any) []string {
		rawList := schema["required"].([]any)
		out := make([]string, 0, len(rawList))
		for _, v := range rawList {
			out = append(out, v.(string))
		}
		return out
	}
	require.Subset(t, requiredStrings(reqSchema), []string{"model", "format"})
	require.Subset(t, requiredStrings(resultSchema), []string{"output_preview", "image_previews"})

	reqProps := reqSchema["properties"].(map[string]any)
	for _, field := range []string{"input", "image_mode", "image_url"} {
		prop := reqProps[field].(map[string]any)
		require.Equal(t, true, prop["nullable"], "%s 必须声明 nullable", field)
	}
	require.Equal(t, 8192, reqProps["input"].(map[string]any)["maxLength"])
	require.Equal(t, 6291456, reqProps["image_url"].(map[string]any)["maxLength"])
	require.Equal(t, []any{"generations", "edits"}, reqProps["image_mode"].(map[string]any)["enum"])
	formatEnum := reqProps["format"].(map[string]any)["enum"].([]any)
	require.Subset(t, formatEnum, []any{"openai-images", "openai-search"})
	require.Len(t, formatEnum, 6)

	resultProps := resultSchema["properties"].(map[string]any)
	require.Equal(t, "string", resultProps["output_preview"].(map[string]any)["type"])
	previews := resultProps["image_previews"].(map[string]any)
	require.Equal(t, "array", previews["type"])
	require.Equal(t, "string", previews["items"].(map[string]any)["type"])
}
