// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/probe"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// probeReadOnlyStore 是探测用例的**只读**替身：只实现探测组装会读的三个方法
// （外加 New 首载 settings 的 GetAllSettings）；其余方法（全部写方法）由嵌入的
// nil Store 承担 ⇒ 一旦探测路径触达任何未预期调用（写库/发布/额外读），立刻
// nil-interface panic。A8 的「fake store 写方法 panic」由此结构性成立。
type probeReadOnlyStore struct {
	Store // nil embedded: unexpected calls panic
	acc   *domain.Account
	ext   *domain.TemplateExt
	cred  *domain.AccountCredential

	extErr  error
	credErr error
	accErr  error
}

func (s *probeReadOnlyStore) GetAllSettings(context.Context) ([]*domain.Setting, error) {
	return nil, nil
}

func (s *probeReadOnlyStore) GetAccountWithTemplate(_ context.Context, id int64) (*domain.Account, error) {
	if s.accErr != nil {
		return nil, s.accErr
	}
	if s.acc == nil || s.acc.ID != id {
		return nil, fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	c := *s.acc
	return &c, nil
}

func (s *probeReadOnlyStore) GetTemplateExt(_ context.Context, _ int64) (*domain.TemplateExt, error) {
	if s.extErr != nil {
		return nil, s.extErr
	}
	if s.ext == nil {
		return nil, fmt.Errorf("%w: template ext missing", repository.ErrNotFound)
	}
	c := *s.ext
	return &c, nil
}

func (s *probeReadOnlyStore) GetOwnedAccountCredential(context.Context, int64) (*domain.AccountCredential, error) {
	if s.credErr != nil {
		return nil, s.credErr
	}
	if s.cred == nil {
		return nil, fmt.Errorf("%w: account ext missing", repository.ErrNotFound)
	}
	c := *s.cred
	return &c, nil
}

type fakeAccountProber struct {
	calls  int
	spec   domain.ProbeSpec
	result domain.ProbeResult
}

func (f *fakeAccountProber) Probe(_ context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	f.calls++
	f.spec = spec
	res := f.result
	if res.Format == "" {
		res.Format = spec.Format
		res.RequestedModel = spec.RequestedModel
		res.UpstreamModel = spec.UpstreamModel
	}
	return res
}

type recordingPublisher struct{ calls int }

func (p *recordingPublisher) Publish(context.Context, notify.Change) error {
	p.calls++
	return nil
}

func newProbeSvc(store Store, prober AccountProber, pub Publisher) *Service {
	return New(Deps{Store: store, Invalidate: NopInvalidator{}, Publisher: pub, EmailCodeStore: testEmailCodes, Prober: prober})
}

func staticProbeTemplate() *domain.Template {
	return &domain.Template{
		ID:               3,
		Name:             "tpl",
		BaseURL:          "https://upstream.example.com",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m1"},
	}
}

func probeAccountFor(tpl *domain.Template) *domain.Account {
	return &domain.Account{
		ID:          11,
		Name:        "acc",
		TemplateID:  tpl.ID,
		Template:    tpl,
		UpstreamKey: "sk-static",
		Enabled:     true,
	}
}

// TestProbeValidation A1：非法 format/model、空 model、格式/模型空间不合法 → 400；
// 合法请求才调用 Prober；上游模型经 domain.MapModel 映射、base_url 账号覆盖生效。
func TestProbeValidation(t *testing.T) {
	formatModels := staticProbeTemplate()
	formatModels.FormatModels = map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m1"}}
	formatModels.Models = nil

	modelSpace := staticProbeTemplate()
	modelSpace.Models = []string{"m1"}

	cases := []struct {
		name      string
		tpl       *domain.Template
		acc       *domain.Account
		req       AccountProbeRequest
		wantErr   string
		wantCalls int
		wantUpstr string
		wantBase  string
	}{
		{
			name:    "unsupported-format-images",
			tpl:     staticProbeTemplate(),
			req:     AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIImages},
			wantErr: "invalid probe format",
		},
		{
			name:    "unknown-format",
			tpl:     staticProbeTemplate(),
			req:     AccountProbeRequest{Model: "m1", Format: domain.RequestFormat("bogus")},
			wantErr: "invalid probe format",
		},
		{
			name:    "empty-model",
			tpl:     staticProbeTemplate(),
			req:     AccountProbeRequest{Model: "", Format: domain.FormatOpenAIChat},
			wantErr: "probe model is empty",
		},
		{
			name:    "whitespace-model",
			tpl:     staticProbeTemplate(),
			req:     AccountProbeRequest{Model: "   ", Format: domain.FormatOpenAIChat},
			wantErr: "probe model is empty",
		},
		{
			name:    "format-not-supported",
			tpl:     staticProbeTemplate(),
			req:     AccountProbeRequest{Model: "m1", Format: domain.FormatAnthropic},
			wantErr: "not eligible",
		},
		{
			name:    "format-models-reject",
			tpl:     formatModels,
			req:     AccountProbeRequest{Model: "m2", Format: domain.FormatOpenAIChat},
			wantErr: "not eligible",
		},
		{
			name:    "model-space-reject",
			tpl:     modelSpace,
			req:     AccountProbeRequest{Model: "m2", Format: domain.FormatOpenAIChat},
			wantErr: "not eligible",
		},
		{
			name:      "valid-with-mapping-and-override",
			tpl:       staticProbeTemplate(),
			req:       AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat},
			wantCalls: 1,
			wantUpstr: "upstream-m1",
			wantBase:  "https://account.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := probeAccountFor(tc.tpl)
			if tc.wantUpstr != "" {
				tc.tpl.ModelMapping = domain.ModelMapping{"m1": {MappedModel: tc.wantUpstr, Mode: domain.ModelMappingModeExplicit}}
				override := tc.wantBase
				acc.BaseURL = &override
			}
			prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
			svc := newProbeSvc(&probeReadOnlyStore{acc: acc}, prober, nil)
			res, err := svc.ProbeAccount(context.Background(), acc.ID, tc.req)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidInput)
				require.Contains(t, err.Error(), tc.wantErr)
				require.Equal(t, 0, prober.calls, "校验失败不得调用 Prober")
				require.Nil(t, res)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.Success)
			require.Equal(t, tc.wantCalls, prober.calls)
			require.Equal(t, tc.wantUpstr, prober.spec.UpstreamModel)
			require.Equal(t, tc.wantBase, prober.spec.BaseURL)
			require.Equal(t, "m1", prober.spec.RequestedModel)
			require.Equal(t, credential.TypeAPIKey, prober.spec.CredentialType)
			require.Equal(t, "sk-static", prober.spec.Credential)
		})
	}
}

