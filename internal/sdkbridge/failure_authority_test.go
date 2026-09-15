// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

type failCASStoreErr struct {
	err error
}

func (f *failCASStoreErr) SetAccountFailed(_ context.Context, _ int64, _ time.Time, _ string) error {
	return nil
}
func (f *failCASStoreErr) GetAccount(_ context.Context, _ int64) (*domain.Account, error) {
	return nil, f.err
}
func (f *failCASStoreErr) FailAccountCAS(_ context.Context, _ int64, _ int64, _ string, _ time.Time, _ string) error {
	return nil
}

func TestSDKFailAPIKeyNilTemplateMissingDiscriminator(t *testing.T) {
	store := &fakeCASStore2{
		accounts: map[int64]*domain.Account{
			1: {ID: 1, UpstreamKey: "k1", LifecycleRevision: 1, Template: nil, Ext: nil},
		},
	}
	latch := newFakeLatch2()
	latch.m[1] = "latched-fp"
	failer := &fakeFailer2{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
	err := HandleFailure(context.Background(), deps, 1, fmt.Errorf("fatal"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMissingCredentialDiscriminator))
	require.Empty(t, failer.failed, "missing discriminator must not self-fail")
	require.Contains(t, latch.m, int64(1), "must preserve latch on missing discriminator")
}

func TestSDKFailResponsesSpecialAndUnknownRejected(t *testing.T) {
	for _, ct := range []credential.Type{credential.TypeResponsesSpecial, credential.Type("bogus"), credential.TypeAPIKey} {
		store := &fakeCASStore2{
			accounts: map[int64]*domain.Account{
				1: {ID: 1, UpstreamKey: "k1", LifecycleRevision: 1, Template: &domain.Template{CredentialType: ct}},
			},
		}
		latch := newFakeLatch2()
		failer := &fakeFailer2{}
		deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
		err := HandleFailure(context.Background(), deps, 1, fmt.Errorf("fatal"))
		require.NoError(t, err, "ct=%q", ct)
		require.Empty(t, failer.failed, "non-Codex %q must not self-fail", ct)
		require.Empty(t, latch.m, "rejected type must not leave latch")
	}
}

func TestSDKFailReadFailurePreservesLatch(t *testing.T) {
	readErr := errors.New("db read failed")
	store := &failCASStoreErr{err: readErr}
	latch := newFakeLatch2()
	latch.m[42] = "old-fp"
	failer := &fakeFailer2{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
	err := HandleFailure(context.Background(), deps, 42, fmt.Errorf("fatal"))
	require.Error(t, err)
	require.True(t, errors.Is(err, readErr))
	require.Empty(t, failer.failed, "read failure must not self-fail")
	require.Contains(t, latch.m, int64(42), "must preserve latch on read failure, never clear")
	require.Equal(t, "old-fp", latch.m[42])
}
