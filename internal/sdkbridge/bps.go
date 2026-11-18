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
	"time"

	bpssdk "github.com/is7Qin/bps-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// ErrBPSProfileRequired is returned before any client/auth construction when a
// BPS account has no complete persisted DeviceProfile.
var ErrBPSProfileRequired = errors.New("sdkbridge: BPS device profile required")

// ErrBPSIdentityMismatch means that an SDK callback attempted to use a
// different upstream account than the one frozen by Prepare.
var ErrBPSIdentityMismatch = errors.New("sdkbridge: BPS upstream account identity mismatch")

// ErrBPSStoreUnavailable means the adapter was constructed without the
// persistence seam required by refresh-token rotation.
var ErrBPSStoreUnavailable = errors.New("sdkbridge: BPS token store unavailable")

// ErrBPSEgressRequired prevents a BPS credential from silently using the
// process default HTTP client or an unbound proxy path.
var ErrBPSEgressRequired = errors.New("sdkbridge: BPS egress required")

// ErrBPSOwnerMismatch means a token-store implementation did not receive the
// account/K owner captured by Prepare. It is deliberately separate from an
// upstream account-id mismatch: c3 account identity and upstream account id
// must never be silently conflated.
var ErrBPSOwnerMismatch = errors.New("sdkbridge: BPS token owner mismatch")

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

// BPSTokenSet is the sdkbridge-owned token persistence shape. It intentionally
// does not expose bps-sdk.TokenSet to repository/service callers.
type BPSTokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// BPSTokenKey is the c3-owned owner/K fence passed to persistence. UpstreamID
// is checked against the string key received from bps-sdk, but is never
// replaced with the c3 account id or a concatenation containing K.
type BPSTokenKey struct {
	AccountID         int64
	IdentityRevision  int64
	Owner             string
	UpstreamAccountID string
}

// BPSTokenStore is the c3 persistence seam for BPS refresh-token rotation.
// Implementations must enforce AccountID, IdentityRevision and Owner as a
// single ownership tuple and CAS expectedRevision in SaveCAS.
type BPSTokenStore interface {
	Load(ctx context.Context, key BPSTokenKey) (BPSTokenSet, int64, error)
	SaveCAS(ctx context.Context, key BPSTokenKey, expectedRevision int64, set BPSTokenSet) (int64, error)
}

type bpsTokenStoreAdapter struct {
	store BPSTokenStore
	key   BPSTokenKey
}

func (s bpsTokenStoreAdapter) Load(ctx context.Context, upstreamAccountID string) (bpssdk.TokenSet, int64, error) {
	if upstreamAccountID != s.key.UpstreamAccountID {
		return bpssdk.TokenSet{}, 0, fmt.Errorf("%w: got %q want %q", ErrBPSIdentityMismatch, upstreamAccountID, s.key.UpstreamAccountID)
	}
	set, revision, err := s.store.Load(ctx, s.key)
	if err != nil {
		return bpssdk.TokenSet{}, 0, err
	}
	return bpssdk.TokenSet{AccessToken: set.AccessToken, RefreshToken: set.RefreshToken, ExpiresAt: set.ExpiresAt}, revision, nil
}

func (s bpsTokenStoreAdapter) SaveCAS(ctx context.Context, upstreamAccountID string, expectedRevision int64, set bpssdk.TokenSet) (int64, error) {
	if upstreamAccountID != s.key.UpstreamAccountID {
		return 0, fmt.Errorf("%w: got %q want %q", ErrBPSIdentityMismatch, upstreamAccountID, s.key.UpstreamAccountID)
	}
	if s.key.AccountID <= 0 || s.key.IdentityRevision <= 0 || s.key.Owner == "" {
		return 0, ErrBPSOwnerMismatch
	}
	next, err := s.store.SaveCAS(ctx, s.key, expectedRevision, BPSTokenSet{
		AccessToken: set.AccessToken, RefreshToken: set.RefreshToken, ExpiresAt: set.ExpiresAt,
	})
	return next, err
}