// TestProbeNoWrites A8：探测路径不触发任何写方法（嵌入 nil Store ⇒ 写方法
// panic），也不发布 NOTIFY。fake prober 只观测一次调用。
func TestProbeNoWrites(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{acc: acc}
	pub := &recordingPublisher{}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, pub)

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 1, prober.calls)
	require.Equal(t, 0, pub.calls, "探测不得发布 NOTIFY")

	// 结构性证据：任何写方法调用都会撞 nil 嵌入接口并 panic（探测路径没有）。
	require.Panics(t, func() {
		_, _ = store.CreateAccount(context.Background(), &domain.Account{Name: "should-not-write"})
	})
	require.Panics(t, func() {
		_ = store.DeleteAccount(context.Background(), acc.ID)
	})
}

// TestProbeCodexOAuthExpired A6：过期 → 失败结论 + 零网络；未过期 → 发请求。
func TestProbeCodexOAuthExpired(t *testing.T) {
	tpl := &domain.Template{
		ID:               5,
		BaseURL:          "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-5"},
	}
	acc := &domain.Account{ID: 21, TemplateID: tpl.ID, Template: tpl, Enabled: true}
	past := time.Now().Add(-time.Minute)
	store := &probeReadOnlyStore{
		acc:  acc,
		cred: &domain.AccountCredential{AccountID: acc.ID, OAuthToken: "expired-at", OAuthExpiresAt: &past},
	}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "gpt-5", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, 0, res.StatusCode)
	require.Equal(t, "codex access token expired; re-run device login", res.Error)
	require.Equal(t, 0, prober.calls, "过期凭据不得发网络请求")

	// 未过期 → prober 收到 oauth token 与固定 format。
	future := time.Now().Add(time.Hour)
	store.cred = &domain.AccountCredential{AccountID: acc.ID, OAuthToken: "fresh-at", OAuthExpiresAt: &future}
	res, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "gpt-5", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 1, prober.calls)
	require.Equal(t, "fresh-at", prober.spec.Credential)
}

