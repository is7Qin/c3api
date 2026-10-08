// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestUpdateKeyGroupSwitchEligibility key 改组可选性五态 + 写前预取失败零更新：
// public/已授予 private → 200 落新组、刷新鉴权快照（GroupID + ProtocolConverts）
// 并发布 publish(Keys:true)（A4）；未授予 private → ErrGroupNotEligible(400)；
// 缺失组 → 404；软删组 → 404；group_id<=0 → ErrInvalidInput(400)。预取失败时
// key 组不变（零更新）。
func TestUpdateKeyGroupSwitchEligibility(t *testing.T) {
	// 同时挂 keys 增量注册 sink（快照断言）与 pubRecorder（发布点断言 A4）。
	fs := newFakeStore()
	keys := &fakeKeyRegistrar{}
	pr := &pubRecorder{}
	svc := &Service{store: fs, inv: &invRecorder{}, keys: keys, pub: pr, log: nil}
	ctx := context.Background()

	user, err := fs.CreateUser(ctx, &domain.User{Email: "switch@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	pub, err := fs.CreateGroup(ctx, &domain.Group{Name: "pub-a", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	pub2, err := fs.CreateGroup(ctx, &domain.Group{
		Name: "pub-b", Visibility: domain.GroupVisibilityPublic,
		ProtocolConverts: []domain.ProtocolConvert{domain.ProtocolConvertChatToResp},
	})
	require.NoError(t, err)
	priv, err := fs.CreateGroup(ctx, &domain.Group{Name: "priv-a", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	k, err := svc.CreateKey(ctx, user.ID, "k", pub.ID, 0, 0)
	require.NoError(t, err)

	// public → 200 落新组 + 快照刷新（GroupID + ProtocolConverts 均为新组）+
	// publish(Keys:true)（同实例即时生效 + 跨实例 invalidate）
	target := pub2.ID
	before := pr.total()
	updated, err := svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &target)
	require.NoError(t, err)
	require.Equal(t, pub2.ID, updated.GroupID, "改组后落新组 id")
	require.Equal(t, pub2.Name, updated.GroupName, "写后显式回填新组名")
	last := keys.lastMeta()
	require.NotNil(t, last)
	require.Equal(t, pub2.ID, last.GroupID, "快照 GroupID = 新组")
	require.Equal(t, pub2.ProtocolConverts, last.ProtocolConverts, "快照转换方向 = 新组")
	require.Equal(t, before+1, pr.total(), "改组成功发布一条 NOTIFY")
	require.True(t, pr.last().Keys, "改组 → publish(Keys:true)（A4）")

	// 未授予 private → ErrGroupNotEligible（映射 400）；预取失败 → 更新零发生
	writes := fs.updateKeyCalls
	publishes := pr.total()
	target = priv.ID
	_, err = svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &target)
	require.ErrorIs(t, err, ErrGroupNotEligible)
	require.ErrorIs(t, err, ErrInvalidInput, "必须映射 400")
	got, err := fs.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, pub2.ID, got.GroupID, "预取失败 → key 组不变（零更新）")
	require.Equal(t, writes, fs.updateKeyCalls, "预取失败 → 未触达 repo UpdateKey（零写库）")
	require.Equal(t, publishes, pr.total(), "预取失败 → 零发布")

	// 授予 → 200 可切
	fs.assign[priv.ID] = []int64{user.ID}
	updated, err = svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &target)
	require.NoError(t, err)
	require.Equal(t, priv.ID, updated.GroupID, "已授予 private 可切")

	// 缺失组 → ErrNotFound（404）
	missing := int64(999999)
	_, err = svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &missing)
	require.ErrorIs(t, err, ErrNotFound)

	// 软删组 → ErrNotFound（404）
	gone, err := fs.CreateGroup(ctx, &domain.Group{Name: "gone", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	now := time.Now()
	fs.groups[gone.ID].DeletedAt = &now
	target = gone.ID
	_, err = svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &target)
	require.ErrorIs(t, err, ErrNotFound, "软删组不可作目标")

	// group_id<=0 → ErrInvalidInput（400）
	for _, bad := range []int64{0, -1} {
		b := bad
		_, err = svc.UpdateKey(ctx, user.ID, k.ID, nil, nil, nil, nil, &b)
		require.ErrorIs(t, err, ErrInvalidInput, "group_id<=0 → 400")
	}
}

// TestUpdateKeyNoChangeShortCircuit 无变更短路（先于任何组读取）：group_id 缺省或
// 显式等于当前组且其余字段全 nil → 零读组/零写/零发布，恒 200——即便当前组已
// 软删（若触 getGroupLive 会 404，故成功即证明无组读取）。
func TestUpdateKeyNoChangeShortCircuit(t *testing.T) {
	svc, fs, pr := newPubSvc()
	ctx := context.Background()

	u := seedUser(t, fs, "noop@example.com", 0, 0)
	g, err := svc.CreateGroup(ctx, "noop-g", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	created, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)

	// 软删当前组：no-op PUT 不得触组读取（否则 404），须恒 200 且零写库零发布
	now := time.Now()
	fs.groups[g.ID].DeletedAt = &now
	before := pr.total()
	writes := fs.updateKeyCalls

	got, err := svc.UpdateKey(ctx, u.ID, created.ID, nil, nil, nil, nil, nil)
	require.NoError(t, err, "当前组软删下 no-op PUT 仍 200（零组读取）")
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, g.ID, got.GroupID)
	require.Equal(t, before, pr.total(), "无变更短路 → 零发布")
	require.Equal(t, writes, fs.updateKeyCalls, "无变更短路 → 未触达 repo UpdateKey（零写库）")

	// 显式 group_id 等于当前组（其余全 nil）→ 同组短路，零读组/零写/零发布
	same := g.ID
	_, err = svc.UpdateKey(ctx, u.ID, created.ID, nil, nil, nil, nil, &same)
	require.NoError(t, err, "显式等于当前组且其余全 nil → 短路恒 200（即便当前组软删）")
	require.Equal(t, before, pr.total(), "同组短路 → 零发布")
	require.Equal(t, writes, fs.updateKeyCalls, "同组短路 → 零写库")

	// 反例：真实字段变更（name）→ 非短路路径确实读组；当前组软删 → getGroupLive
	// 404（证明"无变更短路"之所以恒 200 正是因为它先于组读取）。
	name := "renamed"
	_, err = svc.UpdateKey(ctx, u.ID, created.ID, &name, nil, nil, nil, nil)
	require.ErrorIs(t, err, ErrNotFound, "其它字段变更触发组预取，当前组软删 → 404")
	require.Equal(t, before, pr.total(), "预取失败未发布")
	require.Equal(t, writes, fs.updateKeyCalls, "预取失败未写库")
}
