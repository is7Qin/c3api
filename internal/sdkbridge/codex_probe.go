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
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ProbeResponses 用 codex SDK 的 HTTP arm（StreamBody）发一次静态探测请求。
//
// 这是 account-model-test 的**一次性探测机制**，刻意不复用生产 sdkbridge.Codex：
//   - 不取 ClientTable 条目、不构造账号级 Auth 缓存（每次调用一条临时 SDK client）;
//   - 不传 WithPATOnAuthFatal（无 fatal 回调、不写 failed_at/account_ext）；
//   - 不存在 OAuth 轮转（PAT 不是 rotationTrigger；401/403 原样作为结论）；
//   - 传输守卫（origin 锁定 + 响应体硬上限）由调用方 internal/probe 构造后传入，
//     本函数只负责 SDK 机制，不重复实现守卫。
//
// 返回约定：
//   - 200：status=200，body 读取至多 2 MiB+1 字节后关闭（len > 2 MiB 由调用方判
//     溢出失败）；
//   - 非 200：*codexsdk.HTTPError → 其 StatusCode/Raw（Raw 经调用方 capTransport
//     截断到 ≤2 MiB）；*codexsdk.AuthPermanentlyRevokedError → status=401 +
//     Error()/Raw；
//   - 传输失败（拨号/超时/守卫拒绝）：status=0，err 为底层错误。
func ProbeResponses(ctx context.Context, token, accountID string, transport http.RoundTripper, payload []byte, timeout time.Duration) (status int, body []byte, err error) {
	if transport == nil {
		return 0, nil, errors.New("sdkbridge: probe transport is nil")
	}
	client := codexsdk.NewHTTPClient(
		codexsdk.PAT(token, codexsdk.WithPATAccountID(accountID)),
		codexsdk.WithTransport(transport),
		codexsdk.WithTimeout(timeout),
	)
	resp, callErr := client.StreamBody(ctx, payload, codexsdk.Identity{})
	if callErr != nil {
		var revoked *codexsdk.AuthPermanentlyRevokedError
		if errors.As(callErr, &revoked) {
			return http.StatusUnauthorized, revoked.Raw, callErr
		}
		var httpErr *codexsdk.HTTPError
		if errors.As(callErr, &httpErr) {
			return httpErr.StatusCode, httpErr.Raw, callErr
		}
		return 0, nil, callErr
	}
	if resp == nil {
		return 0, nil, fmt.Errorf("sdkbridge: probe got nil response")
	}
	// 200 分支：SDK 不读 body（HTTP arm 只做状态检查），由本函数读取并**恰好
	// 关闭一次**。读取上限 = 2 MiB+1，多一个字节用于调用方判溢出。
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, domain.ProbeMaxBodyBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, body, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, body, closeErr
	}
	return resp.StatusCode, body, nil
}
