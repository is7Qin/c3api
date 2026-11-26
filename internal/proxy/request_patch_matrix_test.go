// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch HTTP 数据面矩阵（spec custom-request-patch §9 P3 / §11.2/3/4/7/8）：
// 真实 fake upstream（httptest）逐字节断言上游收到的头/体 = 既有变换 + patch。
//   - 六格式（chat/responses/anthropic/converted/images gen/edits/search）各至少
//     一条接线点；
//   - 头：全局层 + 格式层覆盖/Del、Authorization/User-Agent/Content-Type/Accept/
//     Host 覆盖与删除、运行期跳 Accept-Encoding（脏库值）；
//   - body：delete→set 排序、set 胜、大整数字面量、model 改写既有语义不变；
//   - 非 custom 模板行为逐字节不变（Upstream nil ⇒ helper nil）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egressapi"
	"github.com/is7qin/c3api/pkg/logx"
)

// patchReq 一次上游请求的捕获快照。
type patchReq struct {
	Method string
	Path   string
	Host   string
	Header http.Header
	Body   []byte
}

// patchCapture 记录型上游：按固定 status/body 回放，逐请求捕获事实。
type patchCapture struct {
	mu   sync.Mutex
	reqs []patchReq
}

// builtHeaderCapture 在 Transport 写请求行之前捕获网关自建出栈头（与
// pkg/aiclient 测试同款手段）：能区分「网关没设」与「Transport 自动补了
// Accept-Encoding」——后者在真实上游永远可见，无法当判据。
type builtHeaderCapture struct {
	mu   sync.Mutex
	last http.Header
}

func (c *builtHeaderCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.last = req.Header.Clone()
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (c *builtHeaderCapture) header(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.last, "上游请求未发出，出栈头无从捕获")
	return c.last
}

// staticEgressPool 把测试 transport 注入调度器出口（生产走 pkg/egress.Pool；
// 这里只需要 Direct() 给出同一条 client，未绑定账号即走它）。
type staticEgressPool struct{ client *egressapi.Client }

func (p staticEgressPool) Direct() *egressapi.Client                        { return p.client }
func (p staticEgressPool) For(egressapi.Spec) *egressapi.Client             { return p.client }
func (p staticEgressPool) Detach(map[[16]byte]struct{}) []*egressapi.Client { return nil }
func (p staticEgressPool) Size() int                                        { return 0 }

func (c *patchCapture) snapshot() []patchReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]patchReq, len(c.reqs))
	copy(out, c.reqs)
	return out
}

func (c *patchCapture) only(t *testing.T) patchReq {
	t.Helper()
	reqs := c.snapshot()
	require.Len(t, reqs, 1, "期望恰好一次上游调用")
	return reqs[0]
}

// newPatchUpstream 单响应上游（status + body），捕获每个请求。
func newPatchUpstream(t *testing.T, status int, respBody string) (*httptest.Server, *patchCapture) {
	t.Helper()
	cap := &patchCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.reqs = append(cap.reqs, patchReq{Method: r.Method, Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone(), Body: body})
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// newPatchProxy 构造 request_patch 场景代理：自定义模板 + 可注入账号/转换方向/
// 日志/错误明细捕获（水平与 newTestProxyTplAccountsRec 同构，额外放开 template
// Upstream 字段与 warn 日志）。
func newPatchProxy(t *testing.T, tpl *domain.Template, accs map[int64][]*domain.Account, converts []domain.ProtocolConvert, logs usage.LogInserter, logOut string, builtRt http.RoundTripper) *Proxy {
	t.Helper()
	if accs == nil {
		accs = map[int64][]*domain.Account{10: {{
			ID: 1, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-upstream",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 8,
		}}}
	}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 3,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 10 * time.Second,
		UsageCapture:          true,
	}
	// 出口 client：builtRt 注入时**调度器出口与 Factory 底座同一条**（生产同源；
	// 唯一能观察到网关自建出栈头的位置——Transport 自动补的 Accept-Encoding 在
	// 真实上游永远可见，不能当「网关是否设了」的判据）。
	transport := http.RoundTripper(http.DefaultTransport)
	if builtRt != nil {
		transport = builtRt
	}
	hc := &http.Client{Transport: transport}
	var egressCfg scheduler.EgressPool
	if builtRt != nil {
		egressCfg = staticEgressPool{client: egressapi.NewClient(egressapi.Spec{}, [egressapi.NumFlavours]*http.Client{hc, hc})}
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: egressCfg}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	if logs == nil {
		logs = &captureLogStore{}
	}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, logs, nil)
	key := activeKey(1, 1, 10)
	key.ProtocolConverts = converts
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": key}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 10 * time.Second})
	var logger *logx.Logger
	if logOut != "" {
		var err error
		logger, err = logx.New("warn", logOut)
		require.NoError(t, err)
	}
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errLogStoreFrom(logs), nil)
	return New(cfg, sched, rec, clients, auth, logger, nil, errlogW, Deps{})
}

