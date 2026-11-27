// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：registry 状态机 / Redis 索引分配 / stale sweep 的判定逻辑
// 在普通 test job 直接单测（spec §10.7 平台 helper 同口径；AV-A/AV-B 的最小机读验证）。
//
// 隔离手段：e2eRegistryDirOverride 指向 t.TempDir()，**绝不触碰真实 registry**；
// 需要 PG 的回收路径通过 dropStaleDatabase 注入点替换，普通 test job 不依赖 PG。
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// useTempRegistry 把 registry 根目录切到测试私有目录（结束后恢复）。
func useTempRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := e2eRegistryDirOverride
	e2eRegistryDirOverride = dir
	t.Cleanup(func() { e2eRegistryDirOverride = prev })
	require.NoError(t, e2eRegistryEnsureDir())
	return dir
}

// deadPIDForTest 一个「确定死亡」的 PID（普通 test job 不 spawn 进程：取超出系统
// 进程 table 的 PID——Windows OpenProcess ERROR_INVALID_PARAMETER / Unix ESRCH）。
func deadPIDForTest(t *testing.T) int {
	t.Helper()
	const pid = 1 << 21 // 2,097,152：超过 Windows/Linux 常规进程 id 上限
	alive, certain := procAlive(pid)
	require.True(t, certain, "伪死 PID 的状态必须确定")
	require.False(t, alive, "伪死 PID 不应存活（pid=%d）", pid)
	return pid
}

// writeRecordForTest 写一条 registry 记录（helper）。
func writeRecordForTest(t *testing.T, rec *runRecord) {
	t.Helper()
	require.NoError(t, saveRunRecord(rec))
}

// newTestRecord 构造最小记录（无真实资源；PGDB 为空 => sweep 不碰 PG）。
func newTestRecord(t *testing.T, state runState, ownerPID int, start time.Time) *runRecord {
	t.Helper()
	uuid, err := newRunUUID()
	require.NoError(t, err)
	hostname, err := os.Hostname()
	require.NoError(t, err)
	return &runRecord{
		UUID:             uuid,
		PID:              ownerPID,
		Hostname:         hostname,
		Start:            start,
		State:            state,
		RedisDB:          -1,
		AllocatedRedisDB: -1,
	}
}

// TestRunRecordStateMachine registry 状态机：合法迁移 + 原子落盘 + 非法迁移拒绝。
func TestRunRecordStateMachine(t *testing.T) {
	useTempRegistry(t)
	rec := newTestRecord(t, stateStarting, os.Getpid(), time.Now())
	writeRecordForTest(t, rec)

	// 合法链：starting -> resources_ready -> spawning -> ready -> finishing。
	require.NoError(t, transitionRunRecord(rec, stateResourcesReady))
	require.NoError(t, transitionRunRecord(rec, stateSpawning))
	require.NoError(t, transitionRunRecord(rec, stateReady))
	require.NoError(t, transitionRunRecord(rec, stateFinishing))

	// 非法迁移必须报错（启动期编程错误立即暴露）。
	require.Error(t, transitionRunRecord(rec, stateStarting), "finishing 后不得回到 starting")
	require.Error(t, transitionRunRecord(rec, stateReady), "finishing 后不得再进入 ready")

	// 磁盘内容与内存一致（temp+rename 原子写）。
	onDisk, err := readRunRecord(runRecordPath(rec.UUID))
	require.NoError(t, err)
	require.Equal(t, stateFinishing, onDisk.State)
	require.Equal(t, rec.UUID, onDisk.UUID)
}

// TestRedisIndexAllocationUnique 并发抢占 15 个逻辑库：无共享（AV-B 分配器压力最小版）。
func TestRedisIndexAllocationUnique(t *testing.T) {
	useTempRegistry(t)
	const n = e2eRedisDBMax - e2eRedisDBMin + 1 // 15
	var mu sync.Mutex
	got := make([]int, 0, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			uuid, err := newRunUUID()
			if err != nil {
				errs[idx] = err
				return
			}
			db, err := allocRedisIndex(uuid, 0)
			if err != nil {
				errs[idx] = err
				return
			}
			mu.Lock()
			got = append(got, db)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "第 %d 次分配失败", i)
	}
	require.Len(t, got, n)
	uniq := map[int]bool{}
	for _, db := range got {
		require.False(t, uniq[db], "Redis 逻辑库 %d 被重复分配", db)
		uniq[db] = true
	}
	// 16 个并发 run 超过 Redis 16 库上限：必须明确报错（R1）。
	uuid, err := newRunUUID()
	require.NoError(t, err)
	_, err = allocRedisIndex(uuid, 0)
	require.Error(t, err, "第 16 个并发 run 必须明确报错")
	require.Contains(t, err.Error(), "已全部被占用")
}

