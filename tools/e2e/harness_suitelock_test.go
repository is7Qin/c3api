// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// host 级 e2e 套件锁单测（不带 e2e build tag => 普通 test job 跑；spec §16 AV-γ 的
// fast-fail / 死 owner 接管 / 坏记录不接管 / creation-time 校验面）。全部在 t.TempDir
// 注入路径上操作，绝不触碰真实 host 锁（e2eSuiteLockDirOverride）。
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// withSuiteLockDir 把锁目录注入到 t.TempDir（测试结束自动还原，不碰真实 host 锁）。
func withSuiteLockDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), e2eSuiteLockDirName)
	prev := e2eSuiteLockDirOverride
	e2eSuiteLockDirOverride = dir
	t.Cleanup(func() { e2eSuiteLockDirOverride = prev })
	return dir
}

// suiteLockTestHostname 本机 hostname（锁记录判活依赖同 host 判定）。
func suiteLockTestHostname(t *testing.T) string {
	t.Helper()
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "unknown-host"
	}
	return hostname
}

// certainDeadPID 取一个「平台确定已死」的 PID（不 exec 任何进程：普通 test job 的
// 边界 gate 禁止场景 exec；高 PID 在 Windows/Unix 上都不存在）。
func certainDeadPID(t *testing.T) int {
	t.Helper()
	for _, pid := range []int{1 << 30, 1 << 29, 999999, 888888} {
		alive, certain := procAlive(pid)
		if certain && !alive {
			return pid
		}
	}
	t.Skip("本平台无法确定一个已死 PID（procAlive 均返回不确定）")
	return 0
}

// TestSuiteLockAcquireRelease 基本生命周期：抢锁 => 目录+owner.json（pid/hostname/
// start/creation-time/args）=> 释放 => 目录消失 => 可再次抢到。
func TestSuiteLockAcquireRelease(t *testing.T) {
	dir := withSuiteLockDir(t)

	lock, err := acquireE2ESuiteLock()
	require.NoError(t, err, "首次抢锁必须成功")
	require.False(t, lock.tookOver, "空锁目录首次抢锁不应是接管")

	require.DirExists(t, dir, "锁目录必须存在")
	owner, rerr := readSuiteLockOwnerAt(dir)
	require.NoError(t, rerr, "锁记录必须可读")
	require.Equal(t, os.Getpid(), owner.PID, "锁记录必须记录本测试二进制 pid")
	require.Equal(t, suiteLockTestHostname(t), owner.Hostname, "锁记录必须记录本机 hostname")
	require.False(t, owner.Start.IsZero(), "锁记录必须记录 start")
	require.NotEmpty(t, owner.Args, "锁记录应记录测试二进制参数（诊断用）")
	if ct, ok := procCreationTime(os.Getpid()); ok {
		require.Equal(t, ct, owner.CreationTime, "锁记录必须记录本进程 creation_time（PID 复用安全）")
	}

	require.NoError(t, lock.Release(), "释放必须成功")
	require.NoDirExists(t, dir, "释放后锁目录必须消失")

	again, aerr := acquireE2ESuiteLock()
	require.NoError(t, aerr, "释放后必须能再次抢到")
	require.NoError(t, again.Release())
}

// TestSuiteLockBusyFastFail owner 存活 => 立即 fast-fail（不等待/不重试），锁目录与记录
// 原样保留（不污染当前持有者）。
func TestSuiteLockBusyFastFail(t *testing.T) {
	dir := withSuiteLockDir(t)
	first, err := acquireE2ESuiteLock()
	require.NoError(t, err)

	// 把一个「同 host + 本进程存活」的 owner 记录写回（模拟第一个套件仍在运行）。
	liveOwner := &suiteLockOwner{PID: os.Getpid(), Hostname: suiteLockTestHostname(t), Start: time.Now()}
	require.NoError(t, writeSuiteLockOwnerAt(dir, liveOwner))

	begin := time.Now()
	_, aerr := acquireE2ESuiteLock()
	elapsed := time.Since(begin)
	require.Error(t, aerr, "owner 存活时第二个套件必须抢锁失败")
	require.Contains(t, aerr.Error(), "已有 e2e 套件在运行", "fast-fail 文案必须明确可诊断：%v", aerr)
	require.Contains(t, aerr.Error(), "fast-fail", "错误必须说明不许等待/重试")
	require.Less(t, elapsed, 2*time.Second, "抢锁失败必须立即返回（绝不等待/重试），实测 %s", elapsed)
	require.DirExists(t, dir, "fast-fail 不得删除当前持有者的锁目录")

	// 第一个持有者的锁记录未被改写，可正常释放。
	require.NoError(t, first.Release())
}

