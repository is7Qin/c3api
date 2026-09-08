// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

type errGetAccountStore struct {
	accounts map[int64]*domain.Account
	err      error
	casCalls int
}

func (s *errGetAccountStore) GetAccount(_ context.Context, id int64) (*domain.Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	a, ok := s.accounts[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return a, nil
}
func (s *errGetAccountStore) FailAccountCAS(_ context.Context, id int64, expected int64, _ string, _ time.Time, _ string) error {
	s.casCalls++
	return nil
}
func (s *errGetAccountStore) GetAccountGroups(_ context.Context, _ int64) ([]int64, error) {
	return nil, nil
}

func TestRulePersistFailClosedOnGetAccountError(t *testing.T) {
	store := &errGetAccountStore{
		accounts: map[int64]*domain.Account{
			7: {ID: 7, LifecycleRevision: 3, Template: &domain.Template{ID: 1, BaseURL: "https://api.openai.com", CredentialType: "api_key"}},
		},
		err: errors.New("transient db down"),
	}
	latch := newLatchStore()
	// acquire latch first to simulate HealthController acquired
	latch.TryAcquire(7, "fp-ignored", 3)
	require.True(t, latch.IsLatched(7, "fp-ignored"))
	fn := NewRulePersistFunc(store, latch, nil, nil)
	item := rule.PersistItem{
		Event: rule.Event{AccountID: 7, ExpectedRevision: 3, CandidateFingerprint: "fp", ErrorMessage: "boom"},
		Then:  domain.RuleThen{FailAccount: true},
	}
	// barrier: persist func must return error and preserve latch (fail-closed)
	done := make(chan error, 1)
	go func() { done <- fn(context.Background(), item) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "barrier timeout")
	}
	require.Error(t, err, "GetAccount read error must propagate, not silent success")
	require.True(t, latch.IsLatched(7, "fp-ignored"), "latch must not be cleared on GetAccount error (fail-closed)")
	require.Equal(t, 0, store.casCalls, "must not CAS after GetAccount error")
}