// customPatchTpl 构造 custom 模板；patchWire 非空时经 **生产解析入口**
// domain.ParseRequestPatch 归一（与快照读路径同源：规范形头名/空层归一/大整数）。
func customPatchTpl(t *testing.T, id int64, upstream string, formats []domain.RequestFormat, patchWire string) *domain.Template {
	t.Helper()
	var patch *domain.RequestPatch
	if patchWire != "" {
		p, err := domain.ParseRequestPatch(json.RawMessage(patchWire))
		require.NoError(t, err, "夹具 patch 必须过生产解析入口")
		patch = p
	}
	return &domain.Template{
		ID: id, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeCustom,
		SupportedFormats: formats,
		Models:           []string{"gpt-4o", "gpt-image-1"},
		Upstream:         domain.NewTemplateUpstream(nil, patch),
	}
}

// postViaRouter 经真实路由发一条 /v1 请求（路径选择格式）。
func postViaRouter(p *Proxy, path string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	AIRouter(p).ServeHTTP(rec, req)
	return rec
}

// buildImagesEditsMultipart 构造 images edits multipart body（真实 boundary）。
func buildImagesEditsMultipart(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "gpt-image-1"))
	require.NoError(t, mw.WriteField("prompt", "turn this into watercolor"))
	fw, err := mw.CreateFormFile("image", "a.png")
	require.NoError(t, err)
	_, err = fw.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// TestRequestPatchChatHeadersAndBody 头两层语义 + body delete→set：
