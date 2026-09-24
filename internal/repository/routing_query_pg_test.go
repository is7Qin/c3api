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

// rollupReadFixture writes quality facts through the real writer so the read
// face is proven against genuine routing_quality_fact content. The fact table is
// now the single source for the window reads; the same (minute, quality class,
// candidate) seen by two instance_src values proves the read folds shards at
// query time (the old merged rollup folded them at write time).
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

	seed := func(fp domain.CandidateFingerprintVal, instance string, minute time.Time, seq int64, row repository.RoutingQualityRow) {
		row.IdentityVersion = 1
		row.RouteClassID = rc
		row.QualityClassID = qc
		row.CandidateFingerprint = fp
		row.InstanceSrc = instance
		row.BucketMinute = minute
		row.AbsoluteSequence = seq
		require.NoError(t, repos.Partitions.UpsertQualityRow(ctx, row))
	}
	min2 := base.Add(time.Minute)

	// Given: minute 1 fp1 observed by two instances (10 + 4 attempts).
	seed(fp1, "src-a", base, 1, repository.RoutingQualityRow{
		Attempts: 10, Successes: 6, Count429: 1, CountOrdinary4xx: 1, Count5xx: 1, CountNetwork: 1,
		TTFTN: 5, TTFTSumLogQ32: 100, TTFTSumSqLogQ32: 200, TTFTHist: ones,
		InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 300, CacheCreateTokens: 400, Calls: 7, Images: 3,
	})
	seed(fp1, "src-b", base, 1, repository.RoutingQualityRow{Attempts: 4, Successes: 2})
	// Given: minute 2 fp1 + fp2.
	seed(fp1, "src-a", min2, 2, repository.RoutingQualityRow{
		Attempts: 5, Successes: 2, Count429: 2, CountOrdinary4xx: 1, Count5xx: 1, CountNetwork: 1,
		TTFTN: 3, TTFTSumLogQ32: 50, TTFTSumSqLogQ32: 60, TTFTHist: twos,
		InputTokens: 100, OutputTokens: 200, CacheReadTokens: 30, CacheCreateTokens: 40, Calls: 1,
	})
	seed(fp2, "src-a", min2, 2, repository.RoutingQualityRow{Attempts: 8, Successes: 3})
	// Given: a fact exactly at the exclusive upper bound — half-open [from, to)
	// must exclude it even though the row exists (the old fixture relied on the
	// minute never being rolled up; the read no longer depends on the rollup).
	seed(fp1, "src-a", base.Add(3*time.Minute), 1, repository.RoutingQualityRow{Attempts: 100})

	// When: full half-open window covering minutes 1 and 2.
	stats, err := repos.Partitions.QueryQualityFactStats(ctx, rc, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, stats)
	// Then: one group per candidate identity; fp1 sums across minutes AND
	// instance shards (10 + 4 + 5 = 19); the upper-bound minute must NOT appear.
	require.Len(t, stats, 2)
	byFP := map[string]repository.RoutingQualityStat{}
	for _, s := range stats {
		require.Equal(t, rc, s.RouteClassID)
		require.Equal(t, qc, s.QualityClassID)
		byFP[domain.CandidateFPHex(s.CandidateFingerprint)] = s
	}
	got1, ok := byFP[domain.CandidateFPHex(fp1)]
	require.True(t, ok)
	require.Equal(t, int64(19), got1.Attempts)
	require.Equal(t, int64(10), got1.Successes)
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

	// When: window excludes minute 2. Then: only minute 1 totals (both shards).
	stats, err = repos.Partitions.QueryQualityFactStats(ctx, rc, 1, base, min2)
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, fp1, stats[0].CandidateFingerprint)
	require.Equal(t, int64(14), stats[0].Attempts)
	require.Equal(t, ones, stats[0].TTFTHist)

	// Then: an unknown route class filters to empty, non-nil. (There is no
	// identity-version mismatch case to assert any more — the DB column is gone,
	// so a version-mismatch filter has no parameter to bind and nothing to match.)
	empty, err := repos.Partitions.QueryQualityFactStats(ctx, rcOther, 1, base, base.Add(3*time.Minute))
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
	edge := func(ord int16, acc int64, prev *int64, prevOut, reason, outcome string, term bool, gen int64, minute time.Time, chain int64) repository.RoutingFlowRow {
		return repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, TerminalMinute: minute, Ordinal: ord, Lane: "primary",
			AccountID: acc, PreviousAccountID: prev, PreviousOutcome: prevOut, TransitionReason: reason,
			Outcome: outcome, IsTerminal: term, Generation: gen, ChainCount: chain,
		}
	}

	// Given: minute 1 edges e1(ord1,acct10)+e2(ord2,acct20,prev10); snapshot writes merged rows directly.
	rows1 := []repository.RoutingFlowRow{
		edge(1, 10, nil, "", "init", "success", false, 1, base, 2),
		edge(2, 20, ptrInt64(10), "success", "retry", "success", true, 1, base, 5),
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", base, 1, 1, rows1))

	// Given: minute 2 edges e1'(same identity, chain 3)+e3(ord1,acct30).
	rows2 := []repository.RoutingFlowRow{
		edge(1, 10, nil, "", "init", "success", false, 1, min2, 3),
		edge(1, 30, nil, "", "init", "fail", true, 1, min2, 7),
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", min2, 1, 1, rows2))

	// Given: minute 3 direct snapshot (chain 50 visible — snapshots write through).
	rows3 := []repository.RoutingFlowRow{edge(2, 99, nil, "", "init", "success", true, 1, base.Add(2*time.Minute), 50)}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", base.Add(2*time.Minute), 1, 1, rows3))

	// When: full window. Then: one row per merged edge identity, chain_count
	// summed across minutes and shards, deterministic order (ordinal, then account_id).
	stats, err := repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.Len(t, stats, 4)
	require.Equal(t, rc, stats[0].RouteClassID)
	require.Equal(t, int16(1), stats[0].Ordinal)
	require.Equal(t, int64(10), stats[0].AccountID)
	require.Nil(t, stats[0].PreviousAccountID)
	require.Equal(t, int64(5), stats[0].ChainCount, "e1 chain 2+3 across minutes")
	require.Equal(t, int64(1), stats[0].MinGeneration)
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
	require.Equal(t, int64(99), stats[3].AccountID)
	require.Equal(t, int64(50), stats[3].ChainCount)

	// When: window excludes minute 2. Then: e1 keeps only minute 1 chain.
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base, min2)
	require.NoError(t, err)
	require.Len(t, stats, 2)
	require.Equal(t, int64(2), stats[0].ChainCount)

	// Then: filters — unknown route class is empty, non-nil.
	empty, err := repos.Partitions.QueryFlowFactStats(ctx, rcOther, 1, base, base.Add(3*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Len(t, empty, 0)
}
