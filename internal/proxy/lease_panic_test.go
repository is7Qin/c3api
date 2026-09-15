// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

type leaseMemLoader struct {
	mu      sync.Mutex
	byGroup map[int64][]*domain.Account
}

func (m *leaseMemLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		out[k] = v
	}
	return out, nil
}
func (m *leaseMemLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byGroup[id], nil
}

type leaseFakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
}

func (f *leaseFakeRuleStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (f *leaseFakeRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = f.next
	f.next++
	f.rules[r.ID] = r
	return r.ID, nil
}
func (f *leaseFakeRuleStore) UpdateRule(ctx context.Context, r domain.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[r.ID] = r
	return nil
}
func (f *leaseFakeRuleStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, id)
	return nil
}
func (f *leaseFakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.rules, id)
	}
	return nil
}
func (f *leaseFakeRuleStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

var _ repository.RuleStore = (*leaseFakeRuleStore)(nil)

func newLeaseScheduler(t *testing.T, tpl *domain.Template) *scheduler.Scheduler {
	t.Helper()
	loader := &leaseMemLoader{byGroup: map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "k", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4}}}}
	store := &leaseFakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	re := rule.New(rule.Config{}, store, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 2, SyncInterval: 100 * time.Hour}, loader, re, nil, nil, nil, nil)
	require.NoError(t, s.InvalidateAllSync())
	publishTestRoutes(t, s)

	return s
}

func TestLeasePanicGuardReleasesExactOnce(t *testing.T) {
	tpl := &domain.Template{ID: 1, BaseURL: "http://127.0.0.1:1", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"m"}}
	sched := newLeaseScheduler(t, tpl)
	sel, err := sched.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.NotNil(t, sel)
	ri, _ := sched.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency, "leased")

	func() {
		defer func() {
			_ = recover()
		}()
		func() {
			defer leaseGuard(sel)
			panic("injected panic after real Select")
		}()
	}()

	ri, _ = sched.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "panic guard must release exact token once")

	sel.Release()
	ri, _ = sched.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "second release no-op")
	require.GreaterOrEqual(t, ri.Concurrency, int64(0))
}
