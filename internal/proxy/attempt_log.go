// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// attempt_logs 写点（spec 2026-11-18 retry-observability §3.2）：failover 循环里
// 一次 **handled=false** 的 dispatch 落一行明细。
//
// 覆盖面（诚实版，与 spec 一致）：只覆盖**循环可见**的失败——成功（handled=true）
// 与本地拒绝（401/额度/402 预检，循环外）**不产生行**，它们的真值仍在
// usage_logs / err_logs。本表只写不读（S3 的链式查询走 SQL）。
//
// 本文件是唯一写点：`recordFailedDispatch` 由 failover 循环调用一次/一次失败
// dispatch，别处不要投递——行数就是「这次请求打了几次上游」，加第二个投递点会让
// 这个数字变成两倍的谎。

import (
	"context"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// AttemptLogSink 是 attempt_logs 的投递面（`*usage.AttemptLogWorker` 是唯一的
// 生产实现：有界队列 + 非阻塞投递 + 批量落库 + 丢弃计数）。
//
// 为什么是接口而不是具体类型：写点必须能被测试用**注入 sink** 断言（不经 DB、
// 不 flake），而 proxy 不需要知道队列/批量/保留期的任何细节。
type AttemptLogSink interface {
	Enqueue(row *domain.AttemptLog)
}

// SetAttemptLogs 注入 attempt_logs 投递通道（装配期一次；nil = 未装配 → 写点
// no-op）。
//
// typed nil 在这里被**显式拒绝**：`(*usage.AttemptLogWorker)(nil)` 装进接口是
// **非 nil 接口**，写点会调进 nil 接收者（`w.mu.Lock()` 立刻 panic）。装配点
// （wiring_codex.go）也判一次——两道防线：那道管"不要传"，这道兜住"万一传了"。
// 传字面量 nil 表示合法的未装配形态。
func (p *Proxy) SetAttemptLogs(s AttemptLogSink) {
	if w, ok := s.(*usage.AttemptLogWorker); ok && w == nil {
		p.attemptLogs = nil // typed nil 装进接口是非 nil 接口，写点会调进 nil 接收者
		return
	}
	p.attemptLogs = s
}

// recordFailedDispatch 落一行 attempt_logs（唯一投递点，见文件头）。
//
// 参数即循环里已有的实参：attempt 是本次 dispatch 的计划尝试身份（ordinal /
// account / lane 的唯一来源，**不是** sel 或计数器的推导值），code/callErr 是
// 上游结论，clientCancel 与观察面共用 `dispatchClientCancel`（同一判据，避免
// outcome 与 error_type 在取消语义上分叉），msg 是 classify 落进 err_logs 的
// 那条文本（空 = 无文本 → NULL）。
//
// 无条件性：本函数不读 qualityRecorder/foldOwner —— 观测面未装配时 flow 边会
// 丢弃，但尝试明细照写（它是审计事实，不是观测面的副产品）。
func (p *Proxy) recordFailedDispatch(attempt api.Attempt, reqID string, format domain.RequestFormat, code int, clientCancel bool, callErr error, msg string, latency time.Duration) {
	if p.attemptLogs == nil {
		return
	}
	// outcome 投影复用既有单一来源（dispatchFailureOutcome + flowOutcomeToken）：
	// terminal 判定与 observeDispatchFailure 逐字同式，否则同一次失败会在 flow
	// 边与本表给出两种结局。
	terminal := code != 0 && code != http.StatusTooManyRequests
	outcome := dispatchFailureOutcome(pipelineBase(attempt), code, clientCancel, terminal)
	row := &domain.AttemptLog{
		RequestID: reqID,
		Ordinal:   int16(attempt.Ordinal),
		AccountID: attempt.AccountID,
		// 未知 lane（codebook 外）= 空串：**不发明**第四个值，也不落 "primary"
		// 这种假值（foldLaneCodeOf 的 comma-ok 就是为这个陷阱写的）。
		// 真出现时 S3 会显示空 lane，比错标成 primary 可查。
		Lane:         foldLaneTokenOf(foldLaneCodeOf(attempt.Lane)),
		Format:       string(format),
		StatusCode:   code,
		Outcome:      flowOutcomeToken(outcome),
		ErrorType:    string(attemptErrorType(code, callErr, clientCancel)),
		CommitState:  new(outcome.Commit.String()), // handled=false 恒已知，不留 NULL
		ErrorMessage: optionalMessage(msg),
		LatencyMS:    latency.Milliseconds(),
		CreatedAt:    time.Now(),
	}
	p.attemptLogs.Enqueue(row)
}

// optionalMessage 空文本 → NULL（与 usage_logs/err_logs 的 ErrorMessage 同语义：
// 空串会变成一条"有错误但说不出原因"的行，NULL 才是"没有文本"）。
func optionalMessage(msg string) *string {
	if msg == "" {
		return nil
	}
	return &msg
}

// attemptErrorType 一次失败 dispatch 的 error_type 投影（词表 = domain.Err*）。
//
// 与 classifyFailoverAttempt / writeFailoverExhausted 的分类**同词表，但不共用
// 实现**：那两处按规则事件（rule.Kind）裁定冷却与投递，本函数只做落库列的翻译，
// 且多一个来自调用方的 clientCancel（取消的权威判据是请求 ctx，不在 code/callErr
// 里——这里再判一次就成了第二套判定）。
//
// 与 writeFailoverExhausted 的关系：耗尽行曾对 fatal/BPS-4xx 沿用默认的 Err5xx，
// 而同一跳在这里是 Err4xx——同一个 attempt 两张表两种类型。现在那边也按 4xx 区间
// 判（429 先判），两边对同一跳给出同一结论；分工仍是「本函数翻译落库列，那边裁定
// 终态行的投递」。
func attemptErrorType(code int, callErr error, clientCancel bool) domain.ErrorType {
	switch {
	case clientCancel:
		return domain.ErrAbort
	case code == http.StatusTooManyRequests:
		return domain.Err429
	case code >= 400 && code < 500:
		return domain.Err4xx
	case code == 0:
		// 零状态两分（网关预算到期 = 上游预算内零产出 ⇒ timeout；其余 ⇒ 传输层
		// 断开 = network）：判定复用 zeroStatusFailureClass（单一事实源）。
		et, _ := zeroStatusFailureClass(callErr)
		return et
	default:
		return domain.Err5xx
	}
}

// dispatchClientCancel 首字节前客户端断连的**唯一**判据（观察面
// observeDispatchFailure 与 attempt 明细共用）：零状态 + 请求 ctx 已取消。
//
// 抽出来是为了让两处不可能写成近似条件——一旦分叉，同一次取消会在 flow 边上记
// client_cancel、在本表记 network。nil ctx 按「未取消」处理：重试矩阵
// （retryOutcomeForAttempt）的既有判据就是这样写的，重构不得顺手收紧或放松它
// ——nil interface 上取 Err() 是 panic，而这里只是判一次取消。
func dispatchClientCancel(ctx context.Context, code int) bool {
	return code == 0 && ctx != nil && ctx.Err() != nil
}

// attemptOrdinalPtr 尝试序号投影（err_logs.attempt_ordinal）。ordinal == 0 恒表示
// 「没有 attempt」（scheduler.AttemptPlan 要求 Ordinal > 0）⇒ nil = ordinal_unknown。
//
// 返回新指针而不是指向调用方局部/入参：UsageLog 会在 errlog 队列里活得比栈帧久。
func attemptOrdinalPtr(ordinal uint8) *int16 {
	if ordinal == 0 {
		return nil
	}
	return new(int16(ordinal))
}