// TestRedisIndexManualOverride 手动覆盖（E2E_REDIS_DB）必须独占；shared 下同一库
// 不能被第二个 run 抢到。
func TestRedisIndexManualOverride(t *testing.T) {
	useTempRegistry(t)
	uuid1, err := newRunUUID()
	require.NoError(t, err)
	db, err := allocRedisIndex(uuid1, 3)
	require.NoError(t, err)
	require.Equal(t, 3, db)
	lock, err := readRedisLock(3)
	require.NoError(t, err)
	require.NotNil(t, lock)
	require.True(t, lock.ManualOverride, "手动覆盖必须记录 manual_override=true")

	uuid2, err := newRunUUID()
	require.NoError(t, err)
	_, err = allocRedisIndex(uuid2, 3)
	require.Error(t, err, "同一逻辑库不能被第二个 run 抢到")
	require.Contains(t, err.Error(), "已被占用")

	// 非法覆盖值：0（留生产）与 16（不存在）都必须明确拒绝。
	_, err = allocRedisIndex(uuid2, 0)
	require.NoError(t, err, "0 = 未覆盖（自动分配）语义")
	// 释放手动覆盖的 3 后可再次手动占用。
	require.NoError(t, releaseRedisIndex(3, uuid1))
	_, err = allocRedisIndex(uuid2, 3)
	require.NoError(t, err)

	// owner 不匹配 => 拒绝删除（活 run 的锁永不被误删）。
	require.Error(t, releaseRedisIndex(3, uuid1), "owner uuid 不匹配时拒绝释放")
}

// TestSweepCASOnlyOneWinner 并发 sweeper：同一条死记录只被回收一次（CAS 独占）。
func TestSweepCASOnlyOneWinner(t *testing.T) {
	useTempRegistry(t)
	dropStaleDatabase = func(string) error { return nil } // 记录无 PG 库，仍注入以证明调用面
	t.Cleanup(func() { dropStaleDatabase = realDropStaleDatabase })

	rec := newTestRecord(t, stateReady, deadPIDForTest(t), time.Now().Add(-time.Hour))
	rec.ChildPIDs = []childProcess{{PID: deadPIDForTest(t), Kind: "gateway"}}
	writeRecordForTest(t, rec)

	var mu sync.Mutex
	reclaimed := 0
	logf := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		t.Logf("sweep: %s", msg)
		if contains(msg, "已回收 run") {
			mu.Lock()
			reclaimed++
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sweepStaleRuns(logf)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, reclaimed, "CAS 保证同一条记录只被一个 sweeper 回收")
	require.False(t, harnessFileExists(runRecordPath(rec.UUID)), "记录文件应已删除")
}

