// SPDX-License-Identifier: AGPL-3.0-or-later
package worker

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/is7qin/c3api/pkg/logx"
)

// loopRestartDelay is the fixed delay before restarting a panicked loop.
// var (not const) for test injection.
var loopRestartDelay = 5 * time.Second

// Loop runs fn in the current goroutine with panic containment and restart.
// On panic: logs Error with worker name + stack, waits loopRestartDelay
// (respecting ctx cancellation), then restarts fn. If fn returns normally
// or ctx is cancelled during delay, Loop exits.
func Loop(ctx context.Context, name string, log *logx.Logger, fn func(context.Context)) {
	for {
		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
					if log != nil {
						log.Error("worker loop panicked, restarting",
							logx.String("worker", name),
							logx.Any("panic", r),
							logx.String("stack", string(debug.Stack())),
						)
					}
				}
			}()
			fn(ctx)
		}()
		if !panicked {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(loopRestartDelay):
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// GoLoop spawns Loop in a new goroutine.
func GoLoop(ctx context.Context, name string, log *logx.Logger, fn func(context.Context)) {
	go Loop(ctx, name, log, fn)
}

// Recover runs fn with one-shot panic recovery and error log (no restart).
func Recover(name string, log *logx.Logger, fn func()) {
	defer func() {
		if r := recover(); r != nil && log != nil {
			log.Error("worker goroutine panicked",
				logx.String("worker", name),
				logx.Any("panic", r),
				logx.String("stack", string(debug.Stack())),
			)
		}
	}()
	fn()
}

// GoRecover spawns fn in a new goroutine with one-shot panic recovery (no restart).
func GoRecover(name string, log *logx.Logger, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil && log != nil {
				log.Error("worker goroutine panicked",
					logx.String("worker", name),
					logx.Any("panic", r),
					logx.String("stack", string(debug.Stack())),
				)
			}
		}()
		fn()
	}()
}

// CatchPanic recovers from panic in the caller goroutine and logs Error.
// Usage: defer worker.CatchPanic("name", log)
func CatchPanic(name string, log *logx.Logger) {
	if r := recover(); r != nil && log != nil {
		log.Error("worker goroutine panicked",
			logx.String("worker", name),
			logx.Any("panic", r),
			logx.String("stack", string(debug.Stack())),
		)
	}
}
