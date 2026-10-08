// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// TestUserKeySwitchGroup PUT /api/user/keys/{id} 携带 group_id：200 且响应
// GroupID/GroupName 均为新组；未授予 private → 400；缺失组 → 404；group_id<=0
// → 400；no-op PUT（空 body）→ 200 回归。
func TestUserKeySwitchGroup(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)

	mkGroup := func(name, visibility string) int64 {
		body := `{"name":"` + name + `"}`
		if visibility != "" {
			body = `{"name":"` + name + `","visibility":"` + visibility + `"}`
		}
		rec := doAdmin(http.MethodPost, "/api/admin/groups", body, "")
		require.Equal(t, http.StatusOK, rec.Code, "create group %s: %s", name, rec.Body.String())
		var g Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
		return *g.ID
	}
	g1 := mkGroup("switch-g1", "")
	g2 := mkGroup("switch-g2", "")
	priv := mkGroup("switch-priv", "private")

	token, _ := registerAndGet(t, doUser, "switcher@example.com")

	// 在 g1 建 key（响应回填组名）
	rec := doUser(http.MethodPost, "/api/user/keys", `{"name":"k","group_id":`+itoa(g1)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "create key: %s", rec.Body.String())
	var created userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, g1, *created.GroupID)
	require.Equal(t, "switch-g1", *created.GroupName, "创建响应回填组名")

	// 切到 g2 → 200，GroupID/GroupName 均为新组
	rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{"group_id":`+itoa(g2)+`}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "switch group: %s", rec.Body.String())
	var switched userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &switched))
	require.Equal(t, g2, *switched.GroupID, "响应 GroupID = 新组")
	require.Equal(t, "switch-g2", *switched.GroupName, "响应 GroupName = 新组")

	// 未授予 private → 400（ErrGroupNotEligible）
	rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{"group_id":`+itoa(priv)+`}`, token)
	require.Equal(t, http.StatusBadRequest, rec.Code, "private not granted: %s", rec.Body.String())

	// 不存在组 → 404
	rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{"group_id":999999}`, token)
	require.Equal(t, http.StatusNotFound, rec.Code, "missing group: %s", rec.Body.String())

	// group_id<=0 → 400
	for _, bad := range []string{"0", "-1"} {
		rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{"group_id":`+bad+`}`, token)
		require.Equal(t, http.StatusBadRequest, rec.Code, "group_id=%s: %s", bad, rec.Body.String())
	}

	// no-op PUT（空 body，全 nil）→ 200 回归，保持当前组
	rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "no-op PUT: %s", rec.Body.String())
	var noop userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &noop))
	require.Equal(t, g2, *noop.GroupID, "no-op 保持当前组")

	// A7：当前组软删后，no-op PUT（缺省 group_id、其余全 nil）仍 200——若短路
	// 触达组读取（getGroupLive）会 404；成功即证明零组读取。GroupID 保持。
	now := time.Now()
	store.groups[g2].DeletedAt = &now
	rec = doUser(http.MethodPut, "/api/user/keys/"+itoa(*created.ID), `{}`, token)
	require.Equal(t, http.StatusOK, rec.Code, "当前组软删 no-op PUT: %s", rec.Body.String())
	var noopGone userapi.Key
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &noopGone))
	require.Equal(t, g2, *noopGone.GroupID, "当前组软删 no-op PUT 保持当前组")
}
