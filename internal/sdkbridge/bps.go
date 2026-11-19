// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	bpssdk "github.com/is7Qin/bps-sdk"
	bpsredis "github.com/is7Qin/bps-sdk/redisstore"
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// ErrBPSProfileRequired is returned before any client/auth construction when a
// BPS account has no complete persisted DeviceProfile.
var ErrBPSProfileRequired = errors.New("sdkbridge: BPS device profile required")

// ErrBPSIdentityMismatch means the selected credential has an incomplete identity or OAuth pair.
var ErrBPSIdentityMismatch = errors.New("sdkbridge: BPS upstream account identity mismatch")

// ErrBPSStoreUnavailable means the adapter was constructed without the
// persistence seam required by refresh-token rotation.
var ErrBPSStoreUnavailable = errors.New("sdkbridge: BPS token store unavailable")

// ErrBPSEgressRequired prevents a BPS credential from silently using the
// process default HTTP client or an unbound proxy path.
var ErrBPSEgressRequired = errors.New("sdkbridge: BPS egress required")

// BPS transport is derived from the request format. There is no provider-owned
// transport field in domain.Template, AccountCredential, or Selection.
type BPSTransport uint8

const (
	BPSTransportHTTP BPSTransport = iota
	BPSTransportWS
)

func (t BPSTransport) String() string {
	if t == BPSTransportWS {
		return "ws"
	}
	return "http"
}

// BPSFormatTransport is the single format-to-transport authority used by the
// adapter and its tests.
func BPSFormatTransport(format domain.RequestFormat) (BPSTransport, error) {
	switch format {
	case domain.FormatOpenAIResponses:
		return BPSTransportHTTP, nil
	case domain.FormatOpenAIResponsesWS:
		return BPSTransportWS, nil
	default:
		return 0, fmt.Errorf("%w: format %q", credential.ErrUnsupported, format)
	}
}

// BPS is the independent BPS adapter. Redis persistence is implemented by
// bps-sdk/redisstore; c3api supplies its existing Redis connection and only
// freezes the c3 owner/K scope when constructing the SDK TokenAuth.
type bpsClientEntry struct {
	key       string
	client    *bpssdk.Client
	accountID int64
	transport BPSTransport
}

type BPS struct {
	redisStore *bpsredis.Store
	storeErr   error
	failure    FailureHandler
	mu         sync.Mutex
	clients    map[string]*bpsClientEntry
	fatalMu    sync.Mutex
	fatal      map[string]bool
}

// NewBPS receives the application's Redis connection; the SDK-owned redisstore
// retains token ownership. The optional failure callback is the c3api K-fenced
// account-failure seam shared with the Codex adapter.
func NewBPS(rdb redis.Scripter, failure ...FailureHandler) *BPS {
	store, err := bpsredis.New(rdb)
	var fh FailureHandler
	if len(failure) > 0 {
		fh = failure[0]
	}
	return &BPS{redisStore: store, storeErr: err, failure: fh, clients: make(map[string]*bpsClientEntry), fatal: make(map[string]bool)}
}

// BPSRequestSource is the one frozen input bundle for a BPS attempt. Keeping
// credential, format, and egress together prevents callers from mixing a
// current AccountExt with a stale selection or a different egress. The
// optional fingerprint is an adapter-owned escape hatch for future profile
// generation; a validated persisted profile remains authoritative today.
type BPSRequestSource struct {
	Credential domain.AccountCredential
	Format     domain.RequestFormat
	Egress     *egress.Client
	// Transport is the template-level Responses egress (""/"http"/"ws"), the same
	// knob Codex uses. It only matters for the HTTP client lane: the WS client
	// lane already speaks WebSocket to the upstream. "ws" here means "dial BPS
	// over WebSocket but keep streaming SSE to an HTTP client".
	Transport domain.CodexResponsesTransport
}

// BPSPrepared freezes all account-scoped facts for one request attempt. The
// underlying SDK client/auth is private; callers cannot supply an AccountExt
// later and accidentally mix a different profile or K.
type BPSPrepared struct {
	accountID        int64
	identityRevision int64
	format           domain.RequestFormat
	transport        BPSTransport
	upstreamAccount  string
	profile          json.RawMessage
	profileHash      string
	egress           *egress.Client
	client           *bpssdk.Client
	fatalOnce        sync.Once
	reportFatal      func(error)
}

