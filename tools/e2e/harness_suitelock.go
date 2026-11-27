// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：套件锁的判定/接管逻辑是纯基础设施，普通 test job 直接
// 单测（harness_suitelock_test.go 用 e2eSuiteLockDirOverride 注入 t.TempDir，绝不触碰
// 真实锁）；TestMain 在 suite_lock_test.go（//go:build e2e）。
//
// host 级 e2e 套件锁（spec §16 裁定，P1 fix3）：
//   - 同一 host 同时只允许一个 e2e 测试二进制（= 一个套件）；锁是**资源边界裁定**，
//     不是并发缺陷的掩盖：并发承诺收窄为「单套件 × host，套件内场景串行」；
//   - 抢不到锁且 owner 存活/状态不确定 => 立即返回清晰错误，TestMain os.Exit(2)，
//     **绝不等待/重试/挂起**（fast-fail，可诊断）；
//   - owner 确定死亡（同 host + PID 确定死亡；creation-time 优先校验，防 PID 复用）
//     => 保守接管；坏记录（JSON 损坏/字段缺失/异 host 不确定）一律不接管；
//   - 锁路径固定（OS temp 下 e2e-suite.lock 目录），内容 owner.json = pid/hostname/
//     start/creation_time/args/exe；正常退出与 panic 都释放（TestMain defer），
//     进程被强杀时由下一个套件的接管路径回收。
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// e2eSuiteLockDirName host 级套件锁根目录名（OS temp 下；同 host 的任意 worktree/
	// 进程共享同一把锁 => 跨 worktree 也互斥）。
	e2eSuiteLockDirName = "e2e-suite.lock"
	// e2eSuiteLockOwnerFile 锁内容文件名（pid/hostname/start/creation_time...）。
	e2eSuiteLockOwnerFile = "owner.json"
	// e2eSuiteLockClaimSuffix 死 owner 接管的 CAS 标记目录后缀：Mkdir 是跨平台原子
	// 独占原语（与 registry sweep 同因：Windows 上并发 rename 同源不可靠）。
	e2eSuiteLockClaimSuffix = ".claim"
	// e2eSuiteLockClaimStaleAge 接管标记视为「接管者崩溃残留」的年龄门槛；超过才允许
	// 删除重抢（有界一次，不循环等待）。
	e2eSuiteLockClaimStaleAge = 2 * time.Minute
)

// e2eSuiteLockDirOverride 单测注入点：非空时覆盖锁目录（普通 test job 只在 t.TempDir
// 上操作，绝不触碰真实 host 锁）。
var e2eSuiteLockDirOverride string

// e2eSuiteLockDir host 级套件锁目录（固定路径）。
func e2eSuiteLockDir() string {
	if e2eSuiteLockDirOverride != "" {
		return e2eSuiteLockDirOverride
	}
	return filepath.Join(os.TempDir(), e2eSuiteLockDirName)
}

// suiteLockOwner 锁内容（内容 pid/hostname/start/creation-time；args/exe 仅诊断用）。
type suiteLockOwner struct {
	PID          int       `json:"pid"`
	Hostname     string    `json:"hostname"`
	Start        time.Time `json:"start"`
	CreationTime uint64    `json:"creation_time,omitempty"`
	Exe          string    `json:"exe,omitempty"`
	Args         []string  `json:"args,omitempty"`
}

// e2eSuiteLock 已持有的套件锁句柄。
type e2eSuiteLock struct {
	dir          string
	creationTime uint64
	// tookOver 本次是否接管了「上一个套件进程已确定死亡」的陈旧锁（仅供诊断输出）。
	tookOver bool
	// staleOwner 被接管的陈旧 owner（tookOver=true 时非 nil）。
	staleOwner *suiteLockOwner
}

// errSuiteLockDirExists 内部哨兵：锁目录已存在（acquire 需转判定路径）。
var errSuiteLockDirExists = errors.New("e2e suite lock dir exists")