//   - 全局层 Set/Del，格式层覆盖全局层（Set 覆盖与 Del 覆盖三态）；
//   - Authorization/User-Agent/Content-Type/Accept/Host 覆盖（配置面权威）；
//   - {api_key} 用当次 attempt 原始 cred 展开；
//   - body：delete 排序 → set 排序，set 胜；大整数逐字；既有 model 改写不变。
func TestRequestPatchChatHeadersAndBody(t *testing.T) {
	const okBody = `{"id":"c1","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	patch := `{
		"headers":{
			"User-Agent":"vibe-code/1.0",
			"x-call-source":"vibe_code",
			"Authorization":"Bearer {api_key}",
			"Accept":"application/global",
			"Content-Type":"application/vnd.custom+json",
			"Host":"virtual.example",
			"X-Drop":"",
			"X-Global-Only":"global"
		},
		"formats":{"openai-chat":{
			"headers":{"Accept":"application/format-wins","X-Layer":"format","X-Global-Only":""},
			"set":{"metadata.call_source":"vibe_code","max_tokens":4096,"big":9007199254740993,"same_path":"set-wins"},
			"delete":["metadata.client_field","temperature","same_path"]
		}}
	}`
	up, capture := newPatchUpstream(t, http.StatusOK, okBody)
	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, patch)
	built := &builtHeaderCapture{}
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", built)

	clientBody := []byte(`{"model":"gpt-4o","messages":[],"metadata":{"client_field":"drop-me"},"temperature":0.2}`)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{
		"Authorization":   "Bearer ck-1",
		"X-Drop":          "client-value",
		"X-Global-Only":   "client-value",
		"Accept-Encoding": "gzip, br",
	}, clientBody)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := capture.only(t)
	h := got.Header
	require.Equal(t, "Bearer sk-upstream", h.Get("Authorization"), "{api_key} 用当次 attempt 原始 cred 展开（覆盖网关鉴权）")
	require.Equal(t, "vibe-code/1.0", h.Get("User-Agent"), "配置面可覆盖 UA")
	require.Equal(t, "vibe_code", h.Get("X-Call-Source"))
	require.Equal(t, "application/format-wins", h.Get("Accept"), "格式层覆盖全局层")
	require.Equal(t, "application/vnd.custom+json", h.Get("Content-Type"), "可覆盖 Content-Type")
	require.Equal(t, "virtual.example", got.Host, "Host 特例走 req.Host")
	_, hasDrop := h["X-Drop"]
	require.False(t, hasDrop, "空值 = Del 客户端头（全局层）")
	_, hasGlobalOnly := h["X-Global-Only"]
	require.False(t, hasGlobalOnly, "格式层空值覆盖全局层 Set（三态覆盖语义）")
	require.Equal(t, "format", h.Get("X-Layer"))
	_, hasAE := built.header(t)["Accept-Encoding"]
	require.False(t, hasAE, "出站永不出现 Accept-Encoding（入站 relayDeny 剔除 + 出站不变量）")

	upBody := got.Body
	require.Equal(t, "vibe_code", gjson.GetBytes(upBody, "metadata.call_source").String(), "body set 生效")
	require.False(t, gjson.GetBytes(upBody, "metadata.client_field").Exists(), "body delete 生效")
	require.False(t, gjson.GetBytes(upBody, "temperature").Exists(), "body delete 生效（顶层）")
	require.EqualValues(t, 4096, gjson.GetBytes(upBody, "max_tokens").Int(), "同一 path delete+set ⇒ set 胜")
	require.Equal(t, "set-wins", gjson.GetBytes(upBody, "same_path").String(), "同一 path delete+set ⇒ set 胜")
	require.Contains(t, string(upBody), `"big":9007199254740993`, "set 大整数逐字保留（不经 float64）")
	require.Equal(t, "gpt-4o", gjson.GetBytes(upBody, "model").String(), "既有 model 改写语义不变")
}

// TestRequestPatchHostLowercaseAndDirtyAcceptEncoding 保存期归一的规范形契约：
// 小写 host 配置经 ParseRequestPatch 归一后仍按 Host 特例生效；脏库（手工构造、
// 绕过保存期）的非空 Accept-Encoding 在运行期被跳过。
func TestRequestPatchHostLowercaseAndDirtyAcceptEncoding(t *testing.T) {
	up, capture := newPatchUpstream(t, http.StatusOK, `{"id":"c1"}`)
	parsed, err := domain.ParseRequestPatch(json.RawMessage(`{"headers":{"host":"lower.example","authorization":"Bearer {api_key}"}}`))
	require.NoError(t, err)
	require.Equal(t, "lower.example", parsed.Headers["Host"], "解析期必须把 host 归一为 Host 规范形")
	require.Equal(t, "Bearer {api_key}", parsed.Headers["Authorization"])

	// 脏库值：手工构造（保存期会 400）——直连 domain.RequestPatch，模拟手工改库。
	parsed.Headers["Accept-Encoding"] = "gzip"
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeCustom,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		Upstream: domain.NewTemplateUpstream(nil, parsed),
	}
	built := &builtHeaderCapture{}
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", built)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
		[]byte(`{"model":"gpt-4o","messages":[]}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	got := capture.only(t)
	require.Equal(t, "lower.example", got.Host, "小写 host 归一后覆盖生效")
	require.Equal(t, "Bearer sk-upstream", got.Header.Get("Authorization"))
	_, hasAE := built.header(t)["Accept-Encoding"]
	require.False(t, hasAE, "脏库 Accept-Encoding 运行期强制跳过（出栈头不含该键）")
}