func (p *BPSPrepared) AccountID() int64             { return p.accountID }
func (p *BPSPrepared) IdentityRevision() int64      { return p.identityRevision }
func (p *BPSPrepared) Format() domain.RequestFormat { return p.format }
func (p *BPSPrepared) Transport() BPSTransport      { return p.transport }
func (p *BPSPrepared) UpstreamAccountID() string    { return p.upstreamAccount }
func (p *BPSPrepared) Egress() *egress.Client       { return p.egress }
func (p *BPSPrepared) ProfileJSON() []byte          { return append([]byte(nil), p.profile...) }

// Prepare validates BPS-only credential/profile invariants, derives the upstream
// transport (format for the WS client lane, template override for the HTTP lane),
// and creates a client using the fixed SDK endpoint. The c3
// account/K owner is passed only to the SDK-owned redisstore TokenStore; NewTokenAuth receives the
// verified upstream CodexAccountID exactly as required by the SDK.
func (a *BPS) Prepare(ctx context.Context, source BPSRequestSource) (*BPSPrepared, error) {
	if a == nil || a.redisStore == nil || a.storeErr != nil {
		if a != nil && a.storeErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrBPSStoreUnavailable, a.storeErr)
		}
		return nil, ErrBPSStoreUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cred := source.Credential
	if cred.CredentialType != credential.TypeBPS {
		return nil, fmt.Errorf("%w: credential type %q", credential.ErrUnsupported, cred.CredentialType)
	}
	if source.Egress == nil || source.Egress.HTTP() == nil {
		return nil, ErrBPSEgressRequired
	}
	transport, err := BPSFormatTransport(source.Format)
	if err != nil {
		return nil, err
	}
	// 模板级出站传输（与 Codex 同一个开关）：HTTP 客户端 + ws 上游 ⇒ SDK 走
	// WebSocket，仍以标准 SSE 回客户端；WS 客户端车道本来就必须 WS，忽略之。
	if source.Transport == domain.CodexResponsesTransportWS {
		transport = BPSTransportWS
	}
	if cred.AccountID <= 0 || cred.IdentityRevision <= 0 || cred.CodexAccountID == "" || cred.OAuthToken == "" || cred.OAuthRefreshToken == "" {
		return nil, ErrBPSIdentityMismatch
	}
	if err := domain.ValidateBPSProfileJSON(cred.BPSProfile); err != nil {
		return nil, ErrBPSProfileRequired
	}
	profile := append(json.RawMessage(nil), cred.BPSProfile...)
	client := source.Egress.HTTP()
	rt := client.Transport
	if rt == nil {
		return nil, ErrBPSEgressRequired
	}
	profileHash := domain.BPSProfileHash(profile)
	cacheKey := fmt.Sprintf("%d:%d:%s:%s:%x:%s", cred.AccountID, cred.IdentityRevision, cred.CodexAccountID, profileHash, source.Egress.Spec.Key(), transport.String())
	a.mu.Lock()
	entry := a.clients[cacheKey]
	if entry == nil {
		store, storeErr := a.redisStore.ForOwner(bpsredis.Owner{
			ID: fmt.Sprint(cred.AccountID), Revision: cred.IdentityRevision,
		})
		if storeErr != nil {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: %w", ErrBPSStoreUnavailable, storeErr)
		}
		auth, authErr := bpssdk.NewTokenAuth(
			cred.OAuthToken, cred.OAuthRefreshToken, cred.CodexAccountID, "",
			store, bpssdk.WithAuthHTTPTransport(rt),
		)
		if authErr != nil {
			a.mu.Unlock()
			a.reportFatal(cacheKey, cred, authErr)
			return nil, authErr
		}
		options := []bpssdk.Option{
			bpssdk.WithEndpoint(domain.BPSDefaultEndpoint),
			bpssdk.WithDeviceProfileJSON(profile),
			bpssdk.WithHTTPTransport(rt),
			bpssdk.WithUnboundedConcurrency(),
			bpssdk.WithRequestBudget(bpssdk.Budget{MaxRequests: -1}),
			bpssdk.WithRawResponse(true),
		}
		if transport == BPSTransportWS {
			options = append(options, bpssdk.WithResponsesTransport(bpssdk.TransportWS))
		} else {
			options = append(options, bpssdk.WithResponsesTransport(bpssdk.TransportHTTP))
		}
		sdkClient, authErr := bpssdk.NewClient(auth, options...)
		if authErr != nil {
			a.mu.Unlock()
			a.reportFatal(cacheKey, cred, authErr)
			return nil, authErr
		}
		entry = &bpsClientEntry{key: cacheKey, client: sdkClient, accountID: cred.AccountID, transport: transport}
		for key, old := range a.clients {
			if old.accountID == cred.AccountID && old.transport == transport && key != cacheKey {
				delete(a.clients, key)
				_ = old.client.Close()
			}
		}
		a.clients[cacheKey] = entry
		a.fatalMu.Lock()
		delete(a.fatal, fmt.Sprintf("%d:%d", cred.AccountID, cred.IdentityRevision))
		a.fatalMu.Unlock()
	}
	a.mu.Unlock()
	return &BPSPrepared{
		accountID: cred.AccountID, identityRevision: cred.IdentityRevision,
		format: source.Format, transport: transport, upstreamAccount: cred.CodexAccountID,
		profile: append(json.RawMessage(nil), profile...), egress: source.Egress, client: entry.client,
		profileHash: profileHash, reportFatal: func(err error) { a.reportFatal(cacheKey, cred, err) },
	}, nil
}

