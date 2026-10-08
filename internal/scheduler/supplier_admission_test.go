// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// supplier_admission_test.go 验证供给准入门（spec 2026-10-09 §4.6/A19④）：预留
// 放行候选时**同一次视图读取**捕获财务上下文，随 Selection 携带；A→B failover
// 各尝试分别携带各自 owner/bp/revision，终态尝试用其 fin。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// fakeSupplierAdmission 按账号返回财务上下文（模拟 supplier.SupplierSnapshot）。
type fakeSupplierAdmission struct {
	byAccount map[int64]domain.SupplierFinance
	reject    map[int64]bool
	seen      []int64
}

func (f *fakeSupplierAdmission) AdmitSupplierAccount(accountID, ownerUID int64) (domain.SupplierFinance, bool) {
	f.seen = append(f.seen, accountID)
	if f.reject[accountID] {
		return domain.SupplierFinance{}, false
	}
	fin, ok := f.byAccount[accountID]
	return fin, ok
}

// TestReserveAttemptCapturesSupplierFinancePerCandidate A19④：A→B failover 各尝试
// 分别捕获各自 owner/bp/revision；终态尝试的 Selection 携带其 fin。
func TestReserveAttemptCapturesSupplierFinancePerCandidate(t *testing.T) {
	tmpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tmpl, 4)
	a1.SupplierUserID = 100
	a2 := acc(2, tmpl, 4)
	a2.SupplierUserID = 200
	s := newTestScheduler(t, []*domain.Account{a1, a2})
	adm := &fakeSupplierAdmission{byAccount: map[int64]domain.SupplierFinance{
		1: {Ready: true, UID: 100, Bp: 7000, Rev: 3},
		2: {Ready: true, UID: 200, Bp: 3000, Rev: 9},
	}}
	s.SetSupplierAdmission(adm)

	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-fin", MaxAttempts: 2}, route)
	require.NoError(t, err)

	sel1, attempt1, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt1.AccountID)
	require.Equal(t, domain.SupplierFinance{Ready: true, UID: 100, Bp: 7000, Rev: 3}, sel1.SupplierFinance)
	sel1.Release()

	sel2, attempt2, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt2.AccountID, "A→B failover 终态尝试")
	require.Equal(t, domain.SupplierFinance{Ready: true, UID: 200, Bp: 3000, Rev: 9}, sel2.SupplierFinance, "终态尝试用其 fin")
	sel2.Release()

	require.Equal(t, []int64{1, 2}, adm.seen, "准入须按候选账号分别查询")
}

// TestReserveAttemptRejectsNotReadyCandidate 未就绪候选（准入门拒绝）不入选、不耗
// attempt——自然落到下一候选；全拒 ⇒ ErrAttemptsExhausted。
func TestReserveAttemptRejectsNotReadyCandidate(t *testing.T) {
	tmpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tmpl, 4)
	a1.SupplierUserID = 100
	a2 := acc(2, tmpl, 4)
	a2.SupplierUserID = 200
	s := newTestScheduler(t, []*domain.Account{a1, a2})
	adm := &fakeSupplierAdmission{
		byAccount: map[int64]domain.SupplierFinance{2: {Ready: true, UID: 200, Bp: 3000, Rev: 9}},
		reject:    map[int64]bool{1: true},
	}
	s.SetSupplierAdmission(adm)

	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-adm", MaxAttempts: 2}, route)
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt.AccountID, "被拒候选不入选，落到下一候选")
	require.Equal(t, int64(200), sel.SupplierFinance.UID)
	sel.Release()

	// 全部候选被拒 ⇒ 无可用。
	plan2, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-adm2", MaxAttempts: 2}, route)
	require.NoError(t, err)
	adm.reject[2] = true
	_, _, err = s.ReserveAttempt(&plan2)
	require.ErrorIs(t, err, ErrAttemptsExhausted)
}
