// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

type panicProxyAttempt struct{}

func (p *panicProxyAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	panic("simulated upstream panic")
}

func TestProxyRealPanicReleasesLease(t *testing.T) {
	px := newTestProxy(t, "http://127.0.0.1:9", 1)
	sel, plan, attempt, err := px.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o", scheduler.AttemptPlanIdentity{RequestID: "req", UserID: 1})
	require.NoError(t, err)
	// v4-S1: the session is a stack value — bound identity echoes the request.
	require.Equal(t, "req", plan.Identity().RequestID)
	ri, _ := px.sched.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	pa := &panicProxyAttempt{}
	require.Panics(t, func() {
		px.failoverLoopWithPlan(w, req, domain.FormatOpenAIChat, "req", 10, time.Now(), "gpt-4o", []byte("{}"), sel, plan, attempt, attemptState{}, pa, &httpSink{}, false)
	})
	ri, _ = px.sched.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "panic must release lease via failoverLoopWithPlan guard")
	require.NotPanics(t, func() { sel.Release() })
}