// TestSuiteLockCorruptRecordNoTakeover 坏记录（JSON 损坏 / owner.json 缺失）一律不接管，
// 保留目录并要求人工处置。
func TestSuiteLockCorruptRecordNoTakeover(t *testing.T) {
	t.Run("garble", func(t *testing.T) {
		dir := withSuiteLockDir(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, e2eSuiteLockOwnerFile), []byte("{not-json"), 0o644))

		_, err := acquireE2ESuiteLock()
		require.Error(t, err, "坏记录必须导致 fast-fail")
		require.Contains(t, err.Error(), "坏记录不接管", "错误必须声明坏记录不接管：%v", err)
		require.DirExists(t, dir, "坏记录不得被接管/删除")
	})

	t.Run("missing_owner", func(t *testing.T) {
		dir := withSuiteLockDir(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))

		_, err := acquireE2ESuiteLock()
		require.Error(t, err, "缺失 owner.json 必须 fast-fail")
		require.Contains(t, err.Error(), "坏记录不接管", "错误必须声明坏记录不接管：%v", err)
		require.DirExists(t, dir, "缺记录的锁目录不得被接管/删除")
	})
}

// TestSuiteLockDeadOwnerTakeover owner 同 host + PID 确定死亡 => 保守接管（陈旧锁被替换，
// 新 owner 是本进程）。
func TestSuiteLockDeadOwnerTakeover(t *testing.T) {
	dir := withSuiteLockDir(t)
	deadPID := certainDeadPID(t)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	stale := &suiteLockOwner{
		PID:      deadPID,
		Hostname: suiteLockTestHostname(t),
		Start:    time.Now().Add(-time.Hour),
	}
	require.NoError(t, writeSuiteLockOwnerAt(dir, stale))

	lock, err := acquireE2ESuiteLock()
	require.NoError(t, err, "死 owner 的陈旧锁必须可保守接管")
	require.True(t, lock.tookOver, "必须标记为接管")
	require.NotNil(t, lock.staleOwner)
	require.Equal(t, deadPID, lock.staleOwner.PID, "接管记录应保留原 owner pid")

	owner, rerr := readSuiteLockOwnerAt(dir)
	require.NoError(t, rerr)
	require.Equal(t, os.Getpid(), owner.PID, "接管后 owner 必须是本进程")
	require.NoError(t, lock.Release())
	require.NoDirExists(t, dir)
}