// BPS is the independent BPS adapter. It has no Codex client table, identity
// pool, conversation book, or Codex transport dependency.
type BPS struct {
	store BPSTokenStore
}

func NewBPS(store BPSTokenStore) *BPS {
	return &BPS{store: store}
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
	egress           *egress.Client
	client           *bpssdk.Client
}

func (p *BPSPrepared) AccountID() int64             { return p.accountID }
func (p *BPSPrepared) IdentityRevision() int64      { return p.identityRevision }
func (p *BPSPrepared) Format() domain.RequestFormat { return p.format }
func (p *BPSPrepared) Transport() BPSTransport      { return p.transport }
func (p *BPSPrepared) UpstreamAccountID() string    { return p.upstreamAccount }
func (p *BPSPrepared) Egress() *egress.Client       { return p.egress }
func (p *BPSPrepared) ProfileJSON() []byte          { return append([]byte(nil), p.profile...) }

// Prepare validates BPS-only credential/profile invariants, derives transport
// from format, and creates a client using the fixed SDK endpoint. The c3
// account/K owner is passed only to BPSTokenStore; NewTokenAuth receives the
// verified upstream CodexAccountID exactly as required by the SDK.
func (a *BPS) Prepare(ctx context.Context, source BPSRequestSource) (*BPSPrepared, error) {
	if a == nil || a.store == nil {
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
	owner := fmt.Sprintf("c3-account:%d", cred.AccountID)
	key := BPSTokenKey{
		AccountID: cred.AccountID, IdentityRevision: cred.IdentityRevision,
		Owner: owner, UpstreamAccountID: cred.CodexAccountID,
	}
	auth, err := bpssdk.NewTokenAuth(
		cred.OAuthToken, cred.OAuthRefreshToken, cred.CodexAccountID, "",
		bpsTokenStoreAdapter{store: a.store, key: key},
		bpssdk.WithAuthHTTPTransport(rt),
	)
	if err != nil {
		return nil, err
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
	sdkClient, err := bpssdk.NewClient(auth, options...)
	if err != nil {
		return nil, err
	}
	return &BPSPrepared{
		accountID: cred.AccountID, identityRevision: cred.IdentityRevision,
		format: source.Format, transport: transport, upstreamAccount: cred.CodexAccountID,
		profile: profile, egress: source.Egress, client: sdkClient,
	}, nil
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
	if p.transport != BPSTransportHTTP {
		return BPSResult{}, fmt.Errorf("sdkbridge: BPS Responses requires HTTP format")
	}
	response, err := p.client.Responses(ctx, payload)
	if err != nil {
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

func (p *BPSPrepared) StreamBody(ctx context.Context, payload []byte) (*http.Response, error) {
	if p == nil || p.client == nil {
		return nil, ErrBPSStoreUnavailable
	}
	if p.transport != BPSTransportHTTP {
		return nil, fmt.Errorf("sdkbridge: BPS stream body requires HTTP format")
	}
	return p.client.StreamBody(ctx, payload)
}

// BPSStream is the provider-neutral WS frame reader. The bps-sdk Session and
// MessageType stay entirely inside sdkbridge.
type BPSStream struct {
	session *bpssdk.Session
}

func (s *BPSStream) Read(ctx context.Context) ([]byte, error) {
	if s == nil || s.session == nil {
		return nil, io.ErrClosedPipe
	}
	_, data, err := s.session.Read(ctx)
	return append([]byte(nil), data...), err
}

func (s *BPSStream) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close(1000, "")
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
		return nil, err
	}
	return &BPSStream{session: session}, nil
}

func (p *BPSPrepared) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.Close()
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
	Kind       BPSFailureKind
	Status     int
	RetryAfter time.Duration
	Dispatched bool
	Err        error
}

type bpsStatusError interface {
	StatusCode() int
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
	if bpssdk.IsInvalidRequestBody(err) || errors.Is(err, bpssdk.ErrRequestTooLarge) || errors.Is(err, bpssdk.ErrFrameTooLarge) {
		failure.Kind = BPSFailureLocalReject
		return failure
	}
	return failure
}