// newSuiteLockOwner 生成当前测试二进制进程的 owner 记录。
func newSuiteLockOwner() *suiteLockOwner {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown-host"
	}
	exe, _ := os.Executable()
	creationTime, _ := procCreationTime(os.Getpid())
	return &suiteLockOwner{
		PID:          os.Getpid(),
		Hostname:     hostname,
		Start:        time.Now(),
		CreationTime: creationTime,
		Exe:          exe,
		Args:         append([]string(nil), os.Args...),
	}
}

// validateSuiteLockOwner 记录必须完整（坏记录不接管：宁可 fast-fail 也不误删别人的锁）。
func validateSuiteLockOwner(o *suiteLockOwner) error {
	if o == nil {
		return errors.New("记录为空")
	}
	if o.PID <= 0 {
		return fmt.Errorf("pid=%d 非法", o.PID)
	}
	if strings.TrimSpace(o.Hostname) == "" {
		return errors.New("hostname 缺失")
	}
	if o.Start.IsZero() {
		return errors.New("start 缺失")
	}
	return nil
}

// readSuiteLockOwnerAt 读锁记录（文件不存在返回 fs.ErrNotExist；损坏返回 error）。
func readSuiteLockOwnerAt(dir string) (*suiteLockOwner, error) {
	data, err := os.ReadFile(filepath.Join(dir, e2eSuiteLockOwnerFile))
	if err != nil {
		return nil, err
	}
	var owner suiteLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", e2eSuiteLockOwnerFile, err)
	}
	return &owner, nil
}

// writeSuiteLockOwnerAt 原子写锁记录（先写锁目录内再由调用方 rename；同目录同卷）。
func writeSuiteLockOwnerAt(dir string, owner *suiteLockOwner) error {
	data, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(dir, e2eSuiteLockOwnerFile), data)
}

// suiteLockOwnerAlive 判定 owner 进程状态：返回 (alive, certain)。
//   - 异 host => 状态不确定（不接管：跨 host 无法判活，spec §0.3 单机边界）；
//   - 同 host => 平台 helper 判活；PID 存活且记录带 creation_time 时优先比对
//     creation_time（不一致 = PID 已被复用 => 原 owner 已死，可保守接管）；
//   - 查询被拒/未知错误 => certain=false（保守按活，不接管）。
func suiteLockOwnerAlive(owner *suiteLockOwner) (alive bool, certain bool) {
	hostname, _ := os.Hostname()
	if hostname != "" && !sameHostname(owner.Hostname, hostname) {
		return true, false
	}
	alive, certain = procAlive(owner.PID)
	if !certain {
		return true, false
	}
	if !alive {
		return false, true
	}
	if owner.CreationTime != 0 {
		if ct, ok := procCreationTime(owner.PID); ok && ct != owner.CreationTime {
			// PID 已被复用：记录里的那个进程确定已死（spec §10.4 口径）。
			return false, true
		}
	}
	return true, true
}

// sameHostname host 比较（大小写不敏感；Windows 主机名不区分大小写）。
func sameHostname(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// createSuiteLockDir 原子抢锁：Mkdir 成功 = 成为 owner，随后写 owner.json。
// 目录已存在 => errSuiteLockDirExists（调用方转判定/接管路径）。
func createSuiteLockDir(dir string, owner *suiteLockOwner) (*e2eSuiteLock, error) {
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errSuiteLockDirExists
		}
		return nil, err
	}
	if err := writeSuiteLockOwnerAt(dir, owner); err != nil {
		// 写失败 => 本次没抢到有效所有权，回滚目录（否则留下坏记录阻塞下次）。
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("写套件锁记录失败: %w", err)
	}
	return &e2eSuiteLock{dir: dir, creationTime: owner.CreationTime}, nil
}

