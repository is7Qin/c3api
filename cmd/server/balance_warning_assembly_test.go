// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/notification"
	"github.com/is7qin/c3api/internal/worker"
)

type fakeBalanceWarningService struct {
	enabled         bool
	mailConfigCalls int
}

func (s *fakeBalanceWarningService) BalanceWarningEnabled() bool { return s.enabled }

func (s *fakeBalanceWarningService) MailConfig() (string, int, string, string, string, string, bool) {
	s.mailConfigCalls++
	return "smtp.example.com", 465, "user", "secret", "from@example.com", "implicit", true
}

// wireBalanceWarning 恒返回非 nil worker（构造序反转：worker 先建、
// flusher 后建，sink 经 NewFlusher 构造参数注入）。"billing disabled → 无
// worker/无 flusher" 改由 main 的 cfg.Billing.Enabled 分支持有（分支内才调
// wire + NewFlusher，分支外两者均为 nil），wire 层不再表达该语义——旧
// TestWireBalanceWarningReturnsNilWhenBillingSinkAbsent 随 setter 删除而失效；
// disabled 形状仍由 TestOrderedWorkersKeepsEmailAndOmitsWarningWhenBillingDisabled
// 与 TestStatsProvidersOmitsWarningWhenBillingDisabled 在 orderedWorkers(nil, nil)
// 层断言（与 main 禁用分支的 nil warningWorker/billingWorker 同形）。
func TestWireBalanceWarningConstructsWorker(t *testing.T) {
	svc := &fakeBalanceWarningService{enabled: true}

	warningWorker := wireBalanceWarning(notification.NewCooldown(newAssemblyRedis(t)), svc, nil, nil)

	require.NotNil(t, warningWorker)
	require.Equal(t, "notification", warningWorker.Name())
}

type lifecycleWorker struct {
	name   string
	events *[]string
	mu     *sync.Mutex
}

func (w *lifecycleWorker) Name() string { return w.name }

func (w *lifecycleWorker) Start(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.events = append(*w.events, "start:"+w.name)
	return nil
}

func (w *lifecycleWorker) Close(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.events = append(*w.events, "close:"+w.name)
	return nil
}

func newAssemblyRedis(t *testing.T) *redis.Client {
	t.Helper()
	server, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

func TestBalanceWarningEnabledSuppressesWhenGlobalSwitchDisabled(t *testing.T) {
	svc := &fakeBalanceWarningService{}
	enabled := balanceWarningEnabled(svc)

	require.False(t, enabled())
	require.Zero(t, svc.mailConfigCalls)
}

func TestBalanceWarningEnabledUsesServiceConfigWhenGlobalSwitchEnabled(t *testing.T) {
	svc := &fakeBalanceWarningService{enabled: true}
	enabled := balanceWarningEnabled(svc)

	require.True(t, enabled())
	require.Equal(t, 1, svc.mailConfigCalls)
}

func TestOrderedWorkersShutdownUsageThenBillingThenWarningThenEmail(t *testing.T) {
	var events []string
	var mu sync.Mutex
	email := &lifecycleWorker{name: "email", events: &events, mu: &mu}
	warning := &lifecycleWorker{name: "notification", events: &events, mu: &mu}
	billingWorker := &lifecycleWorker{name: "billing", events: &events, mu: &mu}
	usage := &lifecycleWorker{name: "usage", events: &events, mu: &mu}
	manager := worker.New(nil)
	require.NoError(t, manager.Register(orderedWorkers(email, warning, billingWorker, usage)...))
	require.NoError(t, manager.StartAll(context.Background()))

	require.NoError(t, manager.Shutdown(context.Background()))

	require.Equal(t, []string{
		"start:email", "start:notification", "start:billing", "start:usage",
		"close:usage", "close:billing", "close:notification", "close:email",
	}, events)
}

func TestOrderedWorkersKeepsEmailAndOmitsWarningWhenBillingDisabled(t *testing.T) {
	email := &lifecycleWorker{name: "email", events: &[]string{}, mu: &sync.Mutex{}}
	usage := &lifecycleWorker{name: "usage", events: &[]string{}, mu: &sync.Mutex{}}

	workers := orderedWorkers(email, nil, nil, usage)

	require.Len(t, workers, 2)
	require.Equal(t, "email", workers[0].Name())
	require.Equal(t, "usage", workers[1].Name())
}

func TestStatsProvidersMakesConditionalWarningVisibleToOps(t *testing.T) {
	warningWorker := wireBalanceWarning(notification.NewCooldown(newAssemblyRedis(t)), &fakeBalanceWarningService{enabled: true}, nil, nil)
	email := &lifecycleWorker{name: "email", events: &[]string{}, mu: &sync.Mutex{}}
	workers := orderedWorkers(email, warningWorker, nil)
	api := handler.New(nil, handler.OpsOptions{Workers: statsProviders(workers, nil)})
	request := httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	response := httptest.NewRecorder()

	api.Router().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var body handler.WorkersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Workers, 1)
	require.Equal(t, "notification", body.Workers[0].Name)
}

func TestStatsProvidersOmitsWarningWhenBillingDisabled(t *testing.T) {
	email := &lifecycleWorker{name: "email", events: &[]string{}, mu: &sync.Mutex{}}

	providers := statsProviders(orderedWorkers(email, nil, nil), nil)

	require.Empty(t, providers)
}
