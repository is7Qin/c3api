// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestStaticKeyHasNoPointerSliceOrMapFields 是 spec §5.5b「结构保证」的守卫
// （A2①）：staticKey 的每个字段必须是**值类型**。
//
// 为什么这是承重断言而不是风格洁癖：含指针的 struct 依然「可比较」，`==` 也
// 依然合法，但它对指针只比**地址**。键是每次重载重建的——buildSnapshots 每轮
// 都 new 一个 snapshotStatic，其 acc.Ext 与 tpl 指向本轮新建对象。只要键里混进
// 一个这样的指针，键就在两次「事实完全相同」的重载之间**恒不等**，复用分支
// 永不命中，键退化为惰性装饰——正是本 spec 要消灭的那个缺陷（scheduler.go:431
// 的旧手写链即因 oldAv.tpl == av.tpl / oldAv.acc.Ext == av.Ext 两处指针比较而
// 恒假）。Slice/Map 更直接：它们根本不可比较，一旦进键 struct 就失去 `==`。
//
// 因此：[32]byte（reflect.Array，值语义）是唯一允许的复合事实载体。
func TestStaticKeyHasNoPointerSliceOrMapFields(t *testing.T) {
	kt := reflect.TypeOf(staticKey{})
	require.Greater(t, kt.NumField(), 0, "staticKey must not be empty")

	var names []string
	for i := 0; i < kt.NumField(); i++ {
		fl := kt.Field(i)
		names = append(names, fl.Name)
		switch fl.Type.Kind() {
		case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint8,
			reflect.String, reflect.Bool, reflect.Array:
			// 值语义，可比较。
		case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
			t.Fatalf("staticKey.%s has non-value kind %s: the key must stay comparable by == over VALUES; "+
				"a pointer here compares addresses (rebuilt every reload => key never equal), and slice/map/func "+
				"are not comparable at all, which would make the struct uncompilable or the reuse branch inert", fl.Name, fl.Type.Kind())
		default:
			t.Fatalf("staticKey.%s has unclassified kind %s; classify it explicitly (value semantics required)", fl.Name, fl.Type.Kind())
		}
	}
	require.Equal(t, []string{
		"accountID", "name", "templateID", "baseURL", "upstreamKey",
		"maxConcurrency", "enabled", "cacheDomain", "upstreamCostMultiplierBp",
		"identityRevision",
		"codexAccountID", "codexInstallation", "codexSession", "codexThread", "codexWindow",
		"codexEmail", "codexPATKey", "codexOAuthToken", "codexOAuthRefresh", "codexOAuthExpires",
		"credentialType", "stripImageTools", "modelsDigest", "formatModelsKey",
		"supportedFormats", "modelMappingKey",
		"groupIDsDigest",
	}, names, "staticKey field set changed: this assertion is the review tripwire for the §5.5b field list; "+
		"update it deliberately together with spec §5.5/§5.5b, not incidentally")
}

// TestStaticKeyCoversEveryDeclaredAccountField 是字段声明表与静态键之间的机械
// 断言：声明表（domain.AccountFieldSpecs，唯一事实来源）的每一行都必须在静态键
// 里有承载字段。声明表就是"账号侧静态快照消费 = 是"的清单；漏一行即意味着该字段
// 改了而键仍判等 ⇒ 叶子被复用 ⇒ 叶子上的消费点读到陈旧值。
//
// 键侧另有 identity_revision（K）：它是生成只读字段、不在可写声明表内，故单独
// 断言，不由下面的映射覆盖。
func TestStaticKeyCoversEveryDeclaredAccountField(t *testing.T) {
	// 声明字段 → 静态键里承载它的字段名（值语义载体；集合类以规范序摘要承载）。
	carrier := map[domain.AccountField]string{
		domain.FieldName:                   "name",
		domain.FieldTemplateID:             "templateID",
		domain.FieldBaseURL:                "baseURL",
		domain.FieldUpstreamKey:            "upstreamKey",
		domain.FieldMaxConcurrency:         "maxConcurrency",
		domain.FieldGroupIDs:               "groupIDsDigest",
		domain.FieldEnabled:                "enabled",
		domain.FieldCacheDomain:            "cacheDomain",
		domain.FieldUpstreamCostMultiplier: "upstreamCostMultiplierBp",
	}
	kt := reflect.TypeOf(staticKey{})
	keyFields := make(map[string]bool, kt.NumField())
	for i := 0; i < kt.NumField(); i++ {
		keyFields[kt.Field(i).Name] = true
	}

	declared := domain.AccountFieldSpecs()
	require.Len(t, carrier, len(declared), "every declared field needs a key carrier")
	for _, spec := range declared {
		name, ok := carrier[spec.Field]
		require.True(t, ok, "declared field %q has no staticKey carrier", spec.Name)
		require.True(t, keyFields[name], "declared field %q maps to missing staticKey.%s", spec.Name, name)
	}
	require.True(t, keyFields["identityRevision"], "K must be in the static key: a reused leaf carrying a stale K would make in-flight fences compare against an outdated generation")
}