// acquireE2ESuiteLock 抢 host 级 e2e 套件锁。**绝不等候/重试/挂起**：抢不到即返回
// 可诊断错误（TestMain 打印后 os.Exit(2)），由调用方决定人工处置。
func acquireE2ESuiteLock() (*e2eSuiteLock, error) {
	dir := e2eSuiteLockDir()
	owner := newSuiteLockOwner()
	lock, err := createSuiteLockDir(dir, owner)
	if err == nil {
		return lock, nil
	}
	if !errors.Is(err, errSuiteLockDirExists) {
		return nil, fmt.Errorf("创建套件锁目录 %s 失败: %w", dir, err)
	}

	// 已存在：读记录判定（坏记录不接管）。
	stale, rerr := readSuiteLockOwnerAt(dir)
	if rerr != nil {
		return nil, corruptSuiteLockError(dir, rerr)
	}
	if verr := validateSuiteLockOwner(stale); verr != nil {
		return nil, corruptSuiteLockError(dir, verr)
	}
	alive, certain := suiteLockOwnerAlive(stale)
	if alive || !certain {
		return nil, busySuiteLockError(dir, stale, certain)
	}
	return takeoverSuiteLock(dir, stale, owner)
}

// takeoverSuiteLock 保守接管「owner 确定死亡」的陈旧锁：先用 Mkdir claim 独占接管权
// （并发接管者只有一个赢家），再删除陈旧锁并重建；失败一律返回错误（不重试等待）。
func takeoverSuiteLock(dir string, stale *suiteLockOwner, owner *suiteLockOwner) (*e2eSuiteLock, error) {
	claim := dir + e2eSuiteLockClaimSuffix
	if err := os.Mkdir(claim, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("创建套件锁接管标记 %s 失败: %w", claim, err)
		}
		// 接管标记已存在：可能是另一个进程正在接管（正常竞争）或接管者崩溃残留。
		// 只有「陈旧（>2min）」才允许删除后重抢一次；绝不循环等待。
		info, serr := os.Stat(claim)
		if serr != nil || time.Since(info.ModTime()) <= e2eSuiteLockClaimStaleAge {
			return nil, fmt.Errorf(`套件锁接管标记已存在（另一个 e2e 进程正在接管陈旧锁，或接管者崩溃残留）：
  锁目录: %s
  接管标记: %s
  本进程立即 fast-fail（不等待/不重试）。若确认无 e2e 进程运行，手工删除上述两个路径后重试。`, dir, claim)
		}
		if rerr := os.Remove(claim); rerr != nil {
			return nil, fmt.Errorf("删除陈旧接管标记 %s 失败: %w", claim, rerr)
		}
		if merr := os.Mkdir(claim, 0o755); merr != nil {
			return nil, fmt.Errorf("重抢套件锁接管标记 %s 失败: %w", claim, merr)
		}
	}
	defer func() { _ = os.Remove(claim) }()

	// 独占接管权后复核：记录必须仍是刚才判定死亡的那一条（防止接管期间被改写）。
	cur, rerr := readSuiteLockOwnerAt(dir)
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			// 锁目录在接管前被清掉了（手工清理/并发释放）：直接按新锁重建。
			return createSuiteLockDir(dir, owner)
		}
		return nil, corruptSuiteLockError(dir, rerr)
	}
	if !sameSuiteLockOwner(cur, stale) {
		return nil, fmt.Errorf(`套件锁记录在接管期间被改写，放弃接管（fast-fail，不重试）：
  锁目录: %s
  判定时: pid=%d host=%s start=%s
  当前:   pid=%d host=%s start=%s
  处置：确认无 e2e 进程运行后手工删除该锁目录再重试。`,
			dir, stale.PID, stale.Hostname, stale.Start, cur.PID, cur.Hostname, cur.Start)
	}
	if alive, certain := suiteLockOwnerAlive(cur); alive || !certain {
		return nil, busySuiteLockError(dir, cur, certain)
	}
	if rerr := os.RemoveAll(dir); rerr != nil {
		return nil, fmt.Errorf("删除陈旧套件锁目录 %s 失败: %w", dir, rerr)
	}
	lock, cerr := createSuiteLockDir(dir, owner)
	if cerr != nil {
		if errors.Is(cerr, errSuiteLockDirExists) {
			// 删除与重建之间被别的进程抢到了新锁：不等待，直接按占用者报错。
			other, oerr := readSuiteLockOwnerAt(dir)
			if oerr == nil && validateSuiteLockOwner(other) == nil {
				return nil, busySuiteLockError(dir, other, true)
			}
			return nil, corruptSuiteLockError(dir, oerr)
		}
		return nil, cerr
	}
	lock.tookOver = true
	lock.staleOwner = stale
	return lock, nil
}