// TestRequestPatchFormatMatrix 六格式（含转换/图片双端点/search）逐点：上游收到
// 该格式的格式层头 + body splice；images edits multipart 只注入头、body 逐字节不变。
func TestRequestPatchFormatMatrix(t *testing.T) {
	const respOK = `{"id":"r1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	const messOK = `{"id":"m1","type":"message","role":"assistant","model":"gpt-4o","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	const chatOK = `{"id":"c1","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	const imagesOK = `{"data":[{"b64_json":"aGk="}]}`

	cases := []struct {
		name       string
		formats    []domain.RequestFormat
		converts   []domain.ProtocolConvert
		clientPath string
		clientBody []byte
		patchWire  string
		patchKey   string
		respBody   string
		wantPath   string
		wantModel  string
		multipart  bool
	}{
		{
			name: "chat 非流", formats: []domain.RequestFormat{domain.FormatOpenAIChat},
			clientPath: "/v1/chat/completions", clientBody: []byte(`{"model":"gpt-4o","messages":[],"client_only":1}`),
			wantPath: "/v1/chat/completions", patchKey: string(domain.UpstreamPathChat), respBody: chatOK,
			patchWire: `{"formats":{"openai-chat":{"headers":{"X-Patched":"chat"},"set":{"patched":"chat"},"delete":["client_only"]}}}`,
			wantModel: "gpt-4o",
		},
		{
			name: "responses 非流", formats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			clientPath: "/v1/responses", clientBody: []byte(`{"model":"gpt-4o","input":"hi","client_only":1}`),
			wantPath: "/v1/responses", patchKey: string(domain.UpstreamPathResponses), respBody: respOK,
			patchWire: `{"formats":{"openai-responses":{"headers":{"X-Patched":"responses"},"set":{"patched":"responses"},"delete":["client_only"]}}}`,
			wantModel: "gpt-4o",
		},
		{
			name: "anthropic 非流", formats: []domain.RequestFormat{domain.FormatAnthropic},
			clientPath: "/v1/messages", clientBody: []byte(`{"model":"gpt-4o","max_tokens":16,"messages":[],"client_only":1}`),
			wantPath: "/v1/messages", patchKey: string(domain.UpstreamPathAnthropic), respBody: messOK,
			patchWire: `{"formats":{"anthropic":{"headers":{"X-Patched":"anthropic"},"set":{"patched":"anthropic"},"delete":["client_only"]}}}`,
			wantModel: "gpt-4o",
		},
		{
			name: "converted chat to resp", formats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			converts:   []domain.ProtocolConvert{domain.ProtocolConvertChatToResp},
			clientPath: "/v1/chat/completions", clientBody: []byte(`{"model":"gpt-4o","messages":[],"client_only":1}`),
			wantPath: "/v1/responses", patchKey: string(domain.UpstreamPathResponses), respBody: respOK,
			patchWire: `{"formats":{"openai-responses":{"headers":{"X-Patched":"converted"},"set":{"patched":"converted"},"delete":["client_only"]}}}`,
			wantModel: "gpt-4o",
		},
		{
			name: "images generations JSON", formats: []domain.RequestFormat{domain.FormatOpenAIImages},
			clientPath: "/v1/images/generations", clientBody: []byte(`{"model":"gpt-image-1","prompt":"a cat","client_only":1}`),
			wantPath: "/v1/images/generations", patchKey: string(domain.UpstreamPathImagesGen), respBody: imagesOK,
			patchWire: `{"formats":{"openai-images.generations":{"headers":{"X-Patched":"images-gen"},"set":{"patched":"images-gen"},"delete":["client_only"]}}}`,
			wantModel: "gpt-image-1",
		},
		{
			name: "images edits multipart 只头", formats: []domain.RequestFormat{domain.FormatOpenAIImages},
			clientPath: "/v1/images/edits", multipart: true,
			wantPath: "/v1/images/edits", patchKey: string(domain.UpstreamPathImagesEdits), respBody: imagesOK,
			patchWire: `{"formats":{"openai-images.edits":{"headers":{"X-Patched":"images-edits"},"set":{"patched":"must-not-apply"}}}}`,
		},
		{
			name: "search 静态透传", formats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			clientPath: "/v1/alpha/search", clientBody: []byte(`{"model":"gpt-4o","input":"q","client_only":1}`),
			wantPath: "/v1/alpha/search", patchKey: string(domain.UpstreamPathSearch), respBody: `{"output":"done"}`,
			patchWire: `{"formats":{"openai-search":{"headers":{"X-Patched":"search"},"set":{"patched":"search"},"delete":["client_only"]}}}`,
			wantModel: "gpt-4o",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, capture := newPatchUpstream(t, http.StatusOK, tc.respBody)
			tpl := customPatchTpl(t, 1, up.URL, tc.formats, tc.patchWire)
			p := newPatchProxy(t, tpl, nil, tc.converts, &captureLogStore{}, "", nil)

			headers := map[string]string{"Authorization": "Bearer ck-1", "x-api-key": "ck-1"}
			body := tc.clientBody
			if tc.multipart {
				body, headers["Content-Type"] = buildImagesEditsMultipart(t)
			}
			rec := postViaRouter(p, tc.clientPath, headers, body)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			got := capture.only(t)
			require.Equal(t, tc.wantPath, got.Path, "既有路径派生不变")
			// X-Patched 的值是各 case 自己的标签：断言格式层头**按该出站格式的键**
			// 命中（打错键则任何格式层头都不会出现，这里直接为空）。
			wantPatched := map[string]string{
				string(domain.UpstreamPathChat):        "chat",
				string(domain.UpstreamPathResponses):   "responses",
				string(domain.UpstreamPathAnthropic):   "anthropic",
				string(domain.UpstreamPathImagesGen):   "images-gen",
				string(domain.UpstreamPathImagesEdits): "images-edits",
				string(domain.UpstreamPathSearch):      "search",
			}[tc.patchKey]
			if tc.converts != nil {
				wantPatched = "converted"
			}
			require.Equal(t, wantPatched, got.Header.Get("X-Patched"), "格式层头按该出站格式的键命中")

			if tc.multipart {
				require.Equal(t, body, got.Body, "multipart body 结构性跳过：逐字节原样透传")
				require.False(t, gjson.ValidBytes(got.Body), "前提：multipart 不是 JSON")
				return
			}
			require.Equal(t, wantPatched, gjson.GetBytes(got.Body, "patched").String(), "body splice 生效（值 = 该 case 的格式标签）")
			require.False(t, gjson.GetBytes(got.Body, "client_only").Exists(), "body delete 生效")
			if tc.wantModel != "" {
				require.Equal(t, tc.wantModel, gjson.GetBytes(got.Body, "model").String(), "model 字段保真")
			}
		})
	}
}

