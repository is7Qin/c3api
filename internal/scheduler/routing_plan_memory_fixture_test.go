// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Fixed routing-plan-memory fixtures (spec §6). BASE-COMPATIBLE: this file uses
// only symbols that exist at BASE commit d0d59648 (plus Go stdlib), so the same
// fixture/harness source can be copied into the BASE worktree for interleaved
// BASE/HEAD measurement. It never touches the DB or production.
//
// Formats: the spec's four single-operation formats are responses / chat /
// messages / search, but scheduler search is an OPAQUE topology that does not
// build a routing bucket (see search_opaque_topology_test.go), so search cannot
// produce a sortedRouteKeys route. Per parent direction the fourth single-op
// format is responses-ws (FormatOpenAIResponsesWS, single operation).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

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

// rpmLoadScheduler reloads the static root and runs one full compile fire.
func rpmLoadScheduler(tb testing.TB, m *memLoader) *Scheduler {
	tb.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(tb, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil, nil, nil, nil)
	require.NoError(tb, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// TestRPMFixtureTopology pins the r3 fixture obligation: exact route count
// (2 groups × 4 single-op formats × M models) and exactly the four format
// identities, so BASE and HEAD measure the same topology.
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
}
