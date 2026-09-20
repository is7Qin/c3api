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
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/repository"
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
	a.IdentityRevision = 1
	store := &failurePersistStore{account: a}
	fp, err := candidateFingerprint(a)
	require.NoError(t, err)
	persist := NewRulePersistFunc(store, latch.NewLatchStore(), nil, nil)

	for _, tc := range []struct {
		name  string
		event rule.Event
		want  error
	}{
		{name: "missing identity", event: rule.Event{AccountID: 1, ExpectedIdentityRevision: 1}, want: ErrMissingCandidateFingerprint},
		{name: "mismatched identity", event: rule.Event{AccountID: 1, ExpectedIdentityRevision: 1, CandidateFingerprint: fp + "x"}, want: ErrCandidateFingerprintMismatch},
		{name: "missing expected identity revision", event: rule.Event{AccountID: 1, CandidateFingerprint: fp}, want: ErrMissingExpectedRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := persist(context.Background(), rule.PersistItem{Event: tc.event, Then: domain.RuleThen{FailAccount: true}})
			require.Error(t, err)
			require.True(t, errors.Is(err, tc.want))
			require.Equal(t, 0, store.failCalls)
		})
	}
}

// 陈旧 K 由 CAS guard 判定——rule_persist.go 的预检已删除（它与 CAS 重复，
// 且读 C 而非 K，只可能与 CAS 分歧）。本用例证明 persist 函数**传播** CAS 的
// 陈旧判定（不吞掉），并在「陈旧 K + 已推进的新 K」时清掉陈旧锁存。
func TestRulePersistPropagatesStaleIdentityFromCAS(t *testing.T) {
	a := acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)
	a.IdentityRevision = 3 // 身份已推进到 3
	store := &failurePersistStore{account: a, casErr: repository.ErrStaleIdentityRevision}
	fp, err := candidateFingerprint(a)
	require.NoError(t, err)
	ls := latch.NewLatchStore()
	require.True(t, ls.TryAcquire(1, fp, 1))

	persist := NewRulePersistFunc(store, ls, nil, nil)
	err = persist(context.Background(), rule.PersistItem{
		Event: rule.Event{AccountID: 1, ExpectedIdentityRevision: 1, CandidateFingerprint: fp, ErrorMessage: "boom"},
		Then:  domain.RuleThen{FailAccount: true},
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, repository.ErrStaleIdentityRevision), "persist must propagate the CAS staleness verdict")
	require.Equal(t, 1, store.failCalls, "the CAS must have been attempted")
	require.False(t, ls.IsLatched(1, fp), "stale K with an advanced fresh K must clear the stale latch")
}

type failurePersistStore struct {
	account   *domain.Account
	failCalls int
	casErr    error
}

func (s *failurePersistStore) GetAccount(context.Context, int64) (*domain.Account, error) {
	return s.account, nil
}
func (s *failurePersistStore) GetAccountGroups(context.Context, int64) ([]int64, error) {
	return nil, nil
}
func (s *failurePersistStore) FailAccountCAS(context.Context, int64, int64, string, time.Time, string) error {
	s.failCalls++
	return s.casErr
}
