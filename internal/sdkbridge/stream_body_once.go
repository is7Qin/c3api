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

	codexsdk "github.com/is7Qin/codex-sdk"
)

// BodyReader 读取并解释 200 响应体；调用方（internal/probe）拥有读取上限与
// 终态语义。实现必须**只读**，不得 Close——响应体由 StreamBodyOnce 关闭。
type BodyReader func(io.ReadCloser) ([]byte, error)

// StreamBodyOnce 用 codex SDK 的 HTTP arm（StreamBody）发一次请求，只搬运
// SDK 机制，不包含探测语义。
//
// 这是诊断/探测的一次性 seam，刻意不复用生产 sdkbridge.Codex：
//   - 不取 ClientTable 条目、不构造账号级 Auth 缓存（每次调用一条临时 SDK client）;
//   - 不传 WithPATOnAuthFatal（无 fatal 回调、不写 failed_at/account_ext）；
//   - 不存在 OAuth 轮转（PAT 不是 rotationTrigger；401/403 原样作为结论）；
//   - 传输守卫（origin 锁定 + 响应体硬上限）由调用方构造后传入。
//
// 返回契约：
//   - 任何 HTTP 响应（200/201/204/3xx/4xx/5xx）→ status + body，err=nil；
//     200 的 body 是 readBody 的结果；非 200 的 body 是 SDK 已读尽、经调用方
//     cap 截断的 raw；AuthPermanentlyRevokedError 按 401 处理。
//   - 传输错误（含 nil transport）、200 body 读取错误、nil response → status=0
//   - err；错误文本不进入结果，由调用方映射固定文案。
//   - 200 body 由本函数在 readBody 返回后**恰好关闭一次**；readBody 不得关闭。
//   - 不带 timeout 参数：期限只由 ctx 决定，不叠加 SDK http.Client.Timeout。
//   - 不构造 payload、不识别 SSE 终态、不写日志。
func StreamBodyOnce(
	ctx context.Context,
	token, accountID string,
	transport http.RoundTripper,
	payload []byte,
	readBody BodyReader,
) (status int, body []byte, err error) {
	if transport == nil {
		return 0, nil, errors.New("sdkbridge: stream body transport is nil")
	}
	if readBody == nil {
		return 0, nil, errors.New("sdkbridge: stream body reader is nil")
	}
	client := codexsdk.NewHTTPClient(
		codexsdk.PAT(token, codexsdk.WithPATAccountID(accountID)),
		codexsdk.WithTransport(transport),
	)
	resp, callErr := client.StreamBody(ctx, payload, codexsdk.Identity{})
	if callErr != nil {
		// SDK 对非 200 已读尽并关闭 body，只留 HTTPError/Raw；401 判死则留
		// AuthPermanentlyRevokedError。两者都是 HTTP 响应，按真实状态返回。
		var revoked *codexsdk.AuthPermanentlyRevokedError
		if errors.As(callErr, &revoked) {
			return http.StatusUnauthorized, revoked.Raw, nil
		}
		var httpErr *codexsdk.HTTPError
		if errors.As(callErr, &httpErr) {
			return httpErr.StatusCode, httpErr.Raw, nil
		}
		return 0, nil, callErr
	}
	if resp == nil {
		return 0, nil, fmt.Errorf("sdkbridge: stream body got nil response")
	}
	// 200 分支：SDK 返回未关闭的 body，归本函数恰好关闭一次（含 readBody 错误）。
	defer func() { _ = resp.Body.Close() }()
	body, readErr := readBody(resp.Body)
	if readErr != nil {
		return 0, body, readErr
	}
	return resp.StatusCode, body, nil
}