// TestSuiteLockPIDReuseTakeover 记录 PID 存活但 creation-time 不一致（PID 被复用）=> 原
// owner 确定已死 => 允许接管（creation-time 优先于裸 PID 判活，spec §10.4 口径）。
func TestSuiteLockPIDReuseTakeover(t *testing.T) {
	dir := withSuiteLockDir(t)
	ct, ok := procCreationTime(os.Getpid())
	if !ok || ct == 0 {
		t.Skip("本平台拿不到自身 creation_time，跳过 PID 复用校验")
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	reused := &suiteLockOwner{
		PID:          os.Getpid(),
		Hostname:     suiteLockTestHostname(t),
		Start:        time.Now().Add(-time.Hour),
		CreationTime: ct + 1, // 与真实 creation_time 不同 = PID 被复用
	}
	require.NoError(t, writeSuiteLockOwnerAt(dir, reused))

	// 对照：先证明「不看 creation-time 时同 PID 会被判活」（busy）——这里直接断言接管成功。
	lock, err := acquireE2ESuiteLock()
	require.NoError(t, err, "creation-time 不一致必须判定原 owner 已死并可接管")
	require.True(t, lock.tookOver)
	require.NoError(t, lock.Release())
}

// TestSuiteLockForeignHostNoTakeover 异 host 记录无法判活 => 状态不确定 => 不接管（单机
// 边界，spec §0.3）。
func TestSuiteLockForeignHostNoTakeover(t *testing.T) {
	dir := withSuiteLockDir(t)
	deadPID := certainDeadPID(t)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	stale := &suiteLockOwner{
		PID:      deadPID,
		Hostname: "e2e-other-host.invalid",
		Start:    time.Now().Add(-time.Hour),
	}
	require.NoError(t, writeSuiteLockOwnerAt(dir, stale))

	_, err := acquireE2ESuiteLock()
	require.Error(t, err, "异 host 记录必须 fast-fail（无法证明已死）")
	require.Contains(t, err.Error(), "已有 e2e 套件在运行")
	require.Contains(t, err.Error(), "不确定", "错误必须说明状态不确定：%v", err)
	require.DirExists(t, dir, "异 host 记录不得被接管")
}

// TestSuiteLockStaleClaimTakeover 接管标记：新鲜标记 => fast-fail（另一个接管者正在工作）；
// 陈旧标记（接管者崩溃残留，>2min）=> 删除后单次重抢成功。
func TestSuiteLockStaleClaimTakeover(t *testing.T) {
	deadPID := certainDeadPID(t)
	makeStaleLock := func(t *testing.T) string {
		dir := withSuiteLockDir(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, writeSuiteLockOwnerAt(dir, &suiteLockOwner{
			PID: deadPID, Hostname: suiteLockTestHostname(t), Start: time.Now().Add(-time.Hour),
		}))
		return dir
	}

	t.Run("fresh_claim_fast_fail", func(t *testing.T) {
		dir := makeStaleLock(t)
		claim := dir + e2eSuiteLockClaimSuffix
		require.NoError(t, os.Mkdir(claim, 0o755))

		begin := time.Now()
		_, err := acquireE2ESuiteLock()
		require.Error(t, err, "新鲜接管标记存在时必须 fast-fail（不并发接管）")
		require.Contains(t, err.Error(), "接管标记已存在")
		require.Less(t, time.Since(begin), 2*time.Second, "不得等待/重试")
		require.DirExists(t, claim, "新鲜接管标记不得被删除")
	})

	t.Run("stale_claim_takeover", func(t *testing.T) {
		dir := makeStaleLock(t)
		claim := dir + e2eSuiteLockClaimSuffix
		require.NoError(t, os.Mkdir(claim, 0o755))
		old := time.Now().Add(-e2eSuiteLockClaimStaleAge - time.Minute)
		require.NoError(t, os.Chtimes(claim, old, old))

		lock, err := acquireE2ESuiteLock()
		require.NoError(t, err, "陈旧接管标记必须可被接管（否则崩溃一次永久阻塞）")
		require.True(t, lock.tookOver)
		require.NoDirExists(t, claim, "接管完成后接管标记应被清理")
		require.NoError(t, lock.Release())
	})
}

// TestSuiteLockReleaseRefusesForeignOwner 释放只删「仍属于本进程」的锁：被改写/接管后
// 拒绝删除（活 run 永不被回收）。
func TestSuiteLockReleaseRefusesForeignOwner(t *testing.T) {
	dir := withSuiteLockDir(t)
	lock, err := acquireE2ESuiteLock()
	require.NoError(t, err)

	foreign := &suiteLockOwner{
		PID:      certainDeadPID(t),
		Hostname: suiteLockTestHostname(t),
		Start:    time.Now(),
	}
	require.NoError(t, writeSuiteLockOwnerAt(dir, foreign))

	rerr := lock.Release()
	require.Error(t, rerr, "owner 不是本进程时必须拒绝删除")
	require.Contains(t, rerr.Error(), "拒绝删除")
	require.DirExists(t, dir, "拒绝删除后锁目录必须保留")

	// 还原成本进程记录后可正常释放（便于测试清理）。
	require.NoError(t, writeSuiteLockOwnerAt(dir, newSuiteLockOwner()))
	require.NoError(t, lock.Release())
}
