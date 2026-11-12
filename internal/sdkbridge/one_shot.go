// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// oneShotMaxBodyBytes 是诊断/探测 one-shot seam 的文本/search 上游响应体上限
// （2 MiB）。images 用 oneShotImageMaxBodyBytes：其正常 b64_json 结果会超过 2 MiB。
const oneShotMaxBodyBytes = domain.ProbeMaxBodyBytes

// oneShotImageMaxBodyBytes 是 codex GenerateImage 的上游响应体上限（8 MiB）；
// 与 static images 共用 domain.ProbeMaxImageResponseBytes。
const oneShotImageMaxBodyBytes = domain.ProbeMaxImageResponseBytes

// ErrBodyTooLarge 是 one-shot seam 的 200 响应体溢出哨兵。文本/search 上限为
// 2 MiB，GenerateImage 上限为 8 MiB；probe 用 errors.Is 映射到对应固定文案，
// 非 200 永不返回本错误。
var ErrBodyTooLarge = errors.New("sdkbridge: response body exceeds limit")

// oneShotTransport 包裹调用方提供的（已按 origin/上限装配的）transport：记录
// 最后一次 HTTP 状态与响应体实际读出的字节数。GenerateImage 的 typed success
// 没有状态字段，必须靠这里取回真实 status（201/204/206 也不能冒充 200）。
type oneShotTransport struct {
	next   http.RoundTripper
	status atomic.Int32
	read   atomic.Int64
}

func (t *oneShotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if resp != nil {
		t.status.Store(int32(resp.StatusCode))
		if resp.Body != nil {
			resp.Body = &oneShotCountingBody{rc: resp.Body, n: &t.read}
		}
	}
	return resp, err
}

// oneShotCountingBody 只计数、不改变读取语义（EOF/错误原样透传）；Close 恰好
// 一次由 SDK 负责。
type oneShotCountingBody struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (b *oneShotCountingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.n.Add(int64(n))
	}
	return n, err
}

func (b *oneShotCountingBody) Close() error { return b.rc.Close() }

// GenerateImageOnce 用 codex SDK 的 GenerateImage 发一次生图请求（诊断/探测
// one-shot seam）：临时 PAT client + WithTransport，无 ClientTable/Auth 缓存/
// fatal 回调/轮转/日志。
//
// 返回契约：
//   - typed success（SDK 解析成功）→ **真实捕获的 HTTP 状态** + 由
//     MarshalImageResponse 生成的上游 wire 形态 body；200 但实际读出 >2 MiB
//     → ErrBodyTooLarge。
//   - SDK *HTTPError（非 2xx/4xx/5xx）→ 真实 status + raw（不判溢出）。
//   - AuthPermanentlyRevokedError → 401 + raw。
//   - 其他传输/SDK 错误（含 200 解析失败）→ 捕获 status（无则 0）+ err；
//     200 解析失败且读出 >8 MiB → ErrBodyTooLarge。
//
// body 由 SDK 读取/关闭；调用方 cap transport 保证 8 MiB。本函数不写账号状态、
// 不刷新、不回调、不写日志；raw 只供 probe 做成功判定/error detail/预览抽取。
func GenerateImageOnce(
	ctx context.Context,
	token, accountID string,
	transport http.RoundTripper,
	params *domain.ImageGenParams,
) (status int, body []byte, err error) {
	if transport == nil {
		return 0, nil, errors.New("sdkbridge: generate image transport is nil")
	}
	capped := &oneShotTransport{next: transport}
	client := codexsdk.NewHTTPClient(
		codexsdk.PAT(token, codexsdk.WithPATAccountID(accountID)),
		codexsdk.WithTransport(capped),
	)
	img, callErr := client.GenerateImage(ctx, toSDKParams(params))
	status = int(capped.status.Load())
	read := capped.read.Load()
	if callErr != nil {
		var revoked *codexsdk.AuthPermanentlyRevokedError
		if errors.As(callErr, &revoked) {
			return http.StatusUnauthorized, revoked.Raw, nil
		}
		var httpErr *codexsdk.HTTPError
		if errors.As(callErr, &httpErr) {
			return httpErr.StatusCode, httpErr.Raw, nil
		}
		if status == http.StatusOK && read > oneShotImageMaxBodyBytes {
			return status, nil, ErrBodyTooLarge
		}
		return status, nil, callErr
	}
	if img == nil {
		return status, nil, errors.New("sdkbridge: generate image got nil response")
	}
	wire, marshalErr := MarshalImageResponse(fromSDKResponse(img))
	if marshalErr != nil {
		return status, nil, marshalErr
	}
	if status == http.StatusOK && read > oneShotImageMaxBodyBytes {
		return status, nil, ErrBodyTooLarge
	}
	return status, wire, nil
}

// SearchOnce 用 codex SDK 的 Search 发一次 /backend-api/codex/alpha/search 请求
// （诊断/探测 one-shot seam）：临时 PAT client + WithTransport，无 ClientTable/
// Auth 缓存/fatal 回调/轮转/日志。
//
// 返回契约：
//   - 任何 HTTP 响应 → 真实 status + raw，err=nil；200 且 len(raw) >2 MiB →
//     ErrBodyTooLarge（非 200 由调用方 cap 截断到恰好 2 MiB，不判溢出）。
//   - AuthPermanentlyRevokedError → 401 + raw。
//   - 传输错误 → 捕获 status（无则 0）+ err。
//
// body 由 SDK 读取/关闭；本函数不写账号状态、不刷新、不回调、不写日志；raw 只
// 供 probe 做成功判定/error detail/预览抽取，绝不直接进入结果。
func SearchOnce(
	ctx context.Context,
	token, accountID string,
	transport http.RoundTripper,
	payload []byte,
) (status int, body []byte, err error) {
	if transport == nil {
		return 0, nil, errors.New("sdkbridge: search transport is nil")
	}
	capped := &oneShotTransport{next: transport}
	client := codexsdk.NewHTTPClient(
		codexsdk.PAT(token, codexsdk.WithPATAccountID(accountID)),
		codexsdk.WithTransport(capped),
	)
	resp, callErr := client.Search(ctx, payload)
	status = int(capped.status.Load())
	if callErr != nil {
		var revoked *codexsdk.AuthPermanentlyRevokedError
		if errors.As(callErr, &revoked) {
			return http.StatusUnauthorized, revoked.Raw, nil
		}
		var httpErr *codexsdk.HTTPError
		if errors.As(callErr, &httpErr) {
			return httpErr.StatusCode, httpErr.Raw, nil
		}
		return status, nil, callErr
	}
	if resp == nil {
		return status, nil, fmt.Errorf("sdkbridge: search got nil response")
	}
	if resp.StatusCode == http.StatusOK && len(resp.Raw) > oneShotMaxBodyBytes {
		return resp.StatusCode, nil, ErrBodyTooLarge
	}
	return resp.StatusCode, resp.Raw, nil
}
