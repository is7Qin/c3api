// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// registerRaceStore 模拟注册并发竞态（真实 DB 唯一约束语义）：
// GetUserByEmail 双 goroutine 全部放行——pre-check 都读到"不存在"，竞态窗口
// 必现；CreateUser 带 email 唯一检查，后到者返回 repository.ErrConflict
// （镜像 user_repo.go CreateUser 对 23505 的映射形态）。
type registerRaceStore struct {
	*fakeStore
	arrived chan struct{} // GetUserByEmail 到达（cap 2，不阻塞）
	release chan struct{} // 双 pre-check 都到后 close 放行
}

func newRegisterRaceStore() *registerRaceStore {
	return &registerRaceStore{
		fakeStore: newFakeStore(),
		arrived:   make(chan struct{}, 2),
		release:   make(chan struct{}),
	}
}

func (r *registerRaceStore) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	r.arrived <- struct{}{}
	<-r.release
	return nil, nil
}

func (r *registerRaceStore) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	// 锁用内嵌 fakeStore 的 f.mu（与 CountUsers 等读同一把锁——独立锁会造成
	// 同 map 异锁的 -race 误报）
	r.fakeStore.mu.Lock()
	defer r.fakeStore.mu.Unlock()
	for _, existing := range r.users {
		if existing.Email == u.Email {
			return nil, fmt.Errorf("%w: email=%q", repository.ErrConflict, u.Email)
		}
	}
	u.ID = r.nextID
	r.nextID++
	c := *u
	r.users[u.ID] = &c
	return &c, nil
}

// TestRegisterUserConcurrentDuplicateEmail 并发注册同邮箱 → 恰一个成功、一个
// service.ErrConflict（409，非 500）：两 goroutine 同时过 pre-check 后一者撞
// DB 唯一冲突——repo 已映射 repository.ErrConflict → 本层 mapRepoErr → 409
// 语义；未映射前是裸 PG 错误 → 500。
func TestRegisterUserConcurrentDuplicateEmail(t *testing.T) {
	fs := newRegisterRaceStore()
	// New 直接装配（newSnapshotSvc 收 *fakeStore，raceStore 是嵌入式 store）：
	// settings 快照默认 signup_enabled=true、temp_balance=0（不插赠品行）。
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	ctx := context.Background()

	const email = "race@example.com"
	results := make([]*domain.User, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.RegisterUser(ctx, email, "s3cret-pass")
		}(i)
	}
	// 等两个 pre-check（GetUserByEmail）都到达 → 放行 → 双插入竞态必现
	<-fs.arrived
	<-fs.arrived
	close(fs.release)
	wg.Wait()

	ok := 0
	for i := range results {
		switch {
		case errs[i] == nil:
			ok++
			require.NotNil(t, results[i], "成功者必有用户")
			require.Equal(t, email, results[i].Email)
		case errors.Is(errs[i], ErrConflict):
			require.Nil(t, results[i])
		default:
			t.Fatalf("unexpected error type: %v", errs[i])
		}
	}
	require.Equal(t, 1, ok, "恰一个注册成功（另一并发者 409，非 500）")
}