// TestProbeCredentialMissing A6：ErrNotFound / token 全空 → 400，零网络。
func TestProbeCredentialMissing(t *testing.T) {
	codexTpl := func(ct credential.Type) *domain.Template {
		return &domain.Template{
			ID:               6,
			CredentialType:   ct,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			Models:           []string{"m"},
		}
	}
	cases := []struct {
		name    string
		tpl     *domain.Template
		acc     *domain.Account
		store   *probeReadOnlyStore
		wantErr string
	}{
		{
			name: "static-empty-key",
			tpl:  staticProbeTemplate(),
			acc: func() *domain.Account {
				a := probeAccountFor(staticProbeTemplate())
				a.UpstreamKey = ""
				return a
			}(),
			wantErr: "静态上游 key",
		},
		{
			name:    "codex-pat-not-found",
			tpl:     codexTpl(credential.TypeCodexPAT),
			acc:     &domain.Account{ID: 31, TemplateID: 6, Enabled: true},
			store:   &probeReadOnlyStore{credErr: fmt.Errorf("%w: missing", repository.ErrNotFound)},
			wantErr: "codex 凭据",
		},
		{
			name:    "codex-pat-empty",
			tpl:     codexTpl(credential.TypeCodexPAT),
			acc:     &domain.Account{ID: 31, TemplateID: 6, Enabled: true},
			store:   &probeReadOnlyStore{cred: &domain.AccountCredential{}},
			wantErr: "codex 凭据",
		},
		{
			name:    "codex-oauth-empty-token",
			tpl:     codexTpl(credential.TypeCodexOAuth),
			acc:     &domain.Account{ID: 31, TemplateID: 6, Enabled: true},
			store:   &probeReadOnlyStore{cred: &domain.AccountCredential{}},
			wantErr: "codex 凭据",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.acc.Template == nil {
				tc.acc.Template = tc.tpl
			}
			st := tc.store
			if st == nil {
				st = &probeReadOnlyStore{}
			}
			st.acc = tc.acc
			prober := &fakeAccountProber{result: domain.ProbeResult{Success: true}}
			svc := newProbeSvc(st, prober, nil)
			req := AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat}
			if tc.tpl.CredentialType.IsCodex() {
				req = AccountProbeRequest{Model: "m", Format: domain.FormatOpenAIResponses}
			}
			res, err := svc.ProbeAccount(context.Background(), tc.acc.ID, req)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidInput)
			require.Contains(t, err.Error(), tc.wantErr)
			require.Nil(t, res)
			require.Equal(t, 0, prober.calls, "凭据错误路径不得发网络请求")
		})
	}
}

// TestProbeEgressFailClosed A12：绑定存在但不可用 ⇒ 400 且不调用 Prober
// （绝不回落直连）；绑定可用 ⇒ spec 携带该出口规格。
func TestProbeEgressFailClosed(t *testing.T) {
	proxyID := int64(7)
	cases := []struct {
		name    string
		proxy   *domain.Proxy
		wantErr bool
	}{
		{name: "missing-row", proxy: nil, wantErr: true},
		{name: "disabled", proxy: &domain.Proxy{ID: proxyID, Scheme: domain.ProxySchemeHTTP, Host: "127.0.0.1", Port: 3128, Enabled: false}, wantErr: true},
		{name: "enabled", proxy: &domain.Proxy{ID: proxyID, Scheme: domain.ProxySchemeHTTP, Host: "127.0.0.1", Port: 3128, Enabled: true}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpl := staticProbeTemplate()
			acc := probeAccountFor(tpl)
			acc.ProxyID = &proxyID
			acc.Proxy = tc.proxy
			prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
			svc := newProbeSvc(&probeReadOnlyStore{acc: acc}, prober, nil)
			res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidInput)
				require.Contains(t, err.Error(), "账号出口绑定不可用")
				require.Equal(t, 0, prober.calls, "出口不可用不得发任何上游请求")
				require.Nil(t, res)
				return
			}
			require.NoError(t, err)
			require.True(t, res.Success)
			require.Equal(t, "http", prober.spec.Egress.Scheme)
			require.Equal(t, "127.0.0.1", prober.spec.Egress.Host)
			require.Equal(t, 3128, prober.spec.Egress.Port)
		})
	}
}

// TestProbeCustomExtMissing A4：custom 的 ext 行缺失 = 未配置（回落默认路径）；
// 其他读取错误才失败。探测执行使用 custom 路径覆盖由 probe 包端到端用例覆盖。
func TestProbeCustomExtMissing(t *testing.T) {
	tpl := staticProbeTemplate()
	tpl.CredentialType = credential.TypeCustom
	acc := probeAccountFor(tpl)

	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(&probeReadOnlyStore{acc: acc}, prober, nil)
	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Nil(t, prober.spec.UpstreamPaths, "ext 行缺失必须视为空覆盖")

	// 非 NotFound 的读错误 → 500（原样上抛，由 httpface 默认分支映射）。
	storeErr := errors.New("template ext read failed")
	svc = newProbeSvc(&probeReadOnlyStore{acc: acc, extErr: storeErr}, prober, nil)
	res, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.ErrorIs(t, err, storeErr)
	require.Nil(t, res)
	require.Equal(t, 1, prober.calls, "第二次失败请求不得调用 Prober")
}