// contains 子串判定（避免在测试里引入 strings 之外的 helper 依赖）。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSweepConservativePolicy 保守回收规则（宁可泄漏不可误删）：
//   - 记录 PID 存活 => 永不回收（无论 age/状态）；
//   - 坏记录/异 host => 永不回收（仅 warn）；
//   - starting/services_up/spawning => 只 warn 保留（D7）；
//   - resources_ready 已死但未满 2min 或仍有 child 登记 => 保留；
//   - resources_ready 已死 + 无 child + 锁 owner 匹配 => 回收。
func TestSweepConservativePolicy(t *testing.T) {
	useTempRegistry(t)
	var mu sync.Mutex
	var warnLines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		warnLines = append(warnLines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// a) 活 PID + 旧时间戳 => 保留。
	live := newTestRecord(t, stateReady, os.Getpid(), time.Now().Add(-time.Hour))
	writeRecordForTest(t, live)

	// b) 坏记录（JSON 损坏）=> 保留 + corrupt 计数。
	corruptPath := runRecordPath("corrupt-" + time.Now().Format("150405.000000000"))
	require.NoError(t, writeFileAtomic(corruptPath, []byte("{not-json")))

	// c) spawning（未进入可回收状态）+ 死 PID + 旧时间戳 => 保留（D7）。
	spawning := newTestRecord(t, stateSpawning, deadPIDForTest(t), time.Now().Add(-time.Hour))
	writeRecordForTest(t, spawning)

	// d) resources_ready 已死但 age 不足 => 保留。
	young := newTestRecord(t, stateResourcesReady, deadPIDForTest(t), time.Now().Add(-time.Second))
	writeRecordForTest(t, young)

	// e) resources_ready 已死但仍有 child 登记 => 保留。
	withChild := newTestRecord(t, stateResourcesReady, deadPIDForTest(t), time.Now().Add(-time.Hour))
	withChild.ChildPIDs = []childProcess{{PID: deadPIDForTest(t)}}
	writeRecordForTest(t, withChild)

	stats := sweepStaleRuns(logf)
	require.GreaterOrEqual(t, stats.Corrupt, 1, "坏记录必须计入 corrupt")
	for _, rec := range []*runRecord{live, spawning, young, withChild} {
		require.True(t, harnessFileExists(runRecordPath(rec.UUID)), "记录 %s（state=%s）应被保留", rec.UUID, rec.State)
	}
	require.True(t, harnessFileExists(corruptPath), "坏记录永不回收")

	// f) resources_ready 已死 + 无 child + 锁 owner 匹配 => 回收（跳过 PG：无库）。
	reclaim := newTestRecord(t, stateResourcesReady, deadPIDForTest(t), time.Now().Add(-time.Hour))
	require.NoError(t, writeRedisLockExclusive(7, redisLock{UUID: reclaim.UUID, PID: reclaim.PID, Start: reclaim.Start}))
	reclaim.RedisDB = 7
	reclaim.AllocatedRedisDB = 7
	reclaim.LockUUID = reclaim.UUID
	writeRecordForTest(t, reclaim)
	sweepStaleRuns(logf)
	require.False(t, harnessFileExists(runRecordPath(reclaim.UUID)), "resources_ready 已死 + 锁匹配 => 回收")
	lock, err := readRedisLock(7)
	require.NoError(t, err)
	require.Nil(t, lock, "回收时删除 owner 匹配的 Redis 锁")

	// g) resources_ready + 锁 owner 不匹配 => 保守保留（可能已被别的 run 接管：不 DROP
	// 库、不删锁、不回收记录；spec §9.4「不匹配只 warn 不删」）。
	mismatch := newTestRecord(t, stateResourcesReady, deadPIDForTest(t), time.Now().Add(-time.Hour))
	require.NoError(t, writeRedisLockExclusive(8, redisLock{UUID: "other-owner", PID: 1, Start: time.Now()}))
	mismatch.RedisDB = 8
	mismatch.AllocatedRedisDB = 8
	mismatch.LockUUID = mismatch.UUID
	writeRecordForTest(t, mismatch)
	sweepStaleRuns(logf)
	require.True(t, harnessFileExists(runRecordPath(mismatch.UUID)), "锁 owner 不匹配 => 记录保守保留")
	otherLock, err := readRedisLock(8)
	require.NoError(t, err)
	require.NotNil(t, otherLock, "owner uuid 不匹配的锁只 warn 不删")
	require.Equal(t, "other-owner", otherLock.UUID)
}

// TestReapingTakeoverCrashedSweeper .reaping.*（sweeper 崩溃残留）age>5min 可接管；
// 新鲜 .reaping 不接管（防误删活 sweeper）。
func TestReapingTakeoverCrashedSweeper(t *testing.T) {
	useTempRegistry(t)
	rec := newTestRecord(t, stateReady, deadPIDForTest(t), time.Now().Add(-time.Hour))
	hostname, err := os.Hostname()
	require.NoError(t, err)
	rec.Hostname = hostname
	reapingPath := runRecordPath(rec.UUID) + e2eReapingInfix + "other-sweeper"
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, writeFileAtomic(reapingPath, data))
	// 新鲜 .reaping：不接管。
	sweepStaleRuns(func(string, ...any) {})
	require.True(t, harnessFileExists(reapingPath), "新鲜 .reaping 不得被接管")
	// 旧 .reaping（>5min）：接管并回收。
	old := time.Now().Add(-10 * time.Minute)
	require.NoError(t, os.Chtimes(reapingPath, old, old))
	sweepStaleRuns(func(string, ...any) {})
	require.False(t, harnessFileExists(reapingPath), "崩溃 sweeper 的 .reaping 记录 age>5min 应被接管回收")
}

// TestRunTempDirValidation 回收删除路径前的严格校验（绝不 RemoveAll 任意路径）。
func TestRunTempDirValidation(t *testing.T) {
	tmp, err := os.MkdirTemp("", e2eRunTmpDirPrefix+"selftest-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	require.True(t, runTempDirValid(tmp), "harness 自己创建的 temp 目录必须通过校验")
	require.False(t, runTempDirValid(os.TempDir()), "OS temp 根本身不得被删除")
	require.False(t, runTempDirValid(""), "空路径不得通过")
	require.False(t, runTempDirValid(t.TempDir()), "非 e2e-run- 前缀目录不得通过")
}
