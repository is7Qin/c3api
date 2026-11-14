// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"errors"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
)

// deactivatedWorkspaceBody 是用户实证的上游原始体（workspace 被停用）：
// 402 Payment Required + 业务码 deactivated_workspace。原样贴进断言——形状变了
// 就该红，不要"看着差不多"。
const deactivatedWorkspaceBody = `{
  "error": {
    "message": "Payment Required",
    "type": null,
    "code": "deactivated_workspace",
    "param": null
  },
  "status": 402
}`

// TestClassifyAccountFatal 业务面判死码分类（唯一事实源）：
//   - 两种承载形状都命中（裸 error 信封体 / WS 错误事件帧）
//   - status 取调用方已知值，未知（0）时回退读体内顶层 status
//   - 只看 error.code / detail.code 两处错误码字段：业务文本里出现同样的字样
//     不判死（那条误判的代价是把好账号摘掉，只能人工 /recover）
//   - 402 但没有判死码（额度/计费类可恢复）→ 不判死
func TestClassifyAccountFatal(t *testing.T) {
	// 用户实证形状：status 由体内顶层字段给出（HTTP 面调用方也会传 402）
	got := ClassifyAccountFatal(0, []byte(deactivatedWorkspaceBody))
	require.NotNil(t, got, "用户实证形状必须命中")
	require.Equal(t, 402, got.Status, "status 回退读体内顶层字段")
	require.Equal(t, "deactivated_workspace", got.Code)
	require.Contains(t, got.Error(), "deactivated_workspace", "last_error 审计文本带判死依据")

	// 调用方已知状态码 → 用它（体内无 status 的 HTTP 体）
	got = ClassifyAccountFatal(402, []byte(`{"error":{"message":"Payment Required","code":"deactivated_workspace"}}`))
	require.NotNil(t, got)
	require.Equal(t, 402, got.Status)

	// WS 错误事件帧（带 type + status）
	got = ClassifyAccountFatal(0, []byte(`{"type":"error","status":402,"error":{"code":"deactivated_workspace","message":"Payment Required"}}`))
	require.NotNil(t, got)
	require.Equal(t, 402, got.Status)

	// detail.code 形态（ChatGPT 内部 API 风格）
	got = ClassifyAccountFatal(402, []byte(`{"detail":{"code":"deactivated_workspace"}}`))
	require.NotNil(t, got)
	require.Equal(t, "deactivated_workspace", got.Code)

	// 码值大小写不敏感（上游码表大小写不保证）
	got = ClassifyAccountFatal(402, []byte(`{"error":{"code":"Deactivated_Workspace"}}`))
	require.NotNil(t, got)

	// 只认错误码字段：业务文本/用户回显里出现同样字样 → 不判死
	require.Nil(t, ClassifyAccountFatal(0, []byte(`{"type":"response.output_text.delta","delta":"deactivated_workspace"}`)))
	require.Nil(t, ClassifyAccountFatal(0, []byte(`{"error":{"message":"deactivated_workspace"}}`)),
		"message 不是错误码字段")
	// 402 但码不同（额度/计费类——可恢复，换号/重试有意义）
	require.Nil(t, ClassifyAccountFatal(402, []byte(`{"error":{"code":"insufficient_quota"}}`)))
	// 空 / 非 JSON / 无关
	require.Nil(t, ClassifyAccountFatal(0, nil))
	require.Nil(t, ClassifyAccountFatal(402, []byte(`{"error":{"message":"Payment Required"}}`)))
	require.Nil(t, ClassifyAccountFatal(200, []byte(`{"ok":true}`)))
}

// TestClassifyAccountFatalZeroAllocOnMiss WS 每帧判死嗅探是热路径：未命中必须零
// 分配（短针预筛命中不了就直接返回，不碰 gjson）。命中面本来就要分配（构造错误
// 对象），不在门禁范围。
func TestClassifyAccountFatalZeroAllocOnMiss(t *testing.T) {
	content := []byte(`{"type":"response.output_text.delta","delta":"lorem ipsum dolor sit amet consectetur adipiscing elit"}`)
	allocs := testing.AllocsPerRun(100, func() {
		if ClassifyAccountFatal(0, content) != nil {
			t.Fatal("内容帧不得判死")
		}
	})
	require.Zero(t, allocs, "未命中零分配")
}

