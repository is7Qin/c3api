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
	"sync/atomic"
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
	// accBlock 模拟账号读取阻塞；ctx 到期后返回成功，用于证明 store 成功后
	// 已过期的 pctx 不得继续模板/OAuth/prober。
	accBlock       bool
	accEnter       chan struct{}
	accRelease     chan struct{}
	accCalls       atomic.Int64
	accHasDeadline bool
	accDeadline    time.Time
	// credBlock 模拟 codex 凭据 SQL 读取：记录收到的 ctx 期限并返回 ctx 超时
	// （验证 service 传的是含总期限的 pctx，而不是无期限的父 ctx）。
	credBlock       bool
	credDeadline    time.Time
	credHasDeadline bool
}

func (s *probeReadOnlyStore) GetAllSettings(context.Context) ([]*domain.Setting, error) {
	return nil, nil
}

func (s *probeReadOnlyStore) GetAccountWithTemplate(ctx context.Context, id int64) (*domain.Account, error) {
	s.accCalls.Add(1)
	if deadline, ok := ctx.Deadline(); ok {
		s.accDeadline, s.accHasDeadline = deadline, true
	}
	if s.accBlock {
		<-ctx.Done()
	}
	if s.accEnter != nil {
		s.accEnter <- struct{}{}
		<-s.accRelease
	}
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

func (s *probeReadOnlyStore) GetOwnedAccountCredential(ctx context.Context, _ int64) (*domain.AccountCredential, error) {
	if s.credBlock {
		s.credDeadline, s.credHasDeadline = ctx.Deadline()
		return nil, context.DeadlineExceeded
	}
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
	calls            int
	spec             domain.ProbeSpec
	result           domain.ProbeResult
	probeDeadline    time.Time
	probeHasDeadline bool
}

func (f *fakeAccountProber) Probe(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	f.calls++
	f.spec = spec
	if deadline, ok := ctx.Deadline(); ok {
		f.probeDeadline, f.probeHasDeadline = deadline, true
	}
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
	realProber := probe.New(aiclient.NewFactory(hc, aiclient.Config{}), egress.NewPool(hc))
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

	// 用通配模板证明拒绝来自长度守卫，而不是模型恰好不在候选内。
	wildcard := *tpl
	wildcard.Models = nil
	wildcard.FormatModels = nil
	wildcard.ModelMapping = nil
	acc.Template = &wildcard
	acc.DeletedAt = nil
	oversized := strings.Repeat("m", domain.ProbeMaxModelLen+1)
	_, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{
		Model: oversized, Format: domain.FormatOpenAIChat,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Zero(t, prober.calls, "超长模型不得进入探测执行器")
}

// TestProbeInputErrorsBoundedBeforeStore：非法 format / 超长 model 在仓储等待前
// 就被拒绝，错误文本不回显任意长度的输入（避免 1 MiB 回显与 DB 池排队放大）。
func TestProbeInputErrorsBoundedBeforeStore(t *testing.T) {
	store := &probeReadOnlyStore{}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	longFormat := strings.Repeat("f", 1<<20)
	_, err := svc.ProbeAccount(context.Background(), 1, AccountProbeRequest{
		Model: "m1", Format: domain.RequestFormat(longFormat),
	})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.NotContains(t, err.Error(), longFormat)
	require.Less(t, len(err.Error()), 128)

	longModel := strings.Repeat("m", 1<<20)
	_, err = svc.ProbeAccount(context.Background(), 1, AccountProbeRequest{
		Model: longModel, Format: domain.FormatOpenAIChat,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.NotContains(t, err.Error(), longModel)
	require.Less(t, len(err.Error()), 128)
	require.Zero(t, prober.calls)
}

// TestProbeAdmissionBeforeStore：入口闸门在仓储读取前生效；队列满立即 busy，
// 已取消/已过期请求优先返回对应传输文案，且不触达 store/prober。
func TestProbeAdmissionBeforeStore(t *testing.T) {
	store := &probeReadOnlyStore{} // 一旦被读就会 panic（acc/Store 均为 nil）
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)
	for range cap(svc.probeGate.slots) {
		svc.probeGate.slots <- struct{}{}
	}
	for range cap(svc.probeGate.waiters) {
		svc.probeGate.waiters <- struct{}{}
	}
	req := AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat}

	res, err := svc.ProbeAccount(context.Background(), 1, req)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, "probe busy", res.Error)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = svc.ProbeAccount(canceled, 1, req)
	require.NoError(t, err)
	require.Equal(t, "probe canceled", res.Error)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	res, err = svc.ProbeAccount(expired, 1, req)
	require.NoError(t, err)
	require.Equal(t, "probe timed out", res.Error)
	require.Zero(t, prober.calls)
}

// TestProbeStoreContextErrorsBecomeResults：仓储返回 ctx 取消/超时不能被映射成
// 500，而应是 status 0 的探测结论；codex 凭据 SQL 必须收到含 30s 总期限的 pctx。
func TestProbeStoreContextErrorsBecomeResults(t *testing.T) {
	// 读账号时 ctx 过期。
	store := &probeReadOnlyStore{accErr: context.DeadlineExceeded}
	prober := &fakeAccountProber{}
	svc := newProbeSvc(store, prober, nil)
	res, err := svc.ProbeAccount(context.Background(), 1, AccountProbeRequest{
		Model: "m1", Format: domain.FormatOpenAIChat,
	})
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Zero(t, res.StatusCode)
	require.Equal(t, "probe timed out", res.Error)

	// codex 凭据 SQL 必须收到 service 的 30s 总期限（父 ctx 本身没有 deadline）。
	tpl := staticProbeTemplate()
	tpl.CredentialType = credential.TypeCodexPAT
	tpl.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIResponses}
	acc := &domain.Account{ID: 2, TemplateID: tpl.ID, Template: tpl}
	blocking := &probeReadOnlyStore{acc: acc, credBlock: true}
	svc = newProbeSvc(blocking, prober, nil)
	res, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{
		Model: "m1", Format: domain.FormatOpenAIResponses,
	})
	require.NoError(t, err)
	require.Equal(t, "probe timed out", res.Error)
	require.Zero(t, res.StatusCode)
	require.True(t, blocking.credHasDeadline, "凭据读取必须收到 pctx 期限")
	require.WithinDuration(t, time.Now().Add(domain.ProbeTimeout), blocking.credDeadline, 2*time.Second)
}

func newProbeLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.log")
	logger, err := logx.New("info", path)
	require.NoError(t, err)
	return logger, path
}

func probeLogCount(t *testing.T, logger *logx.Logger, path string) int {
	t.Helper()
	require.NoError(t, logger.Sync())
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Count(string(raw), "account_probe")
}

// TestProbeDeadlineOwnedByService：唯一期限由 service 创建并原样传给账号读取
// 与 prober；prober 不再自行包一层。
func TestProbeDeadlineOwnedByService(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{acc: acc}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.True(t, store.accHasDeadline, "账号读取必须收到 pctx 期限")
	require.True(t, prober.probeHasDeadline, "prober 必须收到同一 pctx")
	require.WithinDuration(t, time.Now().Add(domain.ProbeTimeout), store.accDeadline, 2*time.Second)
	require.Equal(t, store.accDeadline, prober.probeDeadline, "store 与 prober 必须共享同一期限")
}

// TestProbeContextAfterStoreSuccess：store 成功返回时 ctx 已过期，不得继续模板/
// OAuth 早退/prober；返回 status 0 + probe timed out。
func TestProbeContextAfterStoreSuccess(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{acc: acc, accBlock: true}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := svc.ProbeAccount(ctx, acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Zero(t, res.StatusCode)
	require.Equal(t, domain.ProbeErrorTimedOut, res.Error)
	require.Zero(t, prober.calls, "过期后不得调用 prober")
}

// TestProbeLatencyExcludesStoreWait：LatencyMS 是 prober 对上游调用的结论，
// service 不得用包含 DB/排队的总耗时覆盖。
func TestProbeLatencyExcludesStoreWait(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	prober := &fakeAccountProber{result: domain.ProbeResult{
		Success: true, StatusCode: 200, LatencyMS: 7,
	}}
	svc := newProbeSvc(&probeReadOnlyStore{acc: acc}, prober, nil)

	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.Equal(t, int64(7), res.LatencyMS)
}

// TestProbeLogOneForProberResult：prober 结果恰好一条 account_probe。
func TestProbeLogOneForProberResult(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	logger, path := newProbeLogger(t)
	svc := New(Deps{
		Store: &probeReadOnlyStore{acc: acc}, Invalidate: NopInvalidator{},
		EmailCodeStore: testEmailCodes, Prober: &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}},
		Log: logger,
	})
	_, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.Equal(t, 1, probeLogCount(t, logger, path))
}

// TestProbeLogOneForAdmission：busy/canceled/timedout 各一条。
func TestProbeLogOneForAdmission(t *testing.T) {
	logger, path := newProbeLogger(t)
	svc := New(Deps{
		Store: &probeReadOnlyStore{}, Invalidate: NopInvalidator{},
		EmailCodeStore: testEmailCodes, Prober: &fakeAccountProber{}, Log: logger,
	})
	for range cap(svc.probeGate.slots) {
		svc.probeGate.slots <- struct{}{}
	}
	for range cap(svc.probeGate.waiters) {
		svc.probeGate.waiters <- struct{}{}
	}
	req := AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat}

	_, err := svc.ProbeAccount(context.Background(), 1, req)
	require.NoError(t, err)
	require.Equal(t, 1, probeLogCount(t, logger, path))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.ProbeAccount(canceled, 1, req)
	require.NoError(t, err)
	require.Equal(t, 2, probeLogCount(t, logger, path))

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	_, err = svc.ProbeAccount(expired, 1, req)
	require.NoError(t, err)
	require.Equal(t, 3, probeLogCount(t, logger, path))
}

