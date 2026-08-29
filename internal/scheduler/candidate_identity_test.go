// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

func TestCandidateFingerprintProducerMatchesSelectionAndFailureEvent(t *testing.T) {
	tpl := tpl(10, domain.FormatOpenAIChat, []string{"m"})
	tpl.CredentialType = credential.TypeCodexOAuth
	a := acc(1, tpl, 4)
	a.Ext = &domain.AccountExt{
		AccountID: 1, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:  &domain.CodexIdentity{InstallationID: "i", SessionID: "s", ThreadID: "t", WindowID: "w"},
		CodexAccountID: strPtrT("ca"),
	}
	s := newTestScheduler(t, []*domain.Account{a})

	want, err := candidateFingerprint(a)
	require.NoError(t, err)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, want, sel.CandidateFingerprint)

	ev := s.failureEvent(sel.AccountID, rule.Kind5xx, "boom")
	require.Equal(t, want, ev.CandidateFingerprint)
	s.Release(sel.AccountID)
}

func TestRulePersistRejectsStaleOrMismatchedFailureIdentity(t *testing.T) {
	a := acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)
	a.LifecycleRevision = 1
	store := &failurePersistStore{account: a}
	fp, err := candidateFingerprint(a)
	require.NoError(t, err)
	persist := NewRulePersistFunc(store, newLatchStore(), nil, nil)

	for _, tc := range []struct {
		name  string
		event rule.Event
		want  error
	}{
		{name: "missing identity", event: rule.Event{AccountID: 1, ExpectedRevision: 1}, want: ErrMissingCandidateFingerprint},
		{name: "mismatched identity", event: rule.Event{AccountID: 1, ExpectedRevision: 1, CandidateFingerprint: fp + "x"}, want: ErrCandidateFingerprintMismatch},
		{name: "stale revision", event: rule.Event{AccountID: 1, ExpectedRevision: 2, CandidateFingerprint: fp}, want: ErrStaleFailureRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := persist(context.Background(), rule.PersistItem{Event: tc.event, Then: domain.RuleThen{FailAccount: true}})
			require.Error(t, err)
			require.True(t, errors.Is(err, tc.want))
			require.Equal(t, 0, store.failCalls)
		})
	}
}

type failurePersistStore struct {
	account   *domain.Account
	failCalls int
}

func (s *failurePersistStore) GetAccount(context.Context, int64) (*domain.Account, error) {
	return s.account, nil
}
func (s *failurePersistStore) GetAccountGroups(context.Context, int64) ([]int64, error) {
	return nil, nil
}
func (s *failurePersistStore) FailAccountCAS(context.Context, int64, int64, string, time.Time, string) error {
	s.failCalls++
	return nil
}