// TestTranslateErrorAccountFatal HTTP 面接线：*HTTPError 402 + 判死码 ⇒
// 判死上报一次 + 返回 AccountFatalError（IsFatal 命中 ⇒ 网关不换号）。
func TestTranslateErrorAccountFatal(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	cred := oauthCred(7, "at-1", "rt-1")
	prep, err := a.Prepare(cred)
	require.NoError(t, err)

	he := &codexsdk.HTTPError{StatusCode: 402, Raw: []byte(deactivatedWorkspaceBody)}
	out := a.translateError(prep.entry, prep.srcRev, he)
	require.Error(t, out)

	var af *AccountFatalError
	require.ErrorAs(t, out, &af, "返回账号级终止错误（不是信封）")
	require.Equal(t, 402, af.StatusCode())
	require.Contains(t, af.RawJSON(), "deactivated_workspace", "信封原文保留（err_logs/规则匹配）")
	require.ErrorIs(t, out, he, "包装链保留（errors.Is/As 可回溯 SDK 错误）")
	require.True(t, IsFatal(out), "IsFatal 命中 ⇒ 网关不换号")

	calls := handler.snapshot()
	require.Len(t, calls, 1, "判死上报一次")
	require.Equal(t, int64(7), calls[0].accountID)
	require.Equal(t, prep.srcRev, calls[0].sourceRevision, "来源 K 来自请求句柄")
	require.ErrorAs(t, calls[0].fatal, &af, "上报原因就是这条判死错误（last_error 带码）")

	// 同一 fatal 二次经过（双源去重路径）→ 仍单次上报
	require.Error(t, a.translateError(prep.entry, prep.srcRev, he))
	require.Len(t, handler.snapshot(), 1, "双源去重：同一条目只上报一次")
}

// TestTranslateError402NonFatal HTTP 面 402 但**无判死码**：保持信封语义
// （不判死、不摘号）——"上游拒了这次请求"与"这个账号再也不会成功"是两件事。
func TestTranslateError402NonFatal(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})
	prep, err := a.Prepare(oauthCred(7, "at-1", "rt-1"))
	require.NoError(t, err)

	out := a.translateError(prep.entry, prep.srcRev,
		&codexsdk.HTTPError{StatusCode: 402, Raw: []byte(`{"error":{"message":"Payment Required","code":"insufficient_quota"}}`)})
	var env *EnvelopeError
	require.ErrorAs(t, out, &env, "非判死 402 → 信封（现状语义不变）")
	require.Equal(t, 402, env.StatusCode())
	require.False(t, IsFatal(out), "非判死码不得进 fatal 集")
	require.Empty(t, handler.snapshot(), "不上报失效")
}

// TestTranslateDialErrorAccountFatal WS 升级面接线：*DialError 402 + 判死码 ⇒
// 同上（DialError.Body 是升级响应体，SDK ≤1KiB 截断）。
func TestTranslateDialErrorAccountFatal(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})
	prep, err := a.Prepare(oauthCred(9, "at-9", "rt-9"))
	require.NoError(t, err)

	de := &codexsdk.DialError{StatusCode: 402, Err: errors.New("handshake failed"), Body: []byte(deactivatedWorkspaceBody)}
	out := a.translateDialError(prep.entry, prep.srcRev, de)
	require.Error(t, out)

	var af *AccountFatalError
	require.ErrorAs(t, out, &af, "升级 402 判死 ⇒ 不是信封")
	require.Equal(t, 402, af.Status)
	require.True(t, IsFatal(out), "IsFatal 命中 ⇒ handleCodexDialError 走 fatal 终态（不转移）")
	require.ErrorIs(t, out, de, "包装链保留")

	calls := handler.snapshot()
	require.Len(t, calls, 1, "判死上报一次")
	require.Equal(t, int64(9), calls[0].accountID)
}

// TestTranslateDialError402NonFatal 升级 402 无判死码 → 信封（Refreshed 语义也
// 必须原样保留：那是"已轮转过、别双份刷新"的判据）。
func TestTranslateDialError402NonFatal(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})
	prep, err := a.Prepare(oauthCred(9, "at-9", "rt-9"))
	require.NoError(t, err)

	de := &codexsdk.DialError{StatusCode: 402, Err: errors.New("x"), Body: []byte(`{"error":{"message":"Payment Required"}}`), Refreshed: true}
	out := a.translateDialError(prep.entry, prep.srcRev, de)
	var env *EnvelopeError
	require.ErrorAs(t, out, &env)
	require.True(t, env.Refreshed, "Refreshed 语义保留")
	require.Empty(t, handler.snapshot())
}

// TestAccountFatalErrorEnvelopeProtocol 信封协议面：statusOf 走 StatusCode()、
// causeMessage 走 RawJSON()（网关零改动复用），Unwrap 保留底层链。
func TestAccountFatalErrorEnvelopeProtocol(t *testing.T) {
	underlying := &codexsdk.HTTPError{StatusCode: 402, Raw: []byte(deactivatedWorkspaceBody)}
	e := ClassifyAccountFatal(402, []byte(deactivatedWorkspaceBody))
	require.NotNil(t, e)
	e.Err = underlying

	type statusCoder interface{ StatusCode() int }
	type rawJSONer interface{ RawJSON() string }
	var sc statusCoder
	var rj rawJSONer
	require.True(t, errors.As(error(e), &sc), "statusOf 方法面")
	require.Equal(t, 402, sc.StatusCode())
	require.True(t, errors.As(error(e), &rj), "upstreamBody 方法面")
	require.Contains(t, rj.RawJSON(), "deactivated_workspace")
	var he *codexsdk.HTTPError
	require.ErrorAs(t, error(e), &he, "Unwrap 链可回溯 SDK 错误")
}