// TestStaticKeyIsComparable 用一次真实 `==` 证明该类型确实可用作比较算子
// （编译期即可比较；这里同时证明零值键的自反性）。
func TestStaticKeyIsComparable(t *testing.T) {
	var a, b staticKey
	require.True(t, a == b, "two zero keys must be equal")
	b.accountID = 1
	require.False(t, a == b, "differing accountID must make keys unequal")
}

// TestStaticKeyGuardDetectsPointerField 是守卫的**反向证明**（required demo i）：
// 把指针加进一个镜像 struct，守卫必须 FAIL。这条测试存在的意义是证明上面那个
// reflect 守卫真的能抓住缺陷，而不是恒真通过。
func TestStaticKeyGuardDetectsPointerField(t *testing.T) {
	// 与 staticKey 同构，但把一个值字段替换成指针——即守卫要拦的形态。
	type poisoned struct {
		accountID int64
		tpl       *domain.Template // ← 每次重载新建 => 地址恒不等
	}
	pt := reflect.TypeOf(poisoned{})
	var found bool
	for i := 0; i < pt.NumField(); i++ {
		if pt.Field(i).Type.Kind() == reflect.Ptr {
			found = true
		}
	}
	require.True(t, found, "the poisoned mirror must actually contain a pointer field, else this reverse-proof is vacuous")

	// 真正跑守卫逻辑（与 TestStaticKeyHasNoPointerSliceOrMapFields 同一判定）：
	// 若守卫对 poisoned 通过，说明守卫无效。
	guardPasses := func(tp reflect.Type) bool {
		for i := 0; i < tp.NumField(); i++ {
			switch tp.Field(i).Type.Kind() {
			case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint8,
				reflect.String, reflect.Bool, reflect.Array:
			case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
				return false
			default:
				return false
			}
		}
		return true
	}
	require.False(t, guardPasses(pt), "guard must REJECT a struct carrying a pointer field")
	require.True(t, guardPasses(reflect.TypeOf(staticKey{})), "guard must ACCEPT the real staticKey")
}

