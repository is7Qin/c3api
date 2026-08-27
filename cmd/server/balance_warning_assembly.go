// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/notification"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

type balanceWarningService interface {
	BalanceWarningEnabled() bool
	MailConfig() (host string, port int, username, password, fromAddr, tlsPolicy string, ok bool)
	SetBalanceWarningCooldownCleaner(func(context.Context, int64, int64) error)
}

type balanceWarningSinkSetter interface {
	SetBalanceWarningSink(billing.BalanceWarningSink)
}

func wireBalanceWarning(setter balanceWarningSinkSetter, rdb *redis.Client, svc balanceWarningService, mailW *service.MailWorker, log *logx.Logger) *notification.Worker {
	cooldown := notification.NewCooldown(rdb)
	svc.SetBalanceWarningCooldownCleaner(cooldown.Clear)
	if setter == nil {
		return nil
	}
	var enqueue notification.WarningMailEnqueue
	if mailW != nil {
		enqueue = mailW.EnqueueBalanceWarning
	}
	warningWorker := notification.New(cooldown, balanceWarningEnabled(svc), enqueue, log)
	setter.SetBalanceWarningSink(warningWorker)
	return warningWorker
}

func balanceWarningEnabled(svc balanceWarningService) func() bool {
	return func() bool {
		if !svc.BalanceWarningEnabled() {
			return false
		}
		_, _, _, _, _, _, ok := svc.MailConfig()
		return ok
	}
}

func orderedWorkers(email, warning, billingWorker worker.Worker, remaining ...worker.Worker) []worker.Worker {
	workers := make([]worker.Worker, 0, 3+len(remaining))
	workers = append(workers, email)
	if warning != nil {
		workers = append(workers, warning)
	}
	if billingWorker != nil {
		workers = append(workers, billingWorker)
	}
	return append(workers, remaining...)
}

func statsProviders(candidates []worker.Worker, log *logx.Logger) []handler.StatsProvider {
	providers := make([]handler.StatsProvider, 0, len(candidates))
	for _, candidate := range candidates {
		provider, ok := candidate.(handler.StatsProvider)
		if ok {
			providers = append(providers, provider)
			continue
		}
		if log != nil {
			log.Warn("worker does not implement StatsProvider, missing from /api/admin/ops/workers",
				logx.String("worker", candidate.Name()))
		}
	}
	return providers
}
