// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Fixed routing-plan-memory fixtures (spec §6 r3). BASE-COMPATIBLE: this file
// uses only symbols that exist at BASE commit d0d59648 (plus Go stdlib), so the
// same fixture/harness source can be copied into the BASE worktree for
// interleaved BASE/HEAD measurement. It never touches the DB or production.
//
// Formats: spec r3 fixes the four single-operation formats to
// responses / chat / messages / responses-ws (FormatOpenAIResponses,
// FormatOpenAIChat, FormatAnthropic, FormatOpenAIResponsesWS); bare "search" is
// not a standalone routing format in this fixture.
//
// Clock: the fixture injects a fixed time (spec §6), never the wall clock, so
// both sides compile under the same minute/window boundary.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

const rpmAccounts = 85

// rpmBenchModels is the env-selected model count for the spec matrix
// (100/300/693). Shared by BASE and HEAD so both sides measure the same size.
func rpmBenchModels() int {
	if v := os.Getenv("RPM_MODELS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

// rpmPrices returns a known purchase price for every model, so candidate cost
// is known (primaries exist) rather than degrading every candidate to explore.
func rpmPrices(models []string) map[string]domain.ResolvedPrices {
	p := make(map[string]domain.ResolvedPrices, len(models))
	for _, m := range models {
		p[m] = domain.ResolvedPrices{InputPerM: new(int64(1000)), OutputPerM: new(int64(1000))}
	}
	return p
}

func rpmSingleOpFormats() []domain.RequestFormat {
	return []domain.RequestFormat{
		domain.FormatOpenAIResponses,
		domain.FormatOpenAIChat,
		domain.FormatAnthropic,
		domain.FormatOpenAIResponsesWS,
	}
}

// rpmTemplate builds the multi-format single-operation template shared by the
// homogeneous fixture (one template, all four formats, given model list).
func rpmTemplate(id int64, models []string) *domain.Template {
	return &domain.Template{
		ID:               id,
		BaseURL:          "https://u/v1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: rpmSingleOpFormats(),
		Models:           models,
	}
}

// rpmModels builds the deterministic model list m000..m(N-1).
func rpmModels(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("m%03d", i))
	}
	return out
}

// rpmHomogeneousScheduler builds the spec's homogeneous fixture: 85 accounts
// that are members of BOTH groups, all supporting every given model/format,
// Enabled, no explicit cache_domain — so every route has the same 85
// private-domain set (D=85, U=1). R = 2 groups × 4 formats × len(models).
func rpmHomogeneousScheduler(tb testing.TB, models []string) (*Scheduler, *memLoader) {
	tb.Helper()
	tpl := rpmTemplate(1, models)
	accs := make([]*domain.Account, 0, rpmAccounts)
	for id := int64(1); id <= rpmAccounts; id++ {
		accs = append(accs, acc(id, tpl, 100000))
	}
	// Same account objects in both groups: one immutable leaf carries both
	// groupIDs, matching the spec's D=85/U=1 shape.
	m := newMemLoader(map[int64][]*domain.Account{10: accs, 20: accs})
	s := rpmLoadScheduler(tb, m)
	return s, m
}

// rpmHeterogeneousScheduler builds a fixture whose routes have DIFFERENT actual
// domain sets: 85 accounts split across four templates (each template supports a
// disjoint round-robin model subset) with a per-template explicit cache_domain,
// so a model's route domain set is that template's single explicit domain and U
// is the number of distinct domains actually serving the model space.
func rpmHeterogeneousScheduler(tb testing.TB, models []string) (*Scheduler, *memLoader) {
	tb.Helper()
	const templates = 4
	tplModels := make([][]string, templates)
	for i, m := range models {
		t := i % templates
		tplModels[t] = append(tplModels[t], m)
	}
	tpls := make([]*domain.Template, templates)
	domains := make([]string, templates)
	for t := range templates {
		tpls[t] = rpmTemplate(int64(t+1), tplModels[t])
		domains[t] = fmt.Sprintf("dom-%d", t)
	}
	byGroup := map[int64][]*domain.Account{}
	for gid, base := range map[int64]int64{10: 0, 20: 1000} {
		accs := make([]*domain.Account, 0, rpmAccounts)
		for i := range rpmAccounts {
			t := i % templates
			a := acc(base+int64(i)+1, tpls[t], 100000)
			d := domains[t]
			a.CacheDomain = &d
			accs = append(accs, a)
		}
		byGroup[gid] = accs
	}
	m := newMemLoader(byGroup)
	s := rpmLoadScheduler(tb, m)
	return s, m
}

// rpmFixedClock is the injected wall-clock anchor for every fixture scheduler:
// a fixed minute so both BASE and HEAD compile under identical time/window
// boundaries and neither measures wall-clock drift (spec §6 "固定时间").
var rpmFixedClock = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// rpmLoadScheduler reloads the static root and runs one full compile fire with
// a fixed injected clock. The scheduler is Closed at test cleanup so a probe
// never leaves an idle root behind between cases.
func rpmLoadScheduler(tb testing.TB, m *memLoader) *Scheduler {
	tb.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(tb, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil, nil, nil, nil)
	s.timeNow = func() time.Time { return rpmFixedClock }
	tb.Cleanup(func() { _ = s.Close(context.Background()) })
	require.NoError(tb, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// TestRPMFixtureTopology pins the r3 fixture obligations: exact route count
// (2 groups × 4 single-op formats × M models), exactly the four format
// identities, and the homogeneous candidate/domain shape (every route serves
// all 85 accounts, so D=85 private domains) so BASE and HEAD measure the same
// topology.
func TestRPMFixtureTopology(t *testing.T) {
	const models = 100
	s, _ := rpmHomogeneousScheduler(t, rpmModels(models))
	dv := s.View().DecisionView()
	require.Equal(t, 2*4*models, len(dv.routes), "R = 2 groups × 4 formats × M models")
	formats := map[string]struct{}{}
	for ref := range dv.routes {
		formats[ref.Format] = struct{}{}
	}
	require.Len(t, formats, 4)
	for _, f := range rpmSingleOpFormats() {
		_, ok := formats[string(f)]
		require.True(t, ok, "missing fixture format %s", f)
	}
	// Homogeneous shape (spec §6): D=85 distinct private domains and 85
	// candidate accounts per route, all groups/formats modelled.
	for ref, rd := range dv.routes {
		require.NotNil(t, rd, "route %v", ref)
		require.Len(t, rd.CacheDomainRing.Domains, rpmAccounts, "D=85 per route %v", ref)
		require.Len(t, rd.CacheDomainAccounts, rpmAccounts, "85 account mappings per route %v", ref)
		got := len(rd.Primary) + len(rd.Degraded) + len(rd.Explore.Ordered)
		require.Equal(t, rpmAccounts, got, "85 candidate accounts per route %v", ref)
	}
}
