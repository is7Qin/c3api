// SPDX-License-Identifier: AGPL-3.0-or-later
// Continuation 的绑定身份是 (候选指纹 I, K)：本文件钉住"模板侧身份变化必须作废
// 依赖旧身份的在途续跑"——翻转模板的 strip_image_tools 使 I 改变，而 K 与 C 都不
// 变，旧绑定必须 fail closed 且不再派发。
package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// TestContinuationTemplateIdentityChangeFailsClosed 模板侧身份写入（换模板对象并
// 翻转 strip_image_tools ⇒ 候选指纹改变）之后，依赖旧指纹的绑定不得 pin 成功。
// 用例自带前提断言：指纹确实变了、K 与 C 确实没变——否则 pin 成功与本断言失败
// 无法区分（若指纹未变，续跑会 200，本用例同样失败，故不存在空转）。
func TestContinuationTemplateIdentityChangeFailsClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_tplid", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc := contAcc(1, tpl, "sk-acc1", up.URL)
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, hits.Load(), "创建派发到绑定账号")

	selB, _, attB, err := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-tpl-before", UserID: 1})
	require.NoError(t, err)
	selB.Release()

	// 模板侧身份写入：换上一个**新的**模板对象（旧快照的不可变性依赖它）并翻转
	// strip_image_tools；账号行、K、C 都不动。
	next := *tpl
	next.StripImageTools = true
	acc.Template = &next
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	selA, _, attA, err := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-tpl-after", UserID: 1})
	require.NoError(t, err)
	selA.Release()
	require.NotEqual(t, attB.CandidateFingerprint, attA.CandidateFingerprint,
		"前提：模板侧改动确实改变了候选身份指纹")
	require.Equal(t, attB.IdentityRevision, attA.IdentityRevision,
		"前提：模板写入不推进 K")
	require.Equal(t, int64(1), acc.LifecycleRevision, "前提：模板写入不推进 C")

	// 断言落在 pin 路径的 409（errContStale）：指纹比较一旦被摘掉，请求会落到
	// 绑定刷新的第二道围栏（502 errContConflict），二者都 fail closed，但只有
	// 409 这一码能钉住"pin 谓词确实按指纹判"。
	w2 := httptest.NewRecorder()
	p.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_tplid"}`))
	require.Equal(t, http.StatusConflict, w2.Code,
		"身份已变 ⇒ 旧绑定必须 fail closed，body=%s", w2.Body.String())
	require.EqualValues(t, 1, hits.Load(), "陈旧身份不得派发")
}