// TestRequestPatchNonCustomTemplateUnchanged 非 custom 模板（Upstream nil）⇒ 所有
// helper nil 短路：上游头/体与改动前逐字节一致（无 X-Patched、无 body 改写）。
func TestRequestPatchNonCustomTemplateUnchanged(t *testing.T) {
	up, capture := newPatchUpstream(t, http.StatusOK, `{"id":"c1"}`)
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	body := []byte(`{"model":"gpt-4o","messages":[],"metadata":{"k":"v"}}`)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"}, body)
	require.Equal(t, http.StatusOK, rec.Code)
	got := capture.only(t)
	require.Empty(t, got.Header.Get("X-Patched"))
	require.JSONEq(t, string(body), string(got.Body), "非 custom：body 无任何 patch 改写")
}

// TestRequestPatchSuccessPathNoPatchLogs 成功路径零 patch 日志（spec §8：只失败限频）。
func TestRequestPatchSuccessPathNoPatchLogs(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-patch-log-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	up, _ := newPatchUpstream(t, http.StatusOK, `{"id":"c1"}`)
	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		`{"headers":{"X-Patch":"1"},"formats":{"openai-chat":{"set":{"patched":"v"}}}}`)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, logOut, nil)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
		[]byte(`{"model":"gpt-4o","messages":[]}`))
	require.Equal(t, http.StatusOK, rec.Code)

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	require.NotContains(t, string(data), "request patch", "成功路径零 patch 日志")
	require.Empty(t, strings.TrimSpace(string(data)), "成功路径不得有 WARN")
}