// TestProbeCustomExtWiring A21：custom ext 存在时 Prober 收到 override；ext 行
// 缺失（ErrNotFound）→ UpstreamPaths 为空且不报错。
func TestProbeCustomExtWiring(t *testing.T) {
	tpl := staticProbeTemplate()
	tpl.CredentialType = credential.TypeCustom
	acc := probeAccountFor(tpl)
	override := map[string]string{string(domain.UpstreamPathChat): "v1/custom/chat?api-version=2024-10-21"}
	store := &probeReadOnlyStore{
		acc: acc,
		ext: &domain.TemplateExt{TemplateID: tpl.ID, UpstreamPaths: override},
	}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 1, prober.calls)
	require.Equal(t, override, prober.spec.UpstreamPaths, "custom ext 存在时必须把 override 交给 Prober")

	// ext 行缺失 → 空覆盖（nil/empty）且无错误。
	store.ext = nil
	res, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 2, prober.calls)
	require.Empty(t, prober.spec.UpstreamPaths, "ext 行缺失必须视为空覆盖")
}

// TestProbeNoUpstreamTextLeakLog A19：service 日志同样只含固定分类文案；上游
// 自由文本、其他租户 key 与内部 request id 不得进入日志。
func TestProbeNoUpstreamTextLeakLog(t *testing.T) {
	const (
		otherTenantKey = "sk-other-tenant"
		internalReqID  = "req_abc123"
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"` +
			otherTenantKey + ` failed for ` + internalReqID + `: free-form upstream text"}}`))
	}))
	defer up.Close()

	tpl := staticProbeTemplate()
	tpl.BaseURL = up.URL
	acc := probeAccountFor(tpl)
	hc := &http.Client{}
	realProber := probe.New(aiclient.NewFactory(hc, aiclient.Config{}), egress.NewPool(hc), nil, 5*time.Second, domain.ProbeMaxConcurrency)
	logPath := filepath.Join(t.TempDir(), "probe.log")
	logger, err := logx.New("info", logPath)
	require.NoError(t, err)
	svc := New(Deps{
		Store:          &probeReadOnlyStore{acc: acc},
		Invalidate:     NopInvalidator{},
		EmailCodeStore: testEmailCodes,
		Prober:         realProber,
		Log:            logger,
	})

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, "upstream rate limited", res.Error)
	require.Equal(t, "code=rate_limit_exceeded", res.Detail)

	require.NoError(t, logger.Sync())
	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logged := string(raw)
	require.Contains(t, logged, "account_probe")
	require.Contains(t, logged, "upstream rate limited")
	require.NotContains(t, logged, otherTenantKey)
	require.NotContains(t, logged, internalReqID)
	require.NotContains(t, logged, "free-form upstream text")
}

// TestProbeProberNotConfigured：nil Prober → 装配错误（handler 默认 500）。
func TestProbeProberNotConfigured(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	svc := newProbeSvc(&probeReadOnlyStore{acc: acc}, nil, nil)
	_, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidInput)
	require.NotErrorIs(t, err, ErrNotFound)
	require.NotErrorIs(t, err, ErrUnavailable)
}

// TestProbeRejectsDeletedAndOversizedAccount：软删账号不得继续用存储凭据探测；
// 通配模型输入在任何排队/上游组装前限制长度。
func TestProbeRejectsDeletedAndOversizedAccount(t *testing.T) {
	tpl := staticProbeTemplate()
	deletedAt := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	acc := &domain.Account{
		ID: 1, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-test",
		DeletedAt: &deletedAt,
	}
	store := &probeReadOnlyStore{acc: acc}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	_, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{
		Model: "m1", Format: domain.FormatOpenAIChat,
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, prober.calls, "软删账号不得进入探测执行器")

	acc.DeletedAt = nil
	oversized := strings.Repeat("m", domain.ProbeMaxModelLen+1)
	_, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{
		Model: oversized, Format: domain.FormatOpenAIChat,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Zero(t, prober.calls, "超长模型不得进入探测执行器")
}
