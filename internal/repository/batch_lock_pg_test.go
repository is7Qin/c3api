// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// batch_lock_pg_test.go N1 归属锁集合协议在真实 PG 上的**并发无死锁**回归：多个
// 转属/配置写线程对同一批账号交错写，users→accounts 固定锁序必须永不触发 40P01
// （deadlock detected），全部收敛成功。
//
// 跑法：TEST_DATABASE_URL=... go test ./internal/repository/ -run 'PGAccountOwnershipNoDeadlock' -count=1 -v

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

// TestPGAccountOwnershipNoDeadlock N1（可重试收敛）：并发「转属 + 普通配置写」下，
// 锁序恒为 users（升序）→ accounts，绝不出现 accounts→users 反序补锁，故无 40P01。
func TestPGAccountOwnershipNoDeadlock(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a := seedSupplierUser(t, repos, "nd-a@example.com")
	b := seedSupplierUser(t, repos, "nd-b@example.com")

	ids := make([]int64, 0, 4)
	for _, name := range []string{"nd-1", "nd-2", "nd-3", "nd-4"} {
		acc := seedOwnedAccount(t, repos, tpl.ID, a.ID, name)
		ids = append(ids, acc.ID)
	}

	const iters = 15
	var wg sync.WaitGroup
	errCh := make(chan error, len(ids)*iters*3)
	for _, id := range ids {
		for i := 0; i < iters; i++ {
			wg.Add(3)
			accID := id
			go func() { // 转属 → B
				defer wg.Done()
				_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{accID}, repository.AccountPatch{SupplierUserID: &b.ID})
				errCh <- err
			}()
			go func() { // 转属 → A
				defer wg.Done()
				_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{accID}, repository.AccountPatch{SupplierUserID: &a.ID})
				errCh <- err
			}()
			go func() { // 普通配置写（不改归属）
				defer wg.Done()
				off := false
				_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{accID}, repository.AccountPatch{Enabled: &off})
				errCh <- err
			}()
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err == nil {
			continue
		}
		msg := err.Error()
		require.False(t, strings.Contains(msg, "40P01") || strings.Contains(strings.ToLower(msg), "deadlock"),
			"归属/配置并发写不得死锁：%s", msg)
		require.NoError(t, err, "并发写应全部收敛成功")
	}
}
