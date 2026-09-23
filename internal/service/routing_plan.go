// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 当前发布计划的**分页投影**：路由列表支持搜索 + 切片，每个返回路由的候选
// 目录各自切片。计划本身在内存里（scheduler 投影），不查库——分页只是切片。
//
// 与 RoutingPlanExplanation 的分工：后者返回**完整**计划（resolveRoutingRoute
// 找路由要用全量），本方法只服务展示面。

import (
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// RoutingPlanCandidatesDefault 契约默认候选页大小（candidates_limit 缺省时由
// handler 填入；显式 0 语义是「不返回候选」，两者必须区分）。
const RoutingPlanCandidatesDefault = routingPlanCandDefault

// RoutingPlanQuery 计划展示面入参。
//
// Route 给定则只返回该条路由（先于 Search/分页生效）。Search/Offset/Limit 作用
// 于路由列表；CandidatesOffset/CandidatesLimit 作用于每个返回路由的候选目录。
type RoutingPlanQuery struct {
	Search           string
	Route            string
	Offset           int
	Limit            int
	CandidatesOffset int
	// CandidatesLimit 0 = 不返回候选（路由选择器取轻量列表）；调用方须在参数
	// 缺省时填入 RoutingPlanCandidatesDefault。
	CandidatesLimit int
}

// RoutingPlanRouteView 一条路由 + 其候选总数（切片后仍可算出分页）。
type RoutingPlanRouteView struct {
	Route           scheduler.RoutingPlanRoute
	CandidatesTotal int64
}

// RoutingPlanResult 计划展示面结果。TotalRoutes 为过滤后总数（不受分页影响）。
type RoutingPlanResult struct {
	Generation  uint64
	Routes      []RoutingPlanRouteView
	TotalRoutes int64
}

// QueryRoutingPlan 当前发布计划的分页投影。
func (s *Service) QueryRoutingPlan(q RoutingPlanQuery) (*RoutingPlanResult, error) {
	plan, err := s.RoutingPlanExplanation()
	if err != nil {
		return nil, err
	}
	// CurrentRoutingPlan 每次返回防御性深拷贝，切片/改写候选安全。
	routes := plan.Routes

	if q.Route != "" {
		if _, err := domain.HexToID(q.Route); err != nil {
			return nil, ErrInvalidInput
		}
		found := -1
		for i := range routes {
			if routes[i].Ref.RouteClassID == q.Route {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, ErrNotFound
		}
		routes = routes[found : found+1]
	} else if term := strings.ToLower(strings.TrimSpace(q.Search)); term != "" {
		filtered := make([]scheduler.RoutingPlanRoute, 0, len(routes))
		for _, r := range routes {
			if routingRouteMatches(r, term) {
				filtered = append(filtered, r)
			}
		}
		routes = filtered
	}

	total := int64(len(routes))
	lo, hi := normalizePage(q.Offset, q.Limit, routingPlanRouteDefault, routingPlanRouteMax, len(routes))
	routes = routes[lo:hi]

	candLimit := q.CandidatesLimit
	if candLimit < 0 {
		candLimit = routingPlanCandDefault
	}
	if candLimit > routingPlanCandMax {
		candLimit = routingPlanCandMax
	}

	out := &RoutingPlanResult{
		Generation:  plan.Generation,
		Routes:      make([]RoutingPlanRouteView, 0, len(routes)),
		TotalRoutes: total,
	}
	for i := range routes {
		cands := routes[i].Candidates
		candTotal := int64(len(cands))
		page := []scheduler.RoutingPlanCandidate{}
		if candLimit > 0 {
			clo := q.CandidatesOffset
			if clo < 0 {
				clo = 0
			}
			if clo > len(cands) {
				clo = len(cands)
			}
			chi := clo + candLimit
			if chi > len(cands) {
				chi = len(cands)
			}
			page = cands[clo:chi]
		}
		view := RoutingPlanRouteView{CandidatesTotal: candTotal}
		view.Route = routes[i]
		view.Route.Candidates = page
		out.Routes = append(out.Routes, view)
	}
	return out, nil
}

// routingRouteMatches 路由标签模糊匹配：model / format / operation_tag /
// group_id（十进制）任一含该词即命中。term 必须已小写化。
func routingRouteMatches(r scheduler.RoutingPlanRoute, term string) bool {
	if strings.Contains(strings.ToLower(r.Ref.Model), term) ||
		strings.Contains(strings.ToLower(r.Ref.Format), term) ||
		strings.Contains(strings.ToLower(r.Ref.OperationTag), term) {
		return true
	}
	return strings.Contains(strconv.FormatInt(r.Ref.GroupID, 10), term)
}
