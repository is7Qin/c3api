// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// --- 业务面账号级终止（402 + 业务错误码） ---
//
// 边界（为什么在网关侧，不在 SDK）：SDK 对 responses **业务**错误体是零解析纪律
// （只把原始字节交回网关，auth_oauth.go「responses 业务协议零解析纪律不受影响」）；
// 留在 SDK 的鉴权码集（token_invalidated / token_revoked）之所以在里面，是因为
// refresh 轮转循环归 SDK 所有。同一条支配线：**谁拥有那条循环，谁拥有它的码集**。
// 业务错误体归网关解析 ⇒ 业务判死码集也归网关。

// accountFatalCodeDeactivatedWorkspace 工作区被停用：上游 402 Payment Required 的
// 业务错误码。
//
// 实证形状（上游两种承载都见过，同一事实）：
//
//	WS 错误事件帧  {"type":"error","status":402,"error":{"code":"deactivated_workspace"}}
//	HTTP 错误体    {"error":{"message":"Payment Required","code":"deactivated_workspace"},"status":402}
//
// 语义：workspace 已停用。**同一账号重试多少次都是同一个 402**——换号能救请求，
// 留在池里只会让每一条派发都白烧一次往返，所以是账号级终态（判死：failed_at +
// failure_source，恢复唯一入口 = 管理面 /recover fenced 端点）。
//
// 只收实证过的码（用户 2026-11-13 实证）。不要因为"看起来也像"扩码：
// 误判死的代价是账号被摘除、需要人工 /recover；漏判的代价只是多烧一次派发。
const accountFatalCodeDeactivatedWorkspace = "deactivated_workspace"

// AccountFatalError 业务面账号级终止（网关侧分类结论）。
//
// 实现信封协议（StatusCode()/RawJSON()）：statusOf 拿到上游真实状态码、causeMessage
// 拿到上游原文（err_logs.error_message 落库 + 规则 error_message_contains 匹配都靠
// 它）。Unwrap 保留被包装的原始错误链——errors.Is/As 仍可回溯到 *codexsdk.HTTPError /
// *codexsdk.DialError（判死/重试分类不必区分包装层）。
type AccountFatalError struct {
	Status int    // 上游状态码（0 = 上游没给过/未知）
	Code   string // 判死依据业务码（accountFatalCode*）
	Raw    []byte // 上游错误体/错误帧原样（诊断 + 信封协议）
	Err    error  // 被包装的 SDK 错误；nil = 无
}

func (e *AccountFatalError) Error() string {
	return fmt.Sprintf("sdkbridge: 上游账号级终止（HTTP %d, code=%s）", e.Status, e.Code)
}

func (e *AccountFatalError) StatusCode() int { return e.Status }

func (e *AccountFatalError) RawJSON() string { return string(e.Raw) }

func (e *AccountFatalError) Unwrap() error { return e.Err }

// ClassifyAccountFatal 判定上游错误体/错误帧是否携带账号级终止码。
//
// status 是调用方**已知**的传输面状态码（HTTP 响应码 / 升级响应码 / WS 帧未知则
// 0）；为 0 时回退读体里的顶层 "status"（WS 错误事件帧把状态码放在帧内）。
//
// 命中返回 *AccountFatalError（Status/Code/Raw 已填，Err 由调用方补包装链），
// 未命中返回 nil（返回具体类型而非 error：调用方要挂包装链，断言回自身是白付一次）。
//
// **调用纪律**：未命中是 nil 指针。要把结果交给 `error` 接口（例如作为函数返回值
// 上抛）必须先 `if af != nil`——typed-nil 装进接口后 `err != nil` 成立，在逐帧
// 判死嗅探点上等于"每一帧都判死"：账号被毒化摘除、happy path 直接断流。
//
// 判定只看 `error.code` / `detail.code` 两处**错误码字段**：body 里别处出现同样
// 的字样（用户 prompt 回显、业务文本）不判死。
//
// 热路径纪律：先短针预筛（未命中不碰 gjson、零分配返回 nil）。调用点（WS 每帧
// 判死嗅探）就是热路径——这条预筛与 `sniffCodexWSDeath` 既有 bytes.Contains
// 同档，不引入逐帧反射或二次解包。预筛针只打在**字段名**上（`"code"`），不打在
// 码值上：码值按 EqualFold 判（与 SDK 判死码集同等纪律——上游码表大小写不保证），
// 打在值上的针一遇到大写形态就把整条判死静默漏掉。
func ClassifyAccountFatal(status int, body []byte) *AccountFatalError {
	if !bytes.Contains(body, []byte(`"code"`)) {
		return nil
	}
	code := ""
	for _, path := range [...]string{"error.code", "detail.code"} {
		if v := gjson.GetBytes(body, path); v.Exists() &&
			strings.EqualFold(v.String(), accountFatalCodeDeactivatedWorkspace) {
			code = accountFatalCodeDeactivatedWorkspace
			break
		}
	}
	if code == "" {
		return nil
	}
	if status <= 0 {
		if v := gjson.GetBytes(body, "status"); v.Type == gjson.Number {
			status = int(v.Int())
		}
	}
	return &AccountFatalError{Status: status, Code: code, Raw: body}
}
