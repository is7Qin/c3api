// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/internal/domain"
)

// errProbeBusy 是准入队列满的内部信号；对外只映射固定 probe busy 文案。
var errProbeBusy = errors.New("service: probe admission full")

// probeLimiter 是探测的控制面准入：4 个执行槽 + 16 个等待位，均在仓储读取
// 之前获取。执行槽在探测全程持有；等待位只在排队期间持有，晋升后立即释放。
// 两个 channel 无锁；Service 每实例一个。
type probeLimiter struct {
	slots   chan struct{}
	waiters chan struct{}
}

func newProbeLimiter() *probeLimiter {
	return &probeLimiter{
		slots:   make(chan struct{}, domain.ProbeMaxConcurrency),
		waiters: make(chan struct{}, domain.ProbeMaxWaiters),
	}
}

// acquire 返回释放执行槽的函数。队列满返回 errProbeBusy；ctx 取消/过期优先
// 返回 ctx.Err()。等待期间 ctx 到期会释放等待位；晋升后但 ctx 已到期会释放
// 执行槽，不把过期请求带入仓储/上游。
func (l *probeLimiter) acquire(ctx context.Context) (release func(), err error) {
	if l == nil {
		return nil, errors.New("service: probe limiter not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
		// ctx 可能在初始检查与拿槽之间到期：拿到槽后必须复核，不能把
		// 已取消请求放进仓储/上游。
		if err := ctx.Err(); err != nil {
			<-l.slots
			return nil, err
		}
		return l.releaseSlot, nil
	default:
	}
	select {
	case l.waiters <- struct{}{}:
	default:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errProbeBusy
	}
	select {
	case l.slots <- struct{}{}:
		<-l.waiters
		if err := ctx.Err(); err != nil {
			<-l.slots
			return nil, err
		}
		return l.releaseSlot, nil
	case <-ctx.Done():
		<-l.waiters
		return nil, ctx.Err()
	}
}

func (l *probeLimiter) releaseSlot() { <-l.slots }
