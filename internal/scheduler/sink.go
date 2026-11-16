// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
)

// LatchSink 是规则引擎的 typed health action 本地宿（根因重开）：
// Throttle 直写 RuntimeHealth；FailAccount 只做 TryAcquire + Hub.Dispatch，
// 不碰 sched view、不调 sched.FailAccount——内存摘除经 Hub 同步扇出到
// Scheduler.onRuleFailure（同协程，零异步窗口）。
//
// 额度补拉的**唯一**入队点也在这里（设计 §5.4 / R3 第 2 条）：只有 sink 同时
// 握有判决的四个健康键分量（key）、判决状态（state）与动作语义（th/ev），
// 「写入成功之后补一次」这个时序才可判。proxy 侧两处 429 判决点因此完全不
// 感知额度。
//
// 位置说明：语义上它属于 latch 中介，但 Go 禁止 import 环——latch 包不能
// 引用 *scheduler.RuntimeHealth（scheduler 反向持有 *latch.LatchStore）。
// 故宿体落本包，构造仍由 main 一次性完成（latch→hub→sink 序），无回填。
type LatchSink struct {
	Health *RuntimeHealth
	Latch  *latch.LatchStore
	Hub    *latch.Hub
	// Quota 429 额度补拉的投递面（nil = 未装配 ⇒ 不投递：测试/纯健康面形态
	// 与接线上线前逐位等价）。实参为 *QuotaQueue；接口而非具体类型：sink 只
	// 依赖"能投递一条 QuotaTask"，判决路径不感知队列/worker 实现。
	Quota quotaEnqueuer
}

// NewLatchSink 构造规则引擎本地宿（main 恰好调用一次；Health/Hub 为构造契约，
// 调用 FailAccount 前必须非 nil——装配序保证，见 cmd/server/main.go）。
// quota 传 *QuotaQueue（nil = 未装配 = 不补拉）。
func NewLatchSink(h *RuntimeHealth, l *latch.LatchStore, hub *latch.Hub, quota quotaEnqueuer) *LatchSink {
	return &LatchSink{Health: h, Latch: l, Hub: hub, Quota: quota}
}

var _ rule.HealthSink = (*LatchSink)(nil)

// Throttle typed 限流动作：account 作用全 RouteClass wildcard；account_route
// 作用单 RouteClass。health nil = 跳过（测试/纯选号无健康面）。
func (s *LatchSink) Throttle(ev rule.Event, th domain.ThrottleAction) error {
	var state HealthState
	var ttl time.Duration
	// resetUntil 非 nil ⇒ 这条动作有**绝对**截止（use_reset），走 ThrottleExtend；
	// 其余分支仍是相对时长，走 Throttle（内部换算成 now+ttl，用的是健康面自己的
	// 时钟 currentMs）。这里绝不能用 time.Now() 自己换算：健康面在测试里可注入
	// 时钟，两套时钟一混，绝对 until 就会落在 now 之前，冷却被夹成 1s。
	var resetUntil *time.Time
	switch th.Mode {
	case domain.ThrottleModeOpen:
		state = StateOPEN
		if th.DurationMs != nil {
			ttl = time.Duration(*th.DurationMs) * time.Millisecond
		} else {
			ttl = 30 * time.Second
		}
	case domain.ThrottleModeRetryAfter:
		state = StateRetryAfter
		if th.UseReset && ev.ResetAt != nil {
			resetUntil = ev.ResetAt
		} else if th.DurationMs != nil {
			ttl = time.Duration(*th.DurationMs) * time.Millisecond
		} else {
			ttl = 30 * time.Second
		}
	default:
		return nil
	}
	// 指纹缺失 fail-closed（与 FailAccount 同纪律）：健康判决按 (账号, 质量类,
	// 身份指纹, K) 落键，写不出具体指纹就等于写一条永远不会被查询到的记录——
	// 静默失效比报错更危险。生产事件由 scheduler 的事件构造器恒带指纹。
	if ev.CandidateFingerprint == "" {
		return ErrMissingCandidateFingerprint
	}
	key := HealthKey{AccountID: ev.AccountID, Identity: ev.CandidateFingerprint, IdentityRevision: ev.ExpectedIdentityRevision}
	if th.Scope == domain.ThrottleScopeAccount {
		key.Quality = "*"
	} else if th.Scope == domain.ThrottleScopeAccountRoute {
		if ev.RouteClassID == "" || ev.QualityClassID == "" {
			return nil
		}
		key.Quality = ev.QualityClassID
	} else {
		return nil
	}
	if s.Health != nil {
		var err error
		if resetUntil != nil {
			_, err = s.Health.ThrottleExtend(context.Background(), key, state, *resetUntil)
		} else {
			_, err = s.Health.Throttle(context.Background(), key, state, ttl)
		}
		if err != nil {
			return err
		}
		// 唯一入队点，且必须在**写入成功之后**（写失败 ⇒ 没有可补全的判决）。
		// 只补"该有绝对截止却没带"的那一类：use_reset 语义明确要求按窗口
		// reset 冷却（ev.ResetAt == nil ⇒ 当前只能落相对 TTL），异步补拉回来
		// 才有机会把它延长到 reset。其余三种形态都不入队——ResetAt 已带（无需
		// 补）、Mode=Open（判决与额度窗口无关）、未启用 use_reset（时长由规则
		// 声明，不由窗口决定）。
		if s.Quota != nil && th.Mode == domain.ThrottleModeRetryAfter && th.UseReset && ev.ResetAt == nil {
			s.Quota.Enqueue(QuotaTask{
				AccountID:        key.AccountID,
				IdentityRevision: key.IdentityRevision,
				Fingerprint:      key.Identity,
				Quality:          key.Quality,
				State:            state,
			})
		}
	}
	return nil
}

// FailAccount typed 失效动作：revision/指纹缺失 fail-closed（先于 persist 的
// 第一道门——引擎内 sink 调用先于 enqueuePersist）；锁存后经 Hub 同步扇出，
// fence（revision/指纹双检查）与内存摘除在 Scheduler.onRuleFailure 内执行。
func (s *LatchSink) FailAccount(ev rule.Event) error {
	if ev.ExpectedIdentityRevision <= 0 {
		return ErrMissingExpectedRevision
	}
	fp := ev.CandidateFingerprint
	if fp == "" {
		return ErrMissingCandidateFingerprint
	}
	if s.Latch != nil {
		s.Latch.TryAcquire(ev.AccountID, fp, ev.ExpectedIdentityRevision)
	}
	s.Hub.Dispatch(ev)
	return nil
}

// onRuleFailure 是 Hub 订阅回调（New 内订阅，首个 MarkResult 前完成）：fence
// fail-closed（revision/指纹不对即丢，不摘除）+ 快照内存摘除。persist func 内的
// 第二重 fence（NewRulePersistFunc）保持——双 fail-closed，比旧单点更严。
func (s *Scheduler) onRuleFailure(ev rule.Event) {
	if v := s.View(); v != nil {
		if as, ok := v.Account(ev.AccountID); ok {
			av := as.static.Load()
			if av.acc.IdentityRevision != ev.ExpectedIdentityRevision {
				return
			}
			current, err := candidateFingerprint(&av.acc)
			if err != nil {
				return
			}
			if ev.CandidateFingerprint == "" || ev.CandidateFingerprint != current {
				return
			}
		}
	}
	s.FailAccount(ev.AccountID)
}
