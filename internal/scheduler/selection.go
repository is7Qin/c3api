// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// Select 按预生成调度路径（格式硬过滤 + 模型硬白名单 + 全模型账号 tier2 兜底
// + 加权轮询序列）选号，并占用并发槽。
// 路径在快照重建时生成（buildRoutes），本函数热路径只做 O(1) 桶查找 + 序列游标取用
// + 动态状态检查（冷却/禁用/并发满，atomic 读）+ CAS 抢占。
// 调用方完成请求后必须 Release + MarkResult。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	return s.selectInternal(groupID, format, model, true)
}

// SelectOpaque Search 透明选号（复用 Responses 路由桶，跳过 ModelMapping）。
// 复用同一 route 查找（零第二 map/DB/lock/分配），仅 pickFrom 跳过映射——
// Search 保持客户端 model/bytes 上游、opaque 下游、固定 codex-search 计费。
func (s *Scheduler) SelectOpaque(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	return s.selectInternal(groupID, format, model, false)
}

func (s *Scheduler) selectInternal(groupID int64, format domain.RequestFormat, model string, applyMapping bool) (*Selection, error) {
	groups, ok := s.store.groups.Load().(map[int64]*groupSnapshot)
	if !ok {
		return nil, ErrGroupNotFound
	}
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
		if sel, ok := s.pickFrom(rt.tier1, format, model, now, applyMapping); ok {
			return sel, nil
		}
	}
	if rt.tier2 != nil {
		if sel, ok := s.pickFrom(rt.tier2, format, model, now, applyMapping); ok {
			return sel, nil
		}
	}
	return nil, ErrNoAvailable
}

// LogMappedModel returns the optional mapped_model audit value. Implicit
// mappings intentionally look unmapped in usage logs.
func (s *Selection) LogMappedModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeExplicit && s.Model != reqModel {
		return s.Model
	}
	return ""
}

// PriceModel returns the model whose pricing applies to this selection.
func (s *Selection) PriceModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return s.Model
}

func (s *Selection) ClientResponseModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return ""
}

// pickFrom 沿预生成序列扫描候选：游标取模 + 动态状态检查 + CAS 抢占。
// 扫描上限 = 序列一轮（每候选检查一次）；全不可用/全竞争失败返回 false。
func (s *Scheduler) pickFrom(ws *weightedSeq, format domain.RequestFormat, model string, now time.Time, applyMapping bool) (*Selection, bool) {
	n := len(ws.seq)
	if n == 0 {
		return nil, false
	}
	cn := s.instancesN()
	view := s.concView.Load()
	for i := 0; i < n; i++ {
		a := ws.seq[int(ws.cursor.Add(1))%n]
		av := a.static.Load()
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue
		}
		cur := a.concurrency.Load()
		limit := int64(av.acc.MaxConcurrency)
		if cur >= int64(concShare(int(limit), cn)) {
			if cur >= limit || !concAllows(view, av.acc.ID, limit, cur+1) {
				continue
			}
		}
		if a.concurrency.CompareAndSwap(cur, cur+1) {
			mapped := model
			var entry domain.ModelMappingEntry
			if applyMapping {
				if e, ok := av.tpl.ModelMapping[model]; ok {
					mapped = e.MappedModel
					entry = e
				}
			}
			used := s.timeNow()
			st2 := *st
			st2.lastUsedAt = &used
			a.state.Store(&st2)
			baseURL := av.tpl.BaseURL
			if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
				baseURL = *av.acc.BaseURL
			}
			return &Selection{
				AccountID: av.acc.ID, TemplateID: av.tpl.ID,
				BaseURL: baseURL, Format: format,
				UpstreamKey: av.acc.UpstreamKey, CredentialType: av.tpl.CredentialType, Model: mapped,
				StripImageTools:  av.tpl.StripImageTools,
				ModelMappingMode: entry.Mode,
				Ext:              av.acc.Ext,
			}, true
		}
	}
	return nil, false
}
