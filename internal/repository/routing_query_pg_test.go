// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// rollupReadFixture writes quality facts via the real writer+rollup pipeline so
// the read face is proven against genuine routing_quality_rollup content.
func TestRoutingQualityRollupQueryPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, base))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rcOther := mustRouteClassVal(t, 2, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp1 := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-rq1", "", "", "", false, "", "", "", "")
	fp2 := mustFPVal(t, 2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-rq2", "", "", "", false, "", "", "", "")
	ones := []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	twos := []int64{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}

	// Given: minute 1 fact for fp1, rolled up.
	row1 := repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp1,
		InstanceSrc: "src-RQ", BucketMinute: base, AbsoluteSequence: 1,
		Attempts: 10, Successes: 6, Count429: 1, CountOrdinary4xx: 1, Count5xx: 1, CountNetwork: 1,
		TTFTN: 5, TTFTSumLogQ32: 100, TTFTSumSqLogQ32: 200, TTFTHist: ones,
		InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 300, CacheCreateTokens: 400, Calls: 7, Images: 3,
	}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row1))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, base, 1))

	// Given: minute 2 facts for fp1 + fp2, rolled up.
	min2 := base.Add(time.Minute)
	row2 := row1
	row2.BucketMinute = min2
	row2.AbsoluteSequence = 2
	row2.Attempts = 5
	row2.Successes = 2
	row2.Count429 = 2
	row2.TTFTN = 3
	row2.TTFTSumLogQ32 = 50
	row2.TTFTSumSqLogQ32 = 60
	row2.TTFTHist = twos
	row2.InputTokens = 100
	row2.OutputTokens = 200
	row2.CacheReadTokens = 30
	row2.CacheCreateTokens = 40
	row2.Calls = 1
	row2.Images = 0
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row2))
	row3 := row2
	row3.CandidateFingerprint = fp2
	row3.Attempts = 8
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row3))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, min2, 1))

	// Given: minute 3 fact NOT rolled up (rollup-only read must ignore it).
	row4 := row1
	row4.BucketMinute = base.Add(2 * time.Minute)
	row4.AbsoluteSequence = 2
	row4.Attempts = 100
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row4))

	// When: full half-open window covering all rolled minutes.
	stats, err := repos.Partitions.QueryQualityRollupStats(ctx, rc, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, stats)
	// Then: one group per candidate identity; fp1 sums across minutes (100 from
	// the un-rolled minute 3 must NOT appear).
	require.Len(t, stats, 2)
	byFP := map[string]repository.RoutingQualityStat{}
	for _, s := range stats {
		require.Equal(t, rc, s.RouteClassID)
		require.Equal(t, qc, s.QualityClassID)
		byFP[domain.CandidateFPHex(s.CandidateFingerprint)] = s
	}
	got1, ok := byFP[domain.CandidateFPHex(fp1)]
	require.True(t, ok)
	require.Equal(t, int64(15), got1.Attempts)
	require.Equal(t, int64(8), got1.Successes)
	require.Equal(t, int64(3), got1.Count429)
	require.Equal(t, int64(2), got1.CountOrdinary4xx)
	require.Equal(t, int64(2), got1.Count5xx)
	require.Equal(t, int64(2), got1.CountNetwork)
	require.Equal(t, int64(8), got1.TTFTN)
	require.Equal(t, int64(150), got1.TTFTSumLogQ32)
	require.Equal(t, int64(260), got1.TTFTSumSqLogQ32)
	require.Equal(t, []int64{3, 3, 3, 3, 3, 3, 3, 3, 3, 3}, got1.TTFTHist)
	require.Equal(t, int64(1100), got1.InputTokens)
	require.Equal(t, int64(2200), got1.OutputTokens)
	require.Equal(t, int64(330), got1.CacheReadTokens)
	require.Equal(t, int64(440), got1.CacheCreateTokens)
	require.Equal(t, int64(8), got1.Calls)
	require.Equal(t, int64(3), got1.Images)
	got2, ok := byFP[domain.CandidateFPHex(fp2)]
	require.True(t, ok)
	require.Equal(t, int64(8), got2.Attempts)

	// When: window excludes minute 2. Then: only minute 1 totals.
	stats, err = repos.Partitions.QueryQualityRollupStats(ctx, rc, 1, base, min2)
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, fp1, stats[0].CandidateFingerprint)
	require.Equal(t, int64(10), stats[0].Attempts)
	require.Equal(t, ones, stats[0].TTFTHist)

	// Then: filters — unknown route class and wrong identity version are empty, non-nil.
	empty, err := repos.Partitions.QueryQualityRollupStats(ctx, rcOther, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Len(t, empty, 0)
	empty, err = repos.Partitions.QueryQualityRollupStats(ctx, rc, 2, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Len(t, empty, 0)
}

func TestRoutingFlowRollupQueryPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	min2 := base.Add(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, base))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rcOther := mustRouteClassVal(t, 2, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp1 := mustFPVal(t, 10, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-rf1", "", "", "", false, "", "", "", "")
	fp2 := mustFPVal(t, 20, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-rf2", "", "", "", false, "", "", "", "")
	fp3 := mustFPVal(t, 30, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-rf3", "", "", "", false, "", "", "", "")
	edge := func(ord int16, acc int64, prev *int64, prevOut, reason, outcome string, term bool, gen int64, fp domain.CandidateFingerprintVal, minute time.Time, chain int64) repository.RoutingFlowRow {
		return repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, TerminalMinute: minute, Ordinal: ord, Lane: "primary",
			AccountID: acc, PreviousAccountID: prev, PreviousOutcome: prevOut, TransitionReason: reason,
			Outcome: outcome, IsTerminal: term, Generation: gen, CandidateFingerprint: fp, ChainCount: chain,
		}
	}

	// Given: minute 1 edges e1(ord1,acct10)+e2(ord2,acct20,prev10); rolled up.
	rows1 := []repository.RoutingFlowRow{
		edge(1, 10, nil, "", "init", "success", false, 1, fp1, base, 2),
		edge(2, 20, ptrInt64(10), "success", "retry", "success", true, 1, fp2, base, 5),
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", base, 1, 1, rows1))
	require.NoError(t, repos.Partitions.RollupFlow(ctx, base, 1))

	// Given: minute 2 edges e1'(same identity, chain 3)+e3(ord1,acct30); rolled up.
	rows2 := []repository.RoutingFlowRow{
		edge(1, 10, nil, "", "init", "success", false, 1, fp1, min2, 3),
		edge(1, 30, nil, "", "init", "fail", true, 1, fp3, min2, 7),
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", min2, 1, 1, rows2))
	require.NoError(t, repos.Partitions.RollupFlow(ctx, min2, 1))

	// Given: minute 3 fact NOT rolled up (chain 50 must stay invisible).
	rows3 := []repository.RoutingFlowRow{edge(2, 99, nil, "", "init", "success", true, 1, fp1, base.Add(2*time.Minute), 50)}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", base.Add(2*time.Minute), 1, 1, rows3))

	// When: full window. Then: one row per complete edge identity, chain_count
	// summed across minutes, deterministic order (ordinal, then account_id).
	stats, err := repos.Partitions.QueryFlowRollupStats(ctx, rc, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.Len(t, stats, 3)
	require.Equal(t, rc, stats[0].RouteClassID)
	require.Equal(t, int16(1), stats[0].Ordinal)
	require.Equal(t, int64(10), stats[0].AccountID)
	require.Nil(t, stats[0].PreviousAccountID)
	require.Equal(t, int64(5), stats[0].ChainCount, "e1 chain 2+3 across minutes")
	require.Equal(t, fp1, stats[0].CandidateFingerprint)
	require.Equal(t, int64(30), stats[1].AccountID)
	require.Equal(t, int64(7), stats[1].ChainCount)
	require.Equal(t, "fail", stats[1].Outcome)
	require.True(t, stats[1].IsTerminal)
	require.Equal(t, int16(2), stats[2].Ordinal)
	require.Equal(t, int64(20), stats[2].AccountID)
	require.NotNil(t, stats[2].PreviousAccountID)
	require.Equal(t, int64(10), *stats[2].PreviousAccountID)
	require.Equal(t, "retry", stats[2].TransitionReason)
	require.Equal(t, int64(5), stats[2].ChainCount)

	// When: window excludes minute 2. Then: e1 keeps only minute 1 chain.
	stats, err = repos.Partitions.QueryFlowRollupStats(ctx, rc, 1, base, min2)
	require.NoError(t, err)
	require.Len(t, stats, 2)
	require.Equal(t, int64(2), stats[0].ChainCount)

	// Then: filters — unknown route class is empty, non-nil.
	empty, err := repos.Partitions.QueryFlowRollupStats(ctx, rcOther, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Len(t, empty, 0)
}