func (a *BPS) reportFatal(cacheKey string, cred domain.AccountCredential, err error) {
	if a == nil || ClassifyBPSFailure(err).Kind != BPSFailureAccountFatal {
		return
	}
	a.mu.Lock()
	if entry := a.clients[cacheKey]; entry != nil {
		delete(a.clients, cacheKey)
		_ = entry.client.Close()
	}
	a.mu.Unlock()
	key := fmt.Sprintf("%d:%d", cred.AccountID, cred.IdentityRevision)
	a.fatalMu.Lock()
	if a.fatal[key] {
		a.fatalMu.Unlock()
		return
	}
	a.fatal[key] = true
	a.fatalMu.Unlock()
	if a.failure != nil {
		a.failure(CredentialFailure{AccountID: cred.AccountID, SourceRevision: cred.IdentityRevision, Cause: err})
	}
}

func (p *BPSPrepared) report(err error) {
	if p == nil || err == nil || p.reportFatal == nil {
		return
	}
	p.fatalOnce.Do(func() { p.reportFatal(err) })
}

// Probe performs one authenticated catalog/access request on the selected
// egress. It does not synthesize quota windows or alter account health itself.
func (a *BPS) Probe(ctx context.Context, cred domain.AccountCredential, eg *egress.Client) error {
	prepared, err := a.Prepare(ctx, BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	if err != nil {
		return err
	}
	defer prepared.Close()
	_, err = prepared.Models(ctx)
	return err
}

// BPSResult is the provider-neutral non-stream result. Raw is a cloned
// standard Responses JSON object and never contains credential/profile data.
type BPSResult struct {
	Raw       json.RawMessage
	RequestID string
	Format    domain.RequestFormat
}

func (p *BPSPrepared) Responses(ctx context.Context, payload []byte) (BPSResult, error) {
	if p == nil || p.client == nil {
		return BPSResult{}, ErrBPSStoreUnavailable
	}
	response, err := p.client.Responses(ctx, payload)
	if err != nil {
		p.report(err)
		return BPSResult{}, err
	}
	raw := append(json.RawMessage(nil), response.Raw...)
	if len(raw) == 0 {
		raw, err = json.Marshal(response)
		if err != nil {
			return BPSResult{}, err
		}
	}
	return BPSResult{Raw: raw, RequestID: response.RequestID, Format: p.format}, nil
}

func (p *BPSPrepared) Models(ctx context.Context) (*bpssdk.Catalog, error) {
	if p == nil || p.client == nil {
		return nil, ErrBPSStoreUnavailable
	}
	catalog, err := p.client.Models(ctx)
	if err != nil {
		p.report(err)
	}
	return catalog, err
}

func (p *BPSPrepared) StreamBody(ctx context.Context, payload []byte) (*http.Response, error) {
	if p == nil || p.client == nil {
		return nil, ErrBPSStoreUnavailable
	}
	resp, err := p.client.StreamBody(ctx, payload)
	if err != nil {
		p.report(err)
	}
	return resp, err
}

// BPSStream is the provider-neutral WS frame reader. The bps-sdk Session and
// MessageType stay entirely inside sdkbridge.
type BPSStream struct {
	session *bpssdk.Session
}

func (s *BPSStream) Write(ctx context.Context, typ websocket.MessageType, frame []byte) error {
	if s == nil || s.session == nil {
		return io.ErrClosedPipe
	}
	if typ != websocket.MessageText {
		return bpssdk.ErrInvalidFrame
	}
	return s.session.Write(ctx, bpssdk.MessageText, frame)
}

func (s *BPSStream) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if s == nil || s.session == nil {
		return websocket.MessageText, nil, io.ErrClosedPipe
	}
	typ, data, err := s.session.Read(ctx)
	if typ != bpssdk.MessageText {
		return websocket.MessageText, append([]byte(nil), data...), bpssdk.ErrInvalidFrame
	}
	return websocket.MessageText, append([]byte(nil), data...), err
}

