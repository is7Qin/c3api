// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// Select 选号并占用并发槽。已编译路由执行预编译 DecisionView 计划
// （NewAttemptPlan + ReserveAttempt：lane 顺序、完整唯一 overflow 尾、
// reservation reject 不耗 attempt）；未编译路由（编译车道尚未装配的中间态）
// 走 legacy 预生成序列扫描——Task27 cutover 物理删除，不构成兼容承诺。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{ApplyModelMapping: true}, RouteRefFor(groupID, string(format), model))
	if err == nil {
		sel, _, rerr := s.ReserveAttempt(plan)
		if rerr != nil {
			return nil, rerr
		}
		return sel, nil
	}
	if errors.Is(err, ErrGroupNotFound) {
		return nil, err
	}
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return nil, ErrGroupNotFound
	}
	groups := v.Groups()
	gs, ok := groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	rt, ok := gs.routes[routeKey{format, model}]
	if !ok {
		rt, ok = gs.routes[routeKey{format, ""}]
	}
	if !ok {
		return nil, ErrFormatUnavailable
	}
	now := s.timeNow()
	if rt.tier1 != nil {
		if sel, ok := s.pickFrom(rt.tier1, format, model, now); ok {
			return sel, nil
		}
	}
	if rt.tier2 != nil {
		if sel, ok := s.pickFrom(rt.tier2, format, model, now); ok {
			return sel, nil
		}
	}
	return nil, ErrNoAvailable
}

// SelectOpaque selects from a compiled route without applying model mapping.
func (s *Scheduler) SelectOpaque(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{ApplyModelMapping: false}, RouteRefFor(groupID, string(format), model))
	if err == nil {
		sel, _, reserveErr := s.ReserveAttempt(plan)
		return sel, reserveErr
	}
	if errors.Is(err, ErrGroupNotFound) {
		return nil, err
	}
	sel, err := s.Select(groupID, format, model)
	if err != nil {
		return nil, err
	}
	sel.Model = model
	sel.ModelMappingMode = domain.ModelMappingModeInvalid
	return sel, nil
}

func (s *Selection) LogMappedModel(reqModel string) string {
	switch s.ModelMappingMode {
	case domain.ModelMappingModeExplicit:
		if s.Model != reqModel {
			return s.Model
		}
	case domain.ModelMappingModeImplicit:
		return reqModel
	}
	return ""
}

func (s *Selection) ClientResponseModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return ""
}

// pickFrom 沿预生成序列扫描候选：游标取模 + 动态状态检查 + CAS 抢占。
// 扫描上限 = 序列一轮（每候选检查一次）；全不可用/全竞争失败返回 false。
func (s *Scheduler) pickFrom(ws *weightedSeq, format domain.RequestFormat, model string, now time.Time) (*Selection, bool) {
	n := len(ws.seq)
	if n == 0 {
		return nil, false
	}
	cn := s.instancesN()
	view := s.concView.Load()
	for i := 0; i < n; i++ {
		a := ws.seq[int(ws.cursor.Add(1))%n]
		av := a.static.Load()
		fp, err := candidateFingerprint(&av.acc)
		if err == nil && s.latch != nil && s.latch.IsLatched(av.acc.ID, fp) {
			continue
		}
		if s.health != nil {
			rev := av.acc.LifecycleRevision
			if s.health.EffectiveState(av.acc.ID, "*", rev) != StateReady {
				continue
			}
		}
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue
		}
		cur := a.runtime.concurrency.Load()
		limit := int64(av.acc.MaxConcurrency) // buildSnapshots 已归一化 ≤0→defaultMax，恒 >0
		if cur >= int64(concShare(int(limit), cn)) {
			if cur >= limit || !concAllows(view, av.acc.ID, limit, cur+1) {
				continue
			}
		}
		if a.runtime.concurrency.CompareAndSwap(cur, cur+1) {
			mapped := model
			var entry domain.ModelMappingEntry
			if e, ok := av.tpl.ModelMapping[model]; ok {
				mapped = e.MappedModel
				entry = e
			}
			used := s.timeNow()
			for {
				curSt := a.runtime.state.Load()
				if curSt == nil {
					break
				}
				next := *curSt
				next.lastUsedAt = &used
				if a.runtime.state.CompareAndSwap(curSt, &next) {
					break
				}
			}
			baseURL := av.tpl.BaseURL
			if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
				baseURL = *av.acc.BaseURL
			}
			return &Selection{
				AccountID: av.acc.ID, TemplateID: av.tpl.ID,
				BaseURL: baseURL, Format: format,
				UpstreamKey: av.acc.UpstreamKey, CredentialType: av.tpl.CredentialType, Model: mapped,
				StripImageTools:      av.tpl.StripImageTools,
				Ext:                  av.acc.Ext,
				CandidateFingerprint: fp,
				lease:                &leaseToken{acc: a},
				ModelMappingMode:     entry.Mode,
			}, true
		}
	}
	return nil, false
}