// sameSuiteLockOwner 两条记录是否指向同一 owner（PID+hostname+start 全等）。
func sameSuiteLockOwner(a, b *suiteLockOwner) bool {
	if a == nil || b == nil {
		return false
	}
	return a.PID == b.PID && sameHostname(a.Hostname, b.Hostname) && a.Start.Equal(b.Start)
}

// Release 释放套件锁（只删「仍属于本进程」的锁；被别的进程接管时拒绝删除）。
func (l *e2eSuiteLock) Release() error {
	if l == nil || l.dir == "" {
		return nil
	}
	owner, err := readSuiteLockOwnerAt(l.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, derr := os.Stat(l.dir); errors.Is(derr, fs.ErrNotExist) {
				return nil // 锁目录已不在（被清理）：幂等返回
			}
			return fmt.Errorf("套件锁目录 %s 存在但缺少 %s，拒绝删除（记录不完整，需人工确认）",
				l.dir, e2eSuiteLockOwnerFile)
		}
		return fmt.Errorf("读套件锁记录失败，拒绝删除 %s: %w", l.dir, err)
	}
	if owner.PID != os.Getpid() {
		return fmt.Errorf("套件锁 owner pid=%d != 本进程 %d：拒绝删除 %s（活 run 永不被回收）",
			owner.PID, os.Getpid(), l.dir)
	}
	if owner.CreationTime != 0 && l.creationTime != 0 && owner.CreationTime != l.creationTime {
		return fmt.Errorf("套件锁 owner creation_time=%d != 本进程 %d：拒绝删除 %s（PID 可能被复用）",
			owner.CreationTime, l.creationTime, l.dir)
	}
	if err := os.RemoveAll(l.dir); err != nil {
		return fmt.Errorf("删除套件锁目录 %s 失败: %w", l.dir, err)
	}
	return nil
}

// busySuiteLockError 「已有 e2e 套件在运行」的清晰诊断（TestMain 打印后 exit 2）。
func busySuiteLockError(dir string, owner *suiteLockOwner, certain bool) error {
	state := "进程存活"
	if !certain {
		state = "进程存活状态不确定（异 host / 查询被拒，无法证明已死）"
	}
	return fmt.Errorf(`已有 e2e 套件在运行（host 级 suite lock；同一 host 同时只允许一个 e2e 套件，spec §16）：
  锁目录: %s
  占用者: pid=%d host=%s start=%s creation_time=%d exe=%s
          args=%v
  状态:   %s
  本进程立即 fast-fail（不等待、不重试、不静默跳过）。
  处置:   等占用者结束后重试；仅当确认占用者进程已死且无其它 e2e 进程时，
          才可手工删除锁目录 %s 后重试。`,
		dir, owner.PID, owner.Hostname, owner.Start.Format(time.RFC3339), owner.CreationTime,
		owner.Exe, owner.Args, state, dir)
}

// corruptSuiteLockError 坏记录不接管（宁可 fast-fail 人工处置，也不误删别人的锁）。
func corruptSuiteLockError(dir string, cause error) error {
	return fmt.Errorf(`e2e 套件锁记录损坏/不完整，拒绝接管（坏记录不接管）：
  锁目录: %s
  原因:   %v
  本进程立即 fast-fail（不等待、不重试）。
  处置:   确认无 e2e 套件进程运行后，手工删除锁目录 %s 再重试；若确实有 e2e 进程
          在运行则等它结束（套件锁不自动清理无法证明归属的记录）。`, dir, cause, dir)
}
