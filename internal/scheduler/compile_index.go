// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Stage-built route index (v5 §4/C2): maps events to affected routes so scoped
// fires name exactly the routes a full recompile would visit. Owner: compile
// lane. Lifecycle: born at stage (attached to the staged root), dies with the
// superseded root, read-only between.

// compileRouteIndex maps events to affected routes: groups → routes (§4
// group→routes index), quality-hex → route (lane-local quality diffs),
// model → routes (lane-local price diffs). setup/steady-state boundary: the
// index is built at STAGE/setup; steady-state fires only borrow, never
// rebuild.
type compileRouteIndex struct {
	groupRoutes map[int64][]RouteRef
	rcRoutes    map[string]RouteRef
	modelRoutes map[string][]RouteRef
	totalRoutes int
}

// buildRouteIndex enumerates groupSnapshot.routes with the exact bridge the
// full compiler uses (routeKey→RouteRef via op mapping incl. images fan-out +
// normRouteRef zeroing), so scoped resolution names exactly the routes a full
// recompile would visit.
func buildRouteIndex(groups map[int64]*groupSnapshot) *compileRouteIndex {
	idx := &compileRouteIndex{
		groupRoutes: make(map[int64][]RouteRef),
		rcRoutes:    make(map[string]RouteRef),
		modelRoutes: make(map[string][]RouteRef),
	}
	for gid, gs := range groups {
		if gs == nil {
			continue
		}
		for rk := range gs.routes {
			for _, op := range operationTagsForFormat(rk.format) {
				rr := canonicalRouteRefWithOp(gid, rk, op)
				key := normRouteRef(rr)
				idx.groupRoutes[gid] = append(idx.groupRoutes[gid], key)
				if rr.RouteClassID != "" {
					idx.rcRoutes[rr.RouteClassID] = key
				}
				idx.modelRoutes[rk.model] = append(idx.modelRoutes[rk.model], key)
				idx.totalRoutes++
			}
		}
	}
	return idx
}

// newStaticView builds a staged root with its owned facts map AND route index
// in one place (copy-modify-Store discipline: the maps are born here, never
// mutated after staging).
func newStaticView(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) *StaticView {
	return &StaticView{groups: groups, byID: byID, facts: attachCompilerFacts(byID), routeIndex: buildRouteIndex(groups)}
}
