// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

type fakeLatch2 struct {
	m  map[int64]string
	kv map[int64]int64
}

func newFakeLatch2() *fakeLatch2 {
	return &fakeLatch2{m: make(map[int64]string), kv: make(map[int64]int64)}
}
func (f *fakeLatch2) TryAcquire(id int64, fp string, rev int64) bool {
	f.m[id] = fp
	f.kv[id] = rev
	return true
}
func (f *fakeLatch2) Clear(id int64) { delete(f.m, id); delete(f.kv, id) }
func (f *fakeLatch2) IsLatched(id int64, fp string, rev int64) bool {
	v, ok := f.m[id]
	if !ok {
		return false
	}
	if fp != "" && v != fp {
		return false
	}
	if rev != 0 && f.kv[id] != rev {
		return false
	}
	return true
}

type fakeCASStore2 struct {
	accounts map[int64]*domain.Account
	casErr   error
	groups   map[int64][]int64
}

func (f *fakeCASStore2) SetAccountFailed(_ context.Context, id int64, _ time.Time, _ string) error {
	return nil
}
func (f *fakeCASStore2) GetAccount(_ context.Context, id int64) (*domain.Account, error) {
	a, ok := f.accounts[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return a, nil
}
func (f *fakeCASStore2) FailAccountCAS(_ context.Context, id int64, expected int64, source string, _ time.Time, _ string) error {
	if f.casErr != nil {
		return f.casErr
	}
	a, ok := f.accounts[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if a.IdentityRevision != expected {
		return fmt.Errorf("%w: stale", repository.ErrStaleIdentityRevision)
	}
	a.LifecycleRevision++
	return nil
}
func (f *fakeCASStore2) GetAccountGroups(_ context.Context, id int64) ([]int64, error) {
	return f.groups[id], nil
}

type fakeFailer2 struct{ failed []int64 }

func (f *fakeFailer2) FailAccount(id int64) { f.failed = append(f.failed, id) }

func TestSDKFailSharesLatchNonCodexCannotSelfFail(t *testing.T) {
	store := &fakeCASStore2{
		accounts: map[int64]*domain.Account{
			1: {ID: 1, UpstreamKey: "k1", LifecycleRevision: 1, IdentityRevision: 1, Template: &domain.Template{CredentialType: credential.TypeAPIKey, BaseURL: "https://api.openai.com"}},
			2: {ID: 2, UpstreamKey: "k2", LifecycleRevision: 1, IdentityRevision: 1, Template: &domain.Template{CredentialType: credential.TypeCodexOAuth, BaseURL: "https://api.openai.com"}},
		},
	}
	latch := newFakeLatch2()
	failer := &fakeFailer2{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
	require.NoError(t, HandleFailure(context.Background(), deps, 1, fmt.Errorf("fatal")))
	require.Empty(t, failer.failed, "non-Codex must not self-fail via SDK")
	require.Empty(t, latch.m)
	require.NoError(t, HandleFailure(context.Background(), deps, 2, fmt.Errorf("fatal")))
	require.Equal(t, []int64{2}, failer.failed)
	require.Empty(t, latch.m, "successful CAS clears old latch")
}

func TestSDKFailStaleRevisionFence(t *testing.T) {
	store := &fakeCASStore2{
		accounts: map[int64]*domain.Account{1: {ID: 1, UpstreamKey: "k1", LifecycleRevision: 5, IdentityRevision: 5, Template: &domain.Template{CredentialType: credential.TypeCodexOAuth, BaseURL: "https://api.openai.com"}}},
	}
	store.casErr = repository.ErrStaleIdentityRevision
	latch := newFakeLatch2()
	latch.m[1] = "k1"
	failer := &fakeFailer2{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
	err := HandleFailure(context.Background(), deps, 1, fmt.Errorf("fatal"))
	require.Error(t, err)
	require.Contains(t, latch.m, int64(1))
}