// TestStaticKeyEqualAcrossDistinctSnapshotsWithIdenticalFacts 是 required demo ii：
// 两个**不同对象**的快照（各自新分配的 acc.Ext / tpl），事实完全相同时键必须
// 相等。这正是旧手写布尔链做不到的事——它因指针比较而恒假。
//
// 同时也锁住「切片/映射事实按规范序比较」：groupIDs/Models 的顺序不同但集合
// 相同 ⇒ 键相等（规范序摘要）。
func TestStaticKeyEqualAcrossDistinctSnapshotsWithIdenticalFacts(t *testing.T) {
	build := func() *snapshotStatic {
		// 每次调用都分配全新对象：tpl 与 Ext 都是新指针，模拟 buildSnapshots
		// 每轮重载的行为。
		tp := &domain.Template{
			BaseURL:          "https://tpl.example/v1",
			CredentialType:   "api_key",
			StripImageTools:  true,
			Models:           []string{"m-b", "m-a"},
			SupportedFormats: []domain.RequestFormat{"anthropic", "openai_chat"},
			FormatModels:     map[domain.RequestFormat][]string{"openai_chat": {"m-b", "m-a"}},
			ModelMapping: domain.ModelMapping{
				"alias": {MappedModel: "up", Mode: domain.ModelMappingModeExplicit},
			},
		}
		pat := "pat-x"
		a := domain.Account{
			ID: 7, Name: "acct-7", TemplateID: 3,
			UpstreamKey: "sk", MaxConcurrency: 4, Enabled: true,
			CacheDomain: strPtr("cache.example"), UpstreamCostMultiplierBp: 12000,
			IdentityRevision: 5,
			Template:         tp,
			Ext: &domain.AccountExt{
				CodexAccountID: strPtr("acct-up"),
				CodexPATKey:    &pat, // 凭据明文：**不进键**（§5.5b(2)：digest 已由 upstreamKey|patKey 覆盖，且 token 刷新不该改身份）
				CodexIdentity: &domain.CodexIdentity{
					InstallationID: "inst", SessionID: "sess", ThreadID: "thr", WindowID: "win",
				},
			},
		}
		return &snapshotStatic{acc: a, tpl: tp, groupIDs: []int64{3, 9}}
	}

	oldAv, newAv := build(), build()

	// 前提：这确实是「不同对象」——否则本证明是空洞的。
	require.NotSame(t, oldAv, newAv)
	require.NotSame(t, oldAv.tpl, newAv.tpl, "tpl must be a distinct object per reload (this is why pointer comparison failed)")
	require.NotSame(t, oldAv.acc.Ext, newAv.acc.Ext, "Ext must be a distinct object per reload (this is why pointer comparison failed)")

	require.True(t, staticKeyOf(oldAv) == staticKeyOf(newAv),
		"identical facts in distinct objects must yield equal keys — otherwise the reuse branch is inert again")

	// 顺序无关：groupIDs 排列不同、集合相同 ⇒ 键相等（规范序）。
	reordered := build()
	reordered.groupIDs = []int64{9, 3}
	require.True(t, staticKeyOf(oldAv) == staticKeyOf(reordered), "groupIDs order must not affect the key")

	// 但**真的**变了就要不等。逐项验证键对这些事实是敏感的。
	for _, tc := range []struct {
		name   string
		mutate func(*snapshotStatic)
	}{
		{"identityRevision (K)", func(s *snapshotStatic) { s.acc.IdentityRevision = 6 }},
		{"name", func(s *snapshotStatic) { s.acc.Name = "other" }},
		{"effective baseURL (account override)", func(s *snapshotStatic) { s.acc.BaseURL = strPtr("https://override/v1") }},
		{"template baseURL", func(s *snapshotStatic) { s.tpl.BaseURL = "https://tpl2.example/v1" }},
		{"upstreamKey", func(s *snapshotStatic) { s.acc.UpstreamKey = "sk2" }},
		{"maxConcurrency", func(s *snapshotStatic) { s.acc.MaxConcurrency = 8 }},
		{"enabled", func(s *snapshotStatic) { s.acc.Enabled = false }},
		{"cacheDomain", func(s *snapshotStatic) { s.acc.CacheDomain = nil }},
		{"upstreamCostMultiplierBp", func(s *snapshotStatic) { s.acc.UpstreamCostMultiplierBp = 9000 }},
		{"groupIDs set", func(s *snapshotStatic) { s.groupIDs = []int64{3} }},
		{"codexAccountID", func(s *snapshotStatic) { s.acc.Ext.CodexAccountID = strPtr("other") }},
		{"codex identity quartet", func(s *snapshotStatic) { s.acc.Ext.CodexIdentity.SessionID = "sess2" }},
		{"credentialType", func(s *snapshotStatic) { s.tpl.CredentialType = "codex_oauth" }},
		{"stripImageTools", func(s *snapshotStatic) { s.tpl.StripImageTools = false }},
		{"models set", func(s *snapshotStatic) { s.tpl.Models = []string{"m-a"} }},
		{"supportedFormats set", func(s *snapshotStatic) { s.tpl.SupportedFormats = []domain.RequestFormat{"openai_chat"} }},
		{"formatModels", func(s *snapshotStatic) {
			s.tpl.FormatModels = map[domain.RequestFormat][]string{"openai_chat": {"m-a"}}
		}},
		{"modelMapping", func(s *snapshotStatic) {
			s.tpl.ModelMapping = domain.ModelMapping{"alias": {MappedModel: "up2", Mode: domain.ModelMappingModeExplicit}}
		}},
		{"modelMapping mode only", func(s *snapshotStatic) {
			s.tpl.ModelMapping = domain.ModelMapping{"alias": {MappedModel: "up", Mode: domain.ModelMappingModeImplicit}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build()
			tc.mutate(m)
			require.False(t, staticKeyOf(oldAv) == staticKeyOf(m), "key must be sensitive to %s", tc.name)
		})
	}

	// §5.5b(2)：OAuth token 与 email 明确**不**比较——它们不是身份事实，
	// 刷新不该改变候选身份（否则每次 SDK 自动刷新都会推翻在途工件）。
	// 凭据值是**从叶消费**的事实（上游鉴权用），故必须改键：不改键则轮转后
	// 键判等 ⇒ 复用旧叶 ⇒ 上游拿旧令牌 401（实测 TestInvalidateAccountReloadsExt）。
	// 注意「token 刷新不改**身份**」由 (I,K) 围栏另行承担，与本键无关——这正是
	// 本键与 (I,K) 必须分开的原因。
	t.Run("codexPATKey rotation MUST change the key", func(t *testing.T) {
		m := build()
		other := "pat-rotated"
		m.acc.Ext.CodexPATKey = &other
		require.False(t, staticKeyOf(oldAv) == staticKeyOf(m), "rotated credential must change the key (it is consumed from the leaf)")
	})
	t.Run("codexOAuthToken rotation MUST change the key", func(t *testing.T) {
		m := build()
		tok := "at-rotated"
		m.acc.Ext.CodexOAuthToken = &tok
		require.False(t, staticKeyOf(oldAv) == staticKeyOf(m), "rotated OAuth token must change the key (upstream auth consumes it)")
	})
	t.Run("codexOAuthRefreshToken rotation MUST change the key", func(t *testing.T) {
		m := build()
		rt := "rt-rotated"
		m.acc.Ext.CodexOAuthRefreshToken = &rt
		require.False(t, staticKeyOf(oldAv) == staticKeyOf(m), "rotated refresh token must change the key")
	})
	t.Run("codexEmail edit MUST change the key", func(t *testing.T) {
		m := build()
		m.acc.Ext.CodexEmail = strPtr("e@example.com")
		require.False(t, staticKeyOf(oldAv) == staticKeyOf(m), "email is reachable via Selection.Ext, so it must not be served stale")
	})
}

// TestStaticKeyNilSnapshotIsZeroKey 锁住首轮语义：无旧快照（oldByID 空）时
// staticKeyOf(nil) 返回零值键，且只在两侧都为空时相等。
func TestStaticKeyNilSnapshotIsZeroKey(t *testing.T) {
	var zero staticKey
	require.True(t, staticKeyOf(nil) == zero)
	require.False(t, staticKeyOf(nil) == staticKeyOf(&snapshotStatic{acc: domain.Account{ID: 1}}))
}

// TestMinGIDIsDeterministic 锁住 gid 的确定性派生：多组账号取最小组 ID，
// 与 map 迭代序无关。此前取「首个出现组」（map 迭代序首元素），同一份 DB
// 数据在不同进程/不同重载下可得不同 gid，而 gid 随事件投递归组
// （scheduler.go groupIDPtr(av.gid)）——组归属是静态事实，不得有这种自由度。
func TestMinGIDIsDeterministic(t *testing.T) {
	require.Equal(t, int64(3), minGID([]int64{3, 9, 5}))
	require.Equal(t, int64(3), minGID([]int64{9, 5, 3}))
	require.Equal(t, int64(3), minGID([]int64{9, 3, 5}))
	require.Equal(t, int64(-4), minGID([]int64{7, -4, 2}))
	require.Equal(t, int64(0), minGID(nil), "empty set yields the 0 sentinel (group IDs are positive autoincrement)")
}

// TestBuildSnapshotsGIDIsMinOfGroupIDs 在真实 buildSnapshots 上端到端验证：
// 多组账号的 gid 必须等于其 groupIDs 的最小值，且与 map 迭代序无关。
func TestBuildSnapshotsGIDIsMinOfGroupIDs(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	// 同一账号出现在 3 个组里，其 gid 必须是 2（最小值），而非碰巧首个被遍历到的组。
	a := acc(1, tp, 100000)
	m := map[int64][]*domain.Account{
		9: {a},
		2: {a},
		5: {a},
	}
	_, byID := buildSnapshots(m, nil)
	leaf, ok := byID[1]
	require.True(t, ok)
	st := leaf.static.Load()
	require.Equal(t, int64(2), st.eventGID(), "eventGID must be min(groupIDs)")
	require.ElementsMatch(t, []int64{2, 5, 9}, st.groupIDs)
	// 不变量：gid 必须始终是成员之一。
	require.Contains(t, st.groupIDs, st.eventGID(), "eventGID must be a member of groupIDs")
}
