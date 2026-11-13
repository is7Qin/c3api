package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// reserveConversationBaseAllocs 是 BASE（85c1ed0）实测的
// NewAttemptPlan + ReserveAttempt + Release 每次分配数（codex 账号，
// MaxConcurrency=4 与 0 均为 5）。会话路径三字段只做字段拷贝，不得超过它（C3-15）。
const reserveConversationBaseAllocs = 5

func newConversationSched(t *testing.T, accs ...*domain.Account) (*Scheduler, RouteRef) {
	t.Helper()
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	ids := make([]int64, 0, len(accs))
	for _, a := range accs {
		ids = append(ids, a.ID)
	}
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(ids...)})
	return s, route
}

func reserveOnce(t *testing.T, s *Scheduler, route RouteRef, conv bool) *Selection {
	t.Helper()
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "r", Conversation: conv}, route)
	require.NoError(t, err)
	sel, _, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	return sel
}

func requirePoolIdle(t *testing.T, pool *identityPool) {
	t.Helper()
	for i, sl := range pool.slots {
		require.False(t, sl.busy.Load(), "槽位 %d 不得被占用", i)
	}
}

// TestConversationSkipsSlotClaim（C3-3 中不依赖会话簿的部分）：Conversation 为真
// 时不认领槽位、池无占用与推进，Selection.Conversation 为真；CacheDomain /
// InstallationID 取自快照静态视图（含私有域）；Conversation 为假时仍按槽位认领；
// MaxConcurrency=0（无池）不 panic、零身份。
func TestConversationSkipsSlotClaim(t *testing.T) {
	t.Run("conversation does not claim", func(t *testing.T) {
		a := codexAcc(1, domain.FormatOpenAIResponses, "m", 2, "inst-1")
		s, route := newConversationSched(t, a)
		pool := s.View().static.identityPools.pools[1]
		require.NotNil(t, pool)
		threads := make([]string, len(pool.slots))
		for i, sl := range pool.slots {
			threads[i] = sl.st.ThreadID
		}

		sel := reserveOnce(t, s, route, true)
		require.True(t, sel.Conversation)
		require.Nil(t, sel.session, "会话路径不认领槽位")
		require.Nil(t, sel.pool)
		require.Equal(t, "private:1", sel.CacheDomain, "未配置缓存域 = 私有域")
		require.Equal(t, "inst-1", sel.InstallationID)
		requirePoolIdle(t, pool)
		sel.Release()
		requirePoolIdle(t, pool)
		for i, sl := range pool.slots {
			require.Equal(t, threads[i], sl.st.ThreadID, "槽位 %d 不得推进", i)
		}
	})

	t.Run("slot path still claims", func(t *testing.T) {
		a := codexAcc(1, domain.FormatOpenAIResponses, "m", 2, "inst-1")
		s, route := newConversationSched(t, a)
		pool := s.View().static.identityPools.pools[1]

		sel := reserveOnce(t, s, route, false)
		require.False(t, sel.Conversation)
		require.NotNil(t, sel.session)
		require.Same(t, pool, sel.pool)
		require.True(t, sel.session.busy.Load())
		require.Equal(t, "private:1", sel.CacheDomain, "槽位路径同样携带账号事实")
		require.Equal(t, "inst-1", sel.InstallationID)
		sel.Release()
		requirePoolIdle(t, pool)
	})

	t.Run("configured cache domain", func(t *testing.T) {
		a := codexAcc(2, domain.FormatOpenAIResponses, "m", 2, "inst-2")
		shared := "team-a"
		a.CacheDomain = &shared
		s, route := newConversationSched(t, a)

		sel := reserveOnce(t, s, route, true)
		require.Equal(t, "team-a", sel.CacheDomain)
		require.Equal(t, "inst-2", sel.InstallationID)
		sel.Release()
	})

	t.Run("non codex carries no facts", func(t *testing.T) {
		a := acc(3, tpl(3, domain.FormatOpenAIResponses, []string{"m"}), 2)
		s, route := newConversationSched(t, a)

		sel := reserveOnce(t, s, route, true)
		require.True(t, sel.Conversation)
		require.Nil(t, sel.session)
		require.Empty(t, sel.CacheDomain, "仅 Codex 凭据填缓存域")
		require.Empty(t, sel.InstallationID)
		sel.Release()
	})

	t.Run("no pool when max concurrency zero", func(t *testing.T) {
		a := codexAcc(4, domain.FormatOpenAIResponses, "m", 0, "inst-4")
		s, route := newConversationSched(t, a)
		require.Nil(t, s.View().static.identityPools.pools[4], "MaxConcurrency=0 不建池")

		for _, conv := range []bool{false, true} {
			sel := reserveOnce(t, s, route, conv)
			require.Equal(t, conv, sel.Conversation)
			require.Nil(t, sel.session, "无池 → 零身份")
			require.Nil(t, sel.pool)
			require.Equal(t, "private:4", sel.CacheDomain)
			require.Equal(t, "inst-4", sel.InstallationID)
			sel.Release()
		}
	})
}

// TestReserveConversationNoNewAllocs（C3-15 预留部分）：会话路径与槽位路径的
// NewAttemptPlan + ReserveAttempt + Release 分配数都不超过 BASE 常数。
func TestReserveConversationNoNewAllocs(t *testing.T) {
	for _, maxConc := range []int{4, 0} {
		a := codexAcc(1, domain.FormatOpenAIResponses, "m", maxConc, "inst-1")
		s, route := newConversationSched(t, a)
		for _, conv := range []bool{false, true} {
			var sel *Selection
			allocs := testing.AllocsPerRun(1000, func() {
				plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "r", Conversation: conv}, route)
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := s.ReserveAttempt(&plan)
				if err != nil {
					t.Fatal(err)
				}
				sel = got
				got.Release()
			})
			require.NotNil(t, sel)
			require.Equal(t, "inst-1", sel.InstallationID)
			require.LessOrEqual(t, allocs, float64(reserveConversationBaseAllocs),
				"maxConc=%d conv=%v allocs=%v", maxConc, conv, allocs)
		}
	}
}