// TestRequestPatchHostTriStateAcrossLayers Host 特例的**按层三态**（spec §2.4/§2.6；
// P3 评审 oracle I1 + M1）：
//   - 全局层 Set + 格式层 ""（Del）⇒ 格式层覆盖 ⇒ req.Host 清空、出站回落 URL.Host；
//   - 仅格式层 ""（无全局层）⇒ 回落 URL.Host；
//   - 全局层 "" + 格式层 Set ⇒ 格式层生效；
//   - 全局层 Set（格式层不设 Host）⇒ 全局层生效（既有语义不回退）；
//   - Host 值里的 {api_key} 在写入 req.Host 前用当次 attempt 原始 cred 展开。
func TestRequestPatchHostTriStateAcrossLayers(t *testing.T) {
	cases := []struct {
		name      string
		patchWire string
		wantHost  string // "" = 期望回落 httptest URL.Host
	}{
		{
			// 判别性核心用例：修复前格式层空值被 no-op，全局 Host 残留（I1）。
			name:      "global_set_format_del_falls_back_to_url_host",
			patchWire: `{"headers":{"Host":"global.example"},"formats":{"openai-chat":{"headers":{"Host":""}}}}`,
		},
		{
			name:      "format_del_only_falls_back_to_url_host",
			patchWire: `{"formats":{"openai-chat":{"headers":{"Host":"","X-Layer":"format"}}}}`,
		},
		{
			name:      "global_del_format_set_wins",
			patchWire: `{"headers":{"Host":""},"formats":{"openai-chat":{"headers":{"Host":"format.example"}}}}`,
			wantHost:  "format.example",
		},
		{
			name:      "global_set_without_format_host_stays",
			patchWire: `{"headers":{"Host":"global.example"}}`,
			wantHost:  "global.example",
		},
		{
			// M1：Host 值占位符必须展开，不得把字面 {api_key} 发给上游。
			name:      "host_value_expands_api_key",
			patchWire: `{"headers":{"Host":"{api_key}.example","X-Vendor-Key":"{api_key}"}}`,
			wantHost:  "sk-upstream.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, capture := newPatchUpstream(t, http.StatusOK, `{"id":"c1"}`)
			upURL, err := url.Parse(up.URL)
			require.NoError(t, err)

			tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, tc.patchWire)
			p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
			rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
				[]byte(`{"model":"gpt-4o","messages":[]}`))
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			got := capture.only(t)
			wantHost := tc.wantHost
			if wantHost == "" {
				wantHost = upURL.Host // req.Host = "" ⇒ 出站 Host 回退 URL.Host
			}
			require.Equal(t, wantHost, got.Host, "Host 按层三态（Set 覆盖 / Del 回落 URL.Host）")
			if tc.name == "format_del_only_falls_back_to_url_host" {
				require.Equal(t, "format", got.Header.Get("X-Layer"), "格式层其余头照常应用")
			}
			if tc.name == "host_value_expands_api_key" {
				require.Equal(t, "sk-upstream", got.Header.Get("X-Vendor-Key"), "{api_key} 同一展开语义")
			}
		})
	}
}
