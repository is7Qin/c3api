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

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/worker"
)

type fakeBalanceWarningService struct {
	enabled         bool
	mailConfigCalls int
	clearCooldown   func(context.Context, int64, int64) error
}

func (s *fakeBalanceWarningService) BalanceWarningEnabled() bool { return s.enabled }

func (s *fakeBalanceWarningService) MailConfig() (string, int, string, string, string, string, bool) {
	s.mailConfigCalls++
	return "smtp.example.com", 465, "user", "secret", "from@example.com", "implicit", true
}

func (*fakeBalanceWarningService) RenderTemplate(context.Context, domain.EmailTemplatePurpose, map[string]string) (string, string, error) {
	return "subject", "body", nil
}

func (s *fakeBalanceWarningService) SetBalanceWarningCooldownCleaner(clear func(context.Context, int64, int64) error) {
	s.clearCooldown = clear
}

type capturingWarningSinkSetter struct {
	sink billing.BalanceWarningSink
}

func (s *capturingWarningSinkSetter) SetBalanceWarningSink(sink billing.BalanceWarningSink) {
	s.sink = sink
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

func TestWireBalanceWarningReturnsNilWhenBillingSinkAbsent(t *testing.T) {
	svc := &fakeBalanceWarningService{}
	worker := wireBalanceWarning(nil, newAssemblyRedis(t), svc, nil, nil)

	require.Nil(t, worker)
	require.NotNil(t, svc.clearCooldown)
}

func TestWireBalanceWarningConstructsWorkerAndSetsBillingSink(t *testing.T) {
	setter := &capturingWarningSinkSetter{}
	svc := &fakeBalanceWarningService{enabled: true}

	warningWorker := wireBalanceWarning(setter, newAssemblyRedis(t), svc, nil, nil)

	require.NotNil(t, warningWorker)
	require.Same(t, warningWorker, setter.sink)
	require.Equal(t, "notification", warningWorker.Name())
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
	setter := &capturingWarningSinkSetter{}
	warningWorker := wireBalanceWarning(setter, newAssemblyRedis(t), &fakeBalanceWarningService{enabled: true}, nil, nil)
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
