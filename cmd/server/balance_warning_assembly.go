// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
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
}

type balanceWarningSinkSetter interface {
	SetBalanceWarningSink(billing.BalanceWarningSink)
}

// wireBalanceWarning 组装余额预警 worker：cooldown 由 main 构造期一次建好
// （ServiceDeps 与此共用同一实例），此处只做 worker 接线。
func wireBalanceWarning(setter balanceWarningSinkSetter, cooldown *notification.Cooldown, svc balanceWarningService, mailW *service.MailWorker, log *logx.Logger) *notification.Worker {
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
