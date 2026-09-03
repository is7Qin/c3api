// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// 回归（上机压测发现）：Reload 在锁外迭代 keys map（= a.keys 当前引用），
// 与 Upsert/Delete 并发写同 map → fatal "concurrent map iteration and map
// write"（128 并发建用户触发，进程崩溃）。修复：gate.reload 移入 a.mu 锁内。
// 本测试在 go test -race 下并发打 Reload/Upsert/Delete——修复前 fatal，
// 修复后无竞态通过。
func TestAuthReloadConcurrentUpsert(t *testing.T) {
	loader := &mutKeyLoader{keys: make(map[string]domain.KeyMeta)}
	a := NewAuth(loader, noopUserLoader{}, nil, true)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写者：并发 Upsert/Delete（模拟多管理请求同时改 key）
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				hash := fmt.Sprintf("race-h%d-%d", w, i)
				a.Upsert(hash, activeKey(int64(i%1000), int64(1+i%50), 10))
				if i%3 == 0 {
					a.Delete(hash)
				}
			}
		}(w)
	}
	// 重载者：Reload（loader 每次返回全新 map）
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.Reload(context.Background())
			}
		}()
	}

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 修复后应能完整跑完（未 fatal）
}

// mutKeyLoader 可变 key 数据源：Reload 每次返回新 map（与生产 LoadKeys 同语义）。
type mutKeyLoader struct {
	mu   sync.Mutex
	keys map[string]domain.KeyMeta
}

func (l *mutKeyLoader) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]domain.KeyMeta, len(l.keys))
	for k, v := range l.keys {
		out[k] = v
	}
	return out, nil
}