func (s *BPSStream) Ping(ctx context.Context) error {
	if s == nil || s.session == nil {
		return io.ErrClosedPipe
	}
	return s.session.Ping(ctx)
}

func (s *BPSStream) Close(code websocket.StatusCode, reason string) error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close(int(code), reason)
}

func (s *BPSStream) CloseNow() {
	if s != nil && s.session != nil {
		_ = s.session.CloseNow()
	}
}

func (p *BPSPrepared) DialFrame(ctx context.Context, frame []byte) (*BPSStream, error) {
	if p == nil || p.client == nil {
		return nil, ErrBPSStoreUnavailable
	}
	if p.transport != BPSTransportWS {
		return nil, fmt.Errorf("sdkbridge: BPS DialFrame requires WS format")
	}
	session, err := p.client.DialFrame(ctx, frame)
	if err != nil {
		p.report(err)
		return nil, err
	}
	return &BPSStream{session: session}, nil
}

func (p *BPSPrepared) Close() error { return nil }

// Close releases all cached SDK clients after the request/worker drain.
func (a *BPS) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	entries := make([]*bpsClientEntry, 0, len(a.clients))
	for key, entry := range a.clients {
		delete(a.clients, key)
		entries = append(entries, entry)
	}
	a.mu.Unlock()
	var first error
	for _, entry := range entries {
		if err := entry.client.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// BPSFailureKind is the provider-neutral BPS classifier contract. Policy is a
// cooldown/failover signal and is intentionally not AccountFatal.
type BPSFailureKind uint8

const (
	BPSFailureUnknown BPSFailureKind = iota
	BPSFailureAccountFatal
	BPSFailurePolicy
	BPSFailureModelDenied
	BPSFailureQuota
	BPSFailureRateLimit
	BPSFailureLocalReject
	BPSFailureAmbiguous
	BPSFailureIdleTimeout
	BPSFailureIncomplete
	BPSFailureTransport
)

type BPSFailure struct {
	Kind   BPSFailureKind
	Status int
	// Code/RequestID 是上游错误体的**标识**（不是正文）：只用于运维日志定位，
	// 客户端文案仍由各调用面给固定句。
	Code       string
	RequestID  string
	RetryAfter time.Duration
	Dispatched bool
	Err        error
}

// String 给日志用；新增 kind 必须在这里补名字，否则日志会印数字。
func (k BPSFailureKind) String() string {
	switch k {
	case BPSFailureAccountFatal:
		return "account_fatal"
	case BPSFailurePolicy:
		return "policy"
	case BPSFailureModelDenied:
		return "model_denied"
	case BPSFailureQuota:
		return "quota"
	case BPSFailureRateLimit:
		return "rate_limit"
	case BPSFailureLocalReject:
		return "local_reject"
	case BPSFailureAmbiguous:
		return "ambiguous"
	case BPSFailureIdleTimeout:
		return "idle_timeout"
	case BPSFailureIncomplete:
		return "incomplete"
	case BPSFailureTransport:
		return "transport"
	default:
		return "unknown"
	}
}

type bpsStatusError interface {
	StatusCode() int
}

// IsBPSIdleTimeout identifies a BPS upstream read stall without coupling proxy
// callers to bps-sdk sentinel errors.
func IsBPSIdleTimeout(err error) bool {
	return errors.Is(err, bpssdk.ErrStreamIdleTimeout)
}

func IsBPSAccountFatal(err error) bool {
	return ClassifyBPSFailure(err).Kind == BPSFailureAccountFatal
}

func ClassifyBPSFailure(err error) BPSFailure {
	if err == nil {
		return BPSFailure{}
	}
	failure := BPSFailure{Kind: BPSFailureUnknown, Err: err}
	var status bpsStatusError
	if errors.As(err, &status) {
		failure.Status = status.StatusCode()
	}
	var api *bpssdk.APIError
	if errors.As(err, &api) {
		failure.Code = api.Code
		failure.RequestID = api.RequestID
	}
	var transportErr *bpssdk.TransportError
	if errors.As(err, &transportErr) {
		failure.Kind = BPSFailureTransport
		failure.Dispatched = transportErr.Dispatched
		if transportErr.Class == bpssdk.ClassAmbiguous {
			failure.Kind = BPSFailureAmbiguous
		}
		return failure
	}
	var ambiguous *bpssdk.AmbiguousCommitError
	if errors.As(err, &ambiguous) {
		failure.Kind = BPSFailureAmbiguous
		failure.Dispatched = true
		return failure
	}
	if bpssdk.IsAccountFatal(err) {
		failure.Kind = BPSFailureAccountFatal
		return failure
	}
	var policy *bpssdk.PolicyError
	if errors.As(err, &policy) {
		failure.Kind = BPSFailurePolicy
		failure.RetryAfter, _ = bpssdk.PolicyCooldown(err)
		return failure
	}
	var modelDenied *bpssdk.ModelDeniedError
	if errors.As(err, &modelDenied) {
		failure.Kind = BPSFailureModelDenied
		return failure
	}
	var modelChanged *bpssdk.ModelAccessChangedError
	if errors.As(err, &modelChanged) {
		failure.Kind = BPSFailureModelDenied
		failure.RetryAfter = modelChanged.RetryAfter
		return failure
	}
	var quota *bpssdk.QuotaError
	if errors.As(err, &quota) {
		failure.Kind = BPSFailureQuota
		return failure
	}
	var rate *bpssdk.RateLimitError
	if errors.As(err, &rate) {
		failure.Kind = BPSFailureRateLimit
		failure.RetryAfter = rate.RetryAfter
		return failure
	}
	if bpssdk.IsIdleTimeout(err) || errors.Is(err, bpssdk.ErrStreamIdleTimeout) {
		failure.Kind = BPSFailureIdleTimeout
		return failure
	}
	if errors.Is(err, bpssdk.ErrStreamIncomplete) {
		failure.Kind = BPSFailureIncomplete
		return failure
	}
	if bpssdk.IsInvalidRequestBody(err) || errors.Is(err, bpssdk.ErrInvalidFrame) || errors.Is(err, bpssdk.ErrRequestTooLarge) || errors.Is(err, bpssdk.ErrFrameTooLarge) {
		failure.Kind = BPSFailureLocalReject
		return failure
	}
	return failure
}

// BPSCodePreviousResponseUnsupported 是 continuation 本地拒绝的唯一文案：proxy 的
// 前置拒绝与 SDK 的归一错误共用同一句，避免两处各写一份后漂移。
const BPSCodePreviousResponseUnsupported = "BPS does not support previous_response_id; send the full input history"

// BPSLocalRejectMessage turns an SDK pre-dial rejection into an actionable,
// non-sensitive client message. The raw upstream error text never leaves the
// adapter; only these fixed sentences and the offending field name do.
func BPSLocalRejectMessage(err error) string {
	switch {
	case errors.Is(err, bpssdk.ErrPreviousResponseUnsupported):
		return BPSCodePreviousResponseUnsupported
	case errors.Is(err, bpssdk.ErrItemReferenceUnsupported):
		return "BPS does not support item_reference; inline the referenced items"
	case errors.Is(err, bpssdk.ErrBackgroundUnsupported):
		return "BPS does not support background responses"
	case errors.Is(err, bpssdk.ErrUnsupportedTextFormat):
		return "BPS does not support text.format; remove it (plain text output only)"
	case errors.Is(err, bpssdk.ErrToolCatalogInvalid):
		// 工具错误只会点名客户端自己提交的类型/名字（不含上游原文），带上能让
		// 调用方直接改请求，而不是对着笼统的 400 猜。
		return "BPS rejected the tool definitions: " + bpsErrorDetail(err)
	}
	var invalid *bpssdk.ValidationError
	if errors.As(err, &invalid) && invalid.Field != "" {
		return "invalid request body: " + invalid.Field
	}
	return "invalid request body"
}

// bpsErrorDetail 保留 SDK 校验错误里可执行的那部分（去掉库前缀），并做 UTF-8
// 安全截断。仅用于工具目录类错误：其内容来自客户端提交的 tools。
func bpsErrorDetail(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"bps: ", "bridge: ", "invalid tool catalog: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	const maxDetail = 200
	if len(msg) <= maxDetail {
		return msg
	}
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}
