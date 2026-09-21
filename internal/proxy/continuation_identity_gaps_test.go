// SPDX-License-Identifier: AGPL-3.0-or-later
// Continuation 绑定身份是 (I,K) 的缺口断言：已有覆盖只管段 (ii) 的正向
// （同账号只推进 C 仍 pin 成功、推进 K 则 409）。本文件补三块：段 (i) 的正向
// （无关账号的配置写入推进组代际 G，等发布完成后续跑仍 pin 成功）与其反向
// （把绑定身份换成 G 则同一序列必须 409），以及段 (ii) 的反向（把绑存与尝试
// 携带两侧都换成 C 则必须 409）。
package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// TestContinuationUnrelatedConfigWriteKeepsPin 段 (i) 正向：无关账号的配置
// 写入（改名，C 推进而 I、K 不变；等发布完成，组代际 G 推进）不得作废在途续
// 跑——续跑仍 pin 到绑定账号。
func TestContinuationUnrelatedConfigWriteKeepsPin(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	up1 := contUpstream(t, "resp_unrelated", "sk-acc1", "", &hits1, &auth1)
	defer up1.Close()
	up2 := contUpstream(t, "resp_unrelated_other", "sk-acc2", "", &hits2, &auth2)
	defer up2.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up1.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", up1.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", up2.URL)
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, hits1.Load(), "创建派发到首选的绑定账号")
	genBefore := p.sched.View().Generation()

	// 无关账号的配置写入：改名（C 推进），指纹与 K 不变；随后等发布完成
	// （组代际 G 推进）。
	acc2.Name = "unrelated-renamed"
	acc2.LifecycleRevision = 2
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)
	genAfter := p.sched.View().Generation()
	require.Greater(t, genAfter, genBefore, "发布必须推进组代际，否则本用例空转")

	w2 := httptest.NewRecorder()
	p.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_unrelated"}`))
	require.Equal(t, 200, w2.Code, "无关写入推进 G 不得作废绑定，body=%s", w2.Body.String())
	require.EqualValues(t, 2, hits1.Load(), "续跑仍派发到绑定账号")
	require.EqualValues(t, 0, hits2.Load(), "续跑不得迁移到无关账号")
	require.Equal(t, "Bearer sk-acc1", auth1.Load(), "续跑仍用绑定账号的凭据")
}

// TestContinuationBindingAsGenerationFailsClosed 段 (i) 反向：若绑定身份是组
// 代际 G（绑存绑定时的 G，尝试携带续跑时的 G），则上面的同一序列必须 409——
// 他人的一次改名就会 409 掉在途会话，这正是 G 不能作绑定身份的理由。
func TestContinuationBindingAsGenerationFailsClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_gen", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", up.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", up.URL)
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s)
	genBound := p.sched.View().Generation()

	// 同一段 (i) 的无关写入与发布：组代际推进。
	acc2.Name = "unrelated-renamed"
	acc2.LifecycleRevision = 2
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)
	genLater := p.sched.View().Generation()
	require.Greater(t, genLater, genBound, "发布必须推进组代际，否则本用例空转")

	// 续跑时刻的尝试：其路由代际即当前视图代际。
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-gen-reverse", UserID: 1})
	require.NoError(t, err)
	require.Equal(t, genLater, attempt.RoutingGeneration, "续跑尝试携带发布后的代际")

	// 把两侧的 revision 语义都换成 G：绑存绑定时的 G，尝试携带续跑时的 G。
	b := &continuation.Binding{AccountID: attempt.AccountID, Fingerprint: parityFP(t, attempt.CandidateFingerprint), IdentityRevision: int64(genBound)}
	attempt.IdentityRevision = int64(genLater)
	_, _, ferr := p.contPin(&plan, sel, attempt, b)
	require.NotNil(t, ferr, "以 G 为身份时，无关写入后的续跑必须 fail closed")
	require.Equal(t, http.StatusConflict, ferr.status)
}

// TestContinuationBindingAsLifecycleTokenFailsClosed 段 (ii) 反向：把绑存与
// 尝试携带两侧都换成客户端 CAS 令牌 C（绑存改名前的 C，尝试携带改名后的 C），
// 则同账号的一次改名必须 409。对照组证明只换一侧不够：C 初值与 K 初值都是 1，
// 只换绑存一侧会碰巧匹配而 pin 成功，故反向必须两侧同换。
func TestContinuationBindingAsLifecycleTokenFailsClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_tok", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	// 两侧同换成 C：绑存改名前的 C（=1），尝试携带改名后的 C（=2）。
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-tok-reverse", UserID: 1})
	require.NoError(t, err)
	b := &continuation.Binding{AccountID: attempt.AccountID, Fingerprint: parityFP(t, attempt.CandidateFingerprint), IdentityRevision: 1}
	attempt.IdentityRevision = 2
	_, _, ferr := p.contPin(&plan, sel, attempt, b)
	require.NotNil(t, ferr, "以 C 为身份时，同账号改名后的续跑必须 fail closed")
	require.Equal(t, http.StatusConflict, ferr.status)

	// 对照：只换绑存一侧（绑存 C=1，尝试仍携带 K=1）会碰巧匹配而 pin 成功——
	// C 与 K 初值都是 1。这正是反向必须两侧同换的理由。
	sel2, plan2, attempt2, err := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-tok-coincide", UserID: 1})
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt2.IdentityRevision)
	b2 := &continuation.Binding{AccountID: attempt2.AccountID, Fingerprint: parityFP(t, attempt2.CandidateFingerprint), IdentityRevision: 1}
	pinned, _, ferr2 := p.contPin(&plan2, sel2, attempt2, b2)
	require.Nil(t, ferr2, "单侧换 C 在初值相等时碰巧匹配，这是反向必须两侧同换的实证")
	require.NotNil(t, pinned)
	pinned.Release()
}
