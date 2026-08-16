// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GET /admin/users-top 实时在途并发排行（spec 2026-08-14）：读门禁快照在途
// 计数（Auth.InFlightUsers 只读访问器——零锁零热路径）→ 过滤 0 → 降序 →
// TopN（缺省 20，上限 100）+ other 归并（其余在途用户合计，非伪用户条目）；
// email = TopN user_id 一次 IN 查询回填（users 表无 name 列——仅 email）。
// 内部 TTL 2s 缓存（top 并入缓存键）；多实例部署下为**本实例**在途并发。
func (h *AdminAPI) GetAdminUsersTop(w http.ResponseWriter, r *http.Request, params GetAdminUsersTopParams) {
	top := deref(params.Top)
	if top < 1 {
		top = 20
	}
	if top > 100 {
		top = 100
	}
	key := fmt.Sprintf("ut:%d", top)
	if v, ok := h.usersTopCache.get(key); ok {
		httpface.WriteJSON(w, http.StatusOK, v)
		return
	}
	var snap map[int64]int64
	if h.ops.InFlightUsers != nil {
		snap = h.ops.InFlightUsers()
	}
	// 过滤 0 在途 + 降序（同并发按 user_id 升序兜底确定性）。
	type pair struct {
		uid int64
		c   int64
	}
	users := make([]pair, 0, len(snap))
	for uid, c := range snap {
		if c > 0 {
			users = append(users, pair{uid: uid, c: c})
		}
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].c != users[j].c {
			return users[i].c > users[j].c
		}
		return users[i].uid < users[j].uid
	})
	n := min(top, len(users))
	var other int64
	for _, u := range users[n:] {
		other += u.c
	}
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = users[i].uid
	}
	emails, err := h.svc.UserEmails(r.Context(), ids)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]UsersTopEntry, 0, n)
	for _, u := range users[:n] {
		out = append(out, UsersTopEntry{UserId: u.uid, Email: emails[u.uid], Concurrency: u.c})
	}
	resp := UsersTopResponse{Users: out, OtherConcurrency: other}
	h.usersTopCache.set(key, resp)
	httpface.WriteJSON(w, http.StatusOK, resp)
}