// TestProbeLogOneForOAuthExpired：codex token 过期早退也恰好一条，且不发网络。
func TestProbeLogOneForOAuthExpired(t *testing.T) {
	tpl := staticProbeTemplate()
	tpl.CredentialType = credential.TypeCodexOAuth
	tpl.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIResponses}
	past := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	acc := &domain.Account{ID: 12, TemplateID: tpl.ID, Template: tpl}
	store := &probeReadOnlyStore{acc: acc, cred: &domain.AccountCredential{
		AccountID: acc.ID, OAuthToken: "expired", OAuthExpiresAt: &past,
	}}
	prober := &fakeAccountProber{}
	logger, path := newProbeLogger(t)
	svc := New(Deps{
		Store: store, Invalidate: NopInvalidator{}, EmailCodeStore: testEmailCodes,
		Prober: prober, Log: logger,
	})
	res, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.Equal(t, domain.ProbeErrorCodexTokenExpired, res.Error)
	require.Zero(t, prober.calls)
	require.Equal(t, 1, probeLogCount(t, logger, path))
}

// TestProbeLogNoneOnValidation404500：未形成 200 探测结论的退出不写 account_probe。
func TestProbeLogNoneOnValidation404500(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{acc: acc}
	logger, path := newProbeLogger(t)
	svc := New(Deps{
		Store: store, Invalidate: NopInvalidator{}, EmailCodeStore: testEmailCodes,
		Prober: &fakeAccountProber{}, Log: logger,
	})

	_, err := svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: "bogus"})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = svc.ProbeAccount(context.Background(), 999, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.ErrorIs(t, err, ErrNotFound)

	store.accErr = errors.New("db failed")
	_, err = svc.ProbeAccount(context.Background(), acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.Error(t, err)
	require.Zero(t, probeLogCount(t, logger, path))
}

// TestProbeStoreErrorAfterDeadline：store 返回非 ctx 错误（404/500）时，只要
// pctx 已过期，结论必须是 status 0 的 probe timed out，而不是错误映射。
func TestProbeStoreErrorAfterDeadline(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{acc: acc, accErr: repository.ErrNotFound, accBlock: true}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := svc.ProbeAccount(ctx, acc.ID, AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err, "不得把过期映射成 404/500")
	require.False(t, res.Success)
	require.Zero(t, res.StatusCode)
	require.Equal(t, domain.ProbeErrorTimedOut, res.Error)
	require.Zero(t, prober.calls)
}

// TestProbeAdmissionBarrierBeforeStore：4 个执行中的请求可以进入 store；第 5 个
// 在等待位里，绝不能进入 store，直到一个执行槽被释放后晋升。
func TestProbeAdmissionBarrierBeforeStore(t *testing.T) {
	tpl := staticProbeTemplate()
	acc := probeAccountFor(tpl)
	store := &probeReadOnlyStore{
		acc:        acc,
		accEnter:   make(chan struct{}, domain.ProbeMaxConcurrency+1),
		accRelease: make(chan struct{}, domain.ProbeMaxConcurrency+1),
	}
	prober := &fakeAccountProber{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := newProbeSvc(store, prober, nil)
	req := AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat}

	results := make(chan error, domain.ProbeMaxConcurrency+1)
	for range domain.ProbeMaxConcurrency {
		go func() {
			_, err := svc.ProbeAccount(context.Background(), acc.ID, req)
			results <- err
		}()
	}
	require.Eventually(t, func() bool {
		return store.accCalls.Load() == domain.ProbeMaxConcurrency
	}, time.Second, time.Millisecond, "4 个执行请求应已进入 store")

	go func() {
		_, err := svc.ProbeAccount(context.Background(), acc.ID, req)
		results <- err
	}()
	require.Eventually(t, func() bool {
		return len(svc.probeGate.waiters) == 1
	}, time.Second, time.Millisecond, "第 5 个应进入等待位")
	require.Equal(t, int64(domain.ProbeMaxConcurrency), store.accCalls.Load(),
		"4 个执行槽占用时第 5 个绝不能进入 store")

	store.accRelease <- struct{}{} // 释放一个执行槽，第 5 个晋升
	require.Eventually(t, func() bool {
		return store.accCalls.Load() == domain.ProbeMaxConcurrency+1
	}, time.Second, time.Millisecond, "晋升后第 5 个才进入 store")

	for range domain.ProbeMaxConcurrency {
		store.accRelease <- struct{}{}
	}
	for range domain.ProbeMaxConcurrency + 1 {
		require.NoError(t, <-results)
	}
}
