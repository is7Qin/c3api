// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：registry 状态机 / Redis 索引预留 / stale sweep 是纯
// 基础设施逻辑，普通 test job 直接单测（harness_registry_test.go 用 e2eRegistryDirOverride
// 注入临时目录，绝不触碰真实 registry）。
//
// 设计要点（spec §3.3/§9.3/§9.4/§10.2-10.4/§10.9）：
//   - 每 run 先原子写 registry（state=starting），之后每次状态迁移 temp+rename 原子写；
//   - 每 run 唯一 PG 库 + 唯一 Redis 逻辑库（1..15，预留文件整个 run 持有）；
//   - stale 回收**保守优先**：hostname!=本机 / PID 活 / PID 不确定 / 坏记录 / 记录
//     尚处 starting|services_up|spawning 一律不回收（warn），宁可泄漏不可误删；
//   - 回收前用 CAS（rename run-*.json -> run-*.json.reaping.<sweeper>）独占记录；
//     强杀前逐个校验 creation_time（PID 复用安全）；PG 有活跃连接不 DROP；
//     Redis 锁 owner uuid 不匹配 => 保留记录与锁并返回可重试 error（下次 sweep 重试；
//     绝不删除记录让孤儿索引失去追踪，P0 review momus Minor-3 / spec §13）。
package e2e

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	rand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// e2eRegistryDirName registry 根目录名（OS temp 下；同 host 多 run 共享）。
	e2eRegistryDirName = "e2e-registry"

	e2eRunFilePrefix = "run-"
	e2eRunFileSuffix = ".json"
	e2eReapingInfix  = ".reaping."
	// e2eClaimSuffix sweep CAS：目录创建是跨平台原子独占原语（rename 在 Windows
	// 并发同源时不可靠，实测两个并发 rename 都返回 nil）。
	e2eClaimSuffix     = ".claim"
	e2eTakeoverSuffix  = ".takeover"
	e2eRedisLockFormat = "e2e-redis-db-%d.lock"
	e2eRunTmpDirPrefix = "e2e-run-"

	// e2eRedisDBMin/Max Redis 逻辑库 1..15（0 留生产/手动部署；16 库上限 => 最多 15 并发 run）。
	e2eRedisDBMin = 1
	e2eRedisDBMax = 15

	// e2eStaleAge 确定死亡后的最短回收等待（spec §3.3：age > 2min）。
	e2eStaleAge = 2 * time.Minute
	// e2eReapingTakeoverAge .reaping.* 记录视为「上一个 sweeper 死亡」的年龄门槛。
	e2eReapingTakeoverAge = 5 * time.Minute
	// e2ePGDBPrefix 每 run PG 库名前缀（DROP 前必须校验，防坏记录误删无关库）。
	e2ePGDBPrefix = "c3api_e2e_"
	// e2eKillGrace 强杀后等待进程消失的预算。
	e2eKillGrace = 5 * time.Second
)

// e2eRegistryDirOverride 单测注入点：非空时覆盖 registry 目录（普通 test job
// 只在 t.TempDir 上操作，绝不触碰真实 registry）。
var e2eRegistryDirOverride string

// e2eRegistryDir registry 根目录。
func e2eRegistryDir() string {
	if e2eRegistryDirOverride != "" {
		return e2eRegistryDirOverride
	}
	return filepath.Join(os.TempDir(), e2eRegistryDirName)
}

// e2eRegistryEnsureDir 创建 registry 目录（幂等）。
func e2eRegistryEnsureDir() error {
	return os.MkdirAll(e2eRegistryDir(), 0o755)
}

// runState registry 状态机（spec §9.4；顺序 starting -> [services_up] ->
// resources_ready -> spawning -> ready -> finishing）。
type runState string

const (
	stateStarting       runState = "starting"
	stateServicesUp     runState = "services_up"
	stateResourcesReady runState = "resources_ready"
	stateSpawning       runState = "spawning"
	stateReady          runState = "ready"
	stateFinishing      runState = "finishing"
)

// runStateTransitions 合法迁移（非法迁移 = 编程错误，registry 写面立即报错）。
var runStateTransitions = map[runState][]runState{
	// finishing = cleanup 收尾态：任何未完成的 run 都可以进入（cleanup 必须幂等）。
	stateStarting:       {stateServicesUp, stateResourcesReady, stateFinishing},
	stateServicesUp:     {stateResourcesReady, stateFinishing},
	stateResourcesReady: {stateSpawning, stateFinishing},
	stateSpawning:       {stateReady, stateFinishing},
	stateReady:          {stateFinishing},
	stateFinishing:      {},
}

func (s runState) canTransitionTo(next runState) bool {
	if s == next {
		return false
	}
	for _, allowed := range runStateTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// childProcess registry 里登记的子进程（PID/PGID/创建时间，强杀前逐项校验）。
type childProcess struct {
	PID          int    `json:"pid"`
	PGID         int    `json:"pgid,omitempty"`
	CreationTime uint64 `json:"creation_time,omitempty"`
	Kind         string `json:"kind"`
}

// runRecord 每 run 的 registry 记录（OS temp/e2e-registry/run-<uuid>.json；所有
// 更新 temp+rename 原子写；字段覆盖 spec §3.2/§9.4 要求 + tmp/log 目录便于回收）。
type runRecord struct {
	UUID             string            `json:"uuid"`
	PID              int               `json:"pid"`
	Hostname         string            `json:"hostname"`
	Start            time.Time         `json:"start"`
	State            runState          `json:"state"`
	PlannedPGDB      string            `json:"planned_pg_db"`
	PGDB             string            `json:"pg_db,omitempty"`
	RedisDB          int               `json:"redis_db"`           // 计划值；-1 = 未分配
	AllocatedRedisDB int               `json:"allocated_redis_db"` // 拿到锁后写入；-1 = 未分配
	LockUUID         string            `json:"lock_uuid,omitempty"`
	ManualOverride   bool              `json:"manual_override"`
	ChildPIDs        []childProcess    `json:"child_pids"`
	ComposeProject   string            `json:"compose_project,omitempty"`
	ComposeFile      string            `json:"compose_file,omitempty"`
	ContainerLabels  map[string]string `json:"container_labels,omitempty"`
	JobName          string            `json:"job_name,omitempty"`
	TmpDir           string            `json:"tmp_dir,omitempty"`
	LogDir           string            `json:"log_dir,omitempty"`
}

// runRecordPath registry 记录文件路径。
func runRecordPath(uuid string) string {
	return filepath.Join(e2eRegistryDir(), e2eRunFilePrefix+uuid+e2eRunFileSuffix)
}

// readRunRecord 读 registry 记录（调用方负责损坏记录的处理策略）。
func readRunRecord(path string) (*runRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec runRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return &rec, nil
}

// writeFileAtomic temp+rename 原子写（同目录 temp 文件 => 同卷 rename 原子；读者
// 只会看到旧文件或新文件，不会读到半截 JSON）。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // rename 成功后 Remove 为 no-op
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// saveRunRecord registry 记录原子落盘（先写后 rename；失败 = run 无法继续）。
func saveRunRecord(rec *runRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run record: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(runRecordPath(rec.UUID), data)
}

// transitionRunRecord 状态迁移（校验合法 + 原子落盘）。返回的新状态写回 rec。
func transitionRunRecord(rec *runRecord, next runState) error {
	if !rec.State.canTransitionTo(next) {
		return fmt.Errorf("run %s 非法状态迁移 %s -> %s", rec.UUID, rec.State, next)
	}
	rec.State = next
	return saveRunRecord(rec)
}

// addChildProcess 登记子进程（Start 成功后立即写 registry，早于 readiness 轮询——
// kill -9 的极窄窗口由状态机保护，见 spec §9.3）。
func addChildProcess(rec *runRecord, child childProcess) error {
	rec.ChildPIDs = append(rec.ChildPIDs, child)
	return saveRunRecord(rec)
}

// removeChildProcess 子进程退出后摘除登记（按 PID 精确匹配）。
func removeChildProcess(rec *runRecord, pid int) error {
	out := rec.ChildPIDs[:0]
	for _, c := range rec.ChildPIDs {
		if c.PID != pid {
			out = append(out, c)
		}
	}
	rec.ChildPIDs = out
	return saveRunRecord(rec)
}

// redisLock Redis 逻辑库预留文件内容（整个 run 持有；O_EXCL 只是毫秒临界区）。
type redisLock struct {
	UUID           string    `json:"uuid"`
	PID            int       `json:"pid"`
	Hostname       string    `json:"hostname"`
	Start          time.Time `json:"start"`
	ManualOverride bool      `json:"manual_override,omitempty"`
}

// redisLockPath 逻辑库预留文件路径（和 registry 同目录，便于 sweep 一起处理）。
func redisLockPath(db int) string {
	return filepath.Join(e2eRegistryDir(), fmt.Sprintf(e2eRedisLockFormat, db))
}

// readRedisLock 读预留文件；不存在 => (nil,nil)；损坏 => error。
func readRedisLock(db int) (*redisLock, error) {
	data, err := os.ReadFile(redisLockPath(db))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var lock redisLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse redis lock db=%d: %w", db, err)
	}
	return &lock, nil
}

// writeRedisLockExclusive O_EXCL 抢逻辑库：失败 = 已被占用（fs.ErrExist）。
func writeRedisLockExclusive(db int, lock redisLock) error {
	path := redisLockPath(db)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(lock)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// allocRedisIndex 分配 Redis 逻辑库：1..15 随机顺序 O_EXCL 抢占；全满 = 明确报错
// （R1：最多 15 并发 run）。manual > 0 时（E2E_REDIS_DB 手动覆盖）必须独占该库。
func allocRedisIndex(uuid string, manual int) (int, error) {
	hostname, _ := os.Hostname()
	lock := redisLock{UUID: uuid, PID: os.Getpid(), Hostname: hostname, Start: time.Now(), ManualOverride: manual > 0}
	if manual > 0 {
		if manual < e2eRedisDBMin || manual > e2eRedisDBMax {
			return 0, fmt.Errorf("E2E_REDIS_DB=%d 非法（合法域 %d..%d；0 留生产/手动部署）", manual, e2eRedisDBMin, e2eRedisDBMax)
		}
		if err := writeRedisLockExclusive(manual, lock); err != nil {
			return 0, fmt.Errorf("E2E_REDIS_DB=%d 已被占用（手动覆盖只能用于单 run 调试）: %w", manual, err)
		}
		return manual, nil
	}
	candidates := make([]int, 0, e2eRedisDBMax-e2eRedisDBMin+1)
	for db := e2eRedisDBMin; db <= e2eRedisDBMax; db++ {
		candidates = append(candidates, db)
	}
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	var lastErr error
	for _, db := range candidates {
		err := writeRedisLockExclusive(db, lock)
		if err == nil {
			return db, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return 0, err
		}
		lastErr = err
	}
	return 0, fmt.Errorf("Redis 逻辑库 %d..%d 已全部被占用（并发 run 上限 15；确认无残留后可人工清理 %s）: %w",
		e2eRedisDBMin, e2eRedisDBMax, e2eRegistryDir(), lastErr)
}

// releaseRedisIndex 释放预留（owner uuid 必须匹配；不匹配只报错不删）。
func releaseRedisIndex(db int, uuid string) error {
	lock, err := readRedisLock(db)
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	if lock.UUID != uuid {
		return fmt.Errorf("Redis 锁 db=%d owner=%s != 本 run %s：拒绝删除（活 run 永不被回收）", db, lock.UUID, uuid)
	}
	if err := os.Remove(redisLockPath(db)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// newPGDatabaseName 每 run 唯一 PG 库名：c3api_e2e_<hosthash>_<pid>_<nano>_<rand4>
// （长度 < 63；hosthash 让同 host 运维可辨，nano+rand4 防撞）。
func newPGDatabaseName() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	h := sha256.Sum256([]byte(hostname))
	randBuf := make([]byte, 2)
	if _, err := crand.Read(randBuf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s_%d_%d_%s",
		e2ePGDBPrefix,
		hex.EncodeToString(h[:])[:8],
		os.Getpid(),
		time.Now().UnixNano(),
		hex.EncodeToString(randBuf),
	), nil
}

// replaceDSNDatabase 只替换 DSN 的库名（harness 不解析/不重建凭据；配置里每 run
// 唯一库 = 隔离边界）。
func replaceDSNDatabase(dsn, dbName string) (string, error) {
	idx := strings.LastIndex(dsn, "/")
	if idx < 0 {
		return "", fmt.Errorf("DSN 缺少库名段: %.40s", dsn)
	}
	rest := dsn[idx+1:]
	query := ""
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		query = rest[q:]
	}
	out := dsn[:idx+1] + dbName + query
	if query == "" && !strings.Contains(out, "sslmode=") {
		out += "?sslmode=disable"
	}
	return out, nil
}

// createPGDatabase 在 base PostgreSQL 上 CREATE DATABASE（harness 是 e2e 里唯一
// 建库入口）。
func createPGDatabase(ctx context.Context, adminDSN, dbName string) error {
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("admin pool: %w", err)
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("admin pool ping: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
		return fmt.Errorf("create database %s: %w", dbName, err)
	}
	return nil
}

// dropStaleDatabase sweep 用的可注入回收副作用（单测替换；默认实现要求真实 admin DSN）。
var dropStaleDatabase = realDropStaleDatabase

// realDropStaleDatabase 回收死 run 的 PG 库：
//   - 库名必须带每 run 前缀（防坏记录误删无关库）；
//   - pg_stat_activity 仍有连接 => 保留库并报错（warn 后由 sweep 保留记录重试）。
func realDropStaleDatabase(dbName string) error {
	if !strings.HasPrefix(dbName, e2ePGDBPrefix) || len(dbName) <= len(e2ePGDBPrefix) {
		return fmt.Errorf("拒绝 DROP 非 e2e 库名 %q", dbName)
	}
	adminDSN := envOr("TEST_DATABASE_URL", "postgres://postgres:c3api@localhost:15432/postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("admin pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("admin pool ping: %w", err)
	}
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, dbName).Scan(&active); err != nil {
		return fmt.Errorf("pg_stat_activity: %w", err)
	}
	if active > 0 {
		return fmt.Errorf("库 %s 仍有 %d 个活跃连接：保留库与 registry 记录", dbName, active)
	}
	if _, err := pool.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop database %s: %w", dbName, err)
	}
	return nil
}

// killRecordedChildren 回收死 run 的子进程/进程组：先逐 PID（creation-time 校验）
// 再整进程组；全部确认死亡才返回 nil，否则保留资源（强杀失败 => 不回收）。
func killRecordedChildren(children []childProcess) error {
	var errs []string
	for _, child := range children {
		if err := forceKillTree(child); err != nil {
			errs = append(errs, fmt.Sprintf("pid=%d: %v", child.PID, err))
		}
	}
	if err := killProcessGroup(children); err != nil {
		errs = append(errs, fmt.Sprintf("pgid kill: %v", err))
	}
	// 等待并确认全部死亡（强杀是异步的；create-time 校验过的 PID 才判定）。
	deadline := time.Now().Add(e2eKillGrace)
	for {
		alive := false
		for _, child := range children {
			if a, certain := procAlive(child.PID); !certain || a {
				alive = true
				break
			}
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			errs = append(errs, "强杀后仍有进程存活（或状态不确定）")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// runTempDirValid 校验登记 temp 目录确实由本 harness 创建（回收时删除路径前
// 必须验证，绝不 RemoveAll 任意路径）。
func runTempDirValid(dir string) bool {
	if dir == "" {
		return false
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) != filepath.Clean(os.TempDir()) {
		return false
	}
	base := filepath.Base(clean)
	if !strings.HasPrefix(base, e2eRunTmpDirPrefix) {
		return false
	}
	return !strings.ContainsFunc(base, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_')
	})
}

// sweepStats stale sweep 观测面（测试/日志用）。
type sweepStats struct {
	Scanned   int
	Reclaimed int
	Kept      int
	Corrupt   int
}

// sweepMark 扫描时把「记录 / 崩溃残留 / 陈旧 claim」三类目录项分开；记录与
// .reaping 残留按同一 base 记录名归并（claim 也从 base 名派生）。
func baseRecordPath(dir, name string) (base string, reaping bool, ok bool) {
	if !strings.HasPrefix(name, e2eRunFilePrefix) {
		return "", false, false
	}
	if idx := strings.Index(name, e2eRunFileSuffix+e2eReapingInfix); idx > 0 {
		return filepath.Join(dir, name[:idx+len(e2eRunFileSuffix)]), true, true
	}
	if strings.HasSuffix(name, e2eRunFileSuffix) {
		return filepath.Join(dir, name), false, true
	}
	return "", false, false
}

// sweepStaleRuns 保守 stale 回收（入口）。logf 为 nil 时静默。
//
// CAS 用 **Mkdir 独占**而不是 rename：本机实测 Windows 上两个并发 rename 同源
// （甚至同/不同目标）都返回 nil，rename 不能作为 CAS 原语；目录创建是原子的。
func sweepStaleRuns(logf func(format string, args ...any)) sweepStats {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var stats sweepStats
	if err := e2eRegistryEnsureDir(); err != nil {
		logf("e2e sweep: registry dir: %v", err)
		return stats
	}
	hostname, _ := os.Hostname()
	sweeperID := newSweeperID()
	entries, err := os.ReadDir(e2eRegistryDir())
	if err != nil {
		logf("e2e sweep: read dir: %v", err)
		return stats
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			sweepClaimDir(filepath.Join(e2eRegistryDir(), name), hostname, sweeperID, &stats, logf)
			continue
		}
		base, reaping, ok := baseRecordPath(e2eRegistryDir(), name)
		if !ok {
			continue
		}
		if reaping {
			// 上一个 sweeper 崩溃留下的 .reaping 记录：age > 5min 才可接管。
			info, ierr := entry.Info()
			if ierr != nil || time.Since(info.ModTime()) <= e2eReapingTakeoverAge {
				continue
			}
		}
		if seen[base] {
			continue
		}
		seen[base] = true
		stats.Scanned++
		sweepRecordBase(base, hostname, sweeperID, &stats, logf)
	}
	return stats
}

// newSweeperID sweep CAS 标识（pid + 单调计数 + 随机后缀：同进程并发 sweep 也互异）。
var sweeperCounter atomic.Uint64

func newSweeperID() string {
	uuid, err := newRunUUID()
	if err != nil {
		uuid = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if len(uuid) > 8 {
		uuid = uuid[:8]
	}
	return fmt.Sprintf("%d-%d-%s", os.Getpid(), sweeperCounter.Add(1), uuid)
}

// sweepClaimDir 处理 claim 目录：只清理「年龄 > 5min」的崩溃残留（活 sweeper 的
// claim 新鲜，永不动）；takeover 标记目录 age > 5min 视为上一个接管者崩溃，可删。
func sweepClaimDir(path, hostname, sweeperID string, stats *sweepStats, logf func(string, ...any)) {
	name := filepath.Base(path)
	if strings.HasSuffix(name, e2eClaimSuffix+e2eTakeoverSuffix) {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > e2eReapingTakeoverAge {
			if rerr := os.Remove(path); rerr == nil {
				logf("e2e sweep: 清理崩溃的 takeover 标记 %s", name)
			}
		}
		return
	}
	if !strings.HasSuffix(name, e2eClaimSuffix) {
		return
	}
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) <= e2eReapingTakeoverAge {
		return
	}
	base := strings.TrimSuffix(path, e2eClaimSuffix)
	logf("e2e sweep: 发现陈旧 claim（sweeper 崩溃残留），尝试接管：%s", name)
	if !takeoverStaleClaim(base, logf) {
		return
	}
	stats.Scanned++
	// 接管者在 takeoverStaleClaim 里已独占重建 claim：把「已持有」直接传给 sweep
	// 内核（sweepRecordBaseOwned），**不得**再走 sweepRecordBase 的第二次 Mkdir ——
	// 否则同路径必然 ErrExist，落入「活 sweeper 正在处理」分支 => 陈旧 claim 永久
	// Kept、记录与资源永不回收（P0 review momus V1 / spec §13）。
	sweepRecordBaseOwned(base, hostname, sweeperID, stats, logf)
}

// takeoverStaleClaim 独占接管陈旧 claim：先 O_EXCL 建 takeover 标记（只有一个赢家），
// 再删陈旧 claim 并立即重抢；抢不到（别的 sweeper 先到）=> false。
func takeoverStaleClaim(base string, logf func(string, ...any)) bool {
	claimPath := base + e2eClaimSuffix
	marker := claimPath + e2eTakeoverSuffix
	if err := os.Mkdir(marker, 0o755); err != nil {
		return false
	}
	defer func() { _ = os.Remove(marker) }()
	if err := os.Remove(claimPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("e2e sweep: 删除陈旧 claim 失败：%v", err)
		return false
	}
	if err := os.Mkdir(claimPath, 0o755); err != nil {
		return false
	}
	return true
}

// findRecordFile 在已独占的 claim 内定位实际记录文件（正常记录路径或 .reaping 残留）。
func findRecordFile(base string) (path string, reaping bool) {
	if _, err := os.Stat(base); err == nil {
		return base, false
	}
	entries, err := os.ReadDir(filepath.Dir(base))
	if err != nil {
		return "", false
	}
	prefix := filepath.Base(base) + e2eReapingInfix
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), prefix) {
			return filepath.Join(filepath.Dir(base), entry.Name()), true
		}
	}
	return "", false
}

// sweepRecordBase 处理一条记录（claim CAS 独占 -> 判定 -> 改名 .reaping -> 回收）。
func sweepRecordBase(base, hostname, sweeperID string, stats *sweepStats, logf func(string, ...any)) {
	claimPath := base + e2eClaimSuffix
	if err := os.Mkdir(claimPath, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 活 sweeper 正在处理；陈旧 claim 由 sweepClaimDir 的接管路径负责。
			stats.Kept++
			return
		}
		stats.Kept++
		logf("e2e sweep: 抢占 claim %s 失败：%v", filepath.Base(claimPath), err)
		return
	}
	sweepRecordBaseOwned(base, hostname, sweeperID, stats, logf)
}

// sweepRecordBaseOwned 在「本 sweeper 已独占 claim」的前提下处理记录：调用方要么刚
// Mkdir 成功（sweepRecordBase），要么是陈旧 claim 的接管者（sweepClaimDir 经
// takeoverStaleClaim 已重建 claim）。**这里禁止再次 Mkdir 同路径**——接管路径会因此
// ErrExist 自抢自败（P0 review V1：记录/claim/PG 库/Redis 锁永久泄漏）。claim 由本
// sweeper 独占，处理结束（含提前 return）必须释放。
func sweepRecordBaseOwned(base, hostname, sweeperID string, stats *sweepStats, logf func(string, ...any)) {
	claimPath := base + e2eClaimSuffix
	defer func() { _ = os.Remove(claimPath) }()

	actual, isReaping := findRecordFile(base)
	if actual == "" {
		return // 记录已被持有者处理掉
	}
	if isReaping {
		if info, err := os.Stat(actual); err == nil && time.Since(info.ModTime()) <= e2eReapingTakeoverAge {
			return // 新鲜 .reaping：可能是另一个活 sweeper 的处理中视图
		}
	}
	rec, err := readRunRecord(actual)
	if err != nil {
		stats.Corrupt++
		logf("e2e sweep: 损坏记录永不回收：%v", err)
		return
	}
	if rec.UUID == "" || rec.Hostname == "" {
		stats.Corrupt++
		logf("e2e sweep: 字段缺失记录永不回收：%s", filepath.Base(actual))
		return
	}
	if rec.Hostname != hostname {
		stats.Kept++
		return // 异 host 记录永不回收（单机边界；人工清理）
	}
	reclaimable, reason := staleReclaimable(rec, time.Now())
	if !reclaimable {
		stats.Kept++
		logf("e2e sweep: 保留 run %s（state=%s age=%s）：%s",
			rec.UUID, rec.State, time.Since(rec.Start).Round(time.Second), reason)
		return
	}

	// 已独占：把记录改名到 .reaping.<sweeper>（崩溃恢复可见；此时 rename 安全）。
	reapingPath := base + e2eReapingInfix + sweeperID
	if err := os.Rename(actual, reapingPath); err != nil {
		stats.Kept++
		logf("e2e sweep: 标记 .reaping 失败（保留记录）：%v", err)
		return
	}
	if err := reclaimRunRecord(rec, logf); err != nil {
		stats.Kept++
		logf("e2e sweep: 回收 run %s 失败，保留资源与记录（下次重试）：%v", rec.UUID, err)
		if rerr := os.Rename(reapingPath, actual); rerr != nil {
			logf("e2e sweep: 恢复 run %s 记录失败（保留 .reaping）：%v", rec.UUID, rerr)
		}
		return
	}
	stats.Reclaimed++
	logf("e2e sweep: 已回收 run %s（pg_db=%s redis_db=%d）", rec.UUID, rec.PGDB, rec.AllocatedRedisDB)
	if err := os.Remove(reapingPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("e2e sweep: 删除 .reaping 记录失败：%v", err)
	}
}

// staleReclaimable 保守判定：返回是否可回收 + 保留原因。
// 规则（spec §9.4/§10.2，宁可泄漏不可误删）：
//   - 本 run PID 存活或不确定 => 永不回收；
//   - 状态 starting/services_up/spawning => 一律只 warn 保留（D7）；
//   - 状态 ready/finishing => 需 age > 2min（finishing 允许代为强杀）；
//   - 状态 resources_ready && 从未 spawn => 需 age > 2min && Redis 锁 owner 匹配。
func staleReclaimable(rec *runRecord, now time.Time) (bool, string) {
	if alive, certain := procAlive(rec.PID); !certain || alive {
		return false, "run PID 存活或状态不确定"
	}
	age := now.Sub(rec.Start)
	switch rec.State {
	case stateReady, stateFinishing:
		if age <= e2eStaleAge {
			return false, "已死但未超过最短回收年龄"
		}
		return true, ""
	case stateResourcesReady:
		if len(rec.ChildPIDs) > 0 {
			return false, "resources_ready 但已登记子进程（疑似未完成 spawn 登记）"
		}
		if age <= e2eStaleAge {
			return false, "已死但未超过最短回收年龄"
		}
		if rec.ManualOverride {
			return false, "手动覆盖 run（E2E_REDIS_DB）不自动回收"
		}
		lock, err := readRedisLock(rec.AllocatedRedisDB)
		if err != nil {
			return false, "Redis 锁不可读：" + err.Error()
		}
		if lock == nil || lock.UUID != rec.LockUUID {
			return false, "Redis 锁 owner 与记录不匹配（可能已被接管）"
		}
		return true, ""
	case stateStarting, stateServicesUp, stateSpawning:
		return false, "未进入可回收状态（D7：只告警保留，>24h 人工清理）"
	default:
		return false, "未知状态"
	}
}

// reclaimRunRecord 回收一条已 CAS 独占的死 run：先杀孤儿 -> 清资源 -> 删 temp/记录。
// 任何一步失败返回 error（调用方恢复记录，资源保留）。
func reclaimRunRecord(rec *runRecord, logf func(string, ...any)) error {
	// 1) 先杀孤儿子进程/进程组（强杀失败 => 保留全部资源，不回收）。
	if err := killRecordedChildren(rec.ChildPIDs); err != nil {
		return fmt.Errorf("强杀子进程失败: %w", err)
	}
	// 2) 基础资源：专用栈 down -v（库随卷消失）或 DROP 共享 PG 库。
	if rec.ComposeProject != "" {
		if err := cleanupComposeProject(rec); err != nil {
			return fmt.Errorf("清理专用 compose 栈失败: %w", err)
		}
	} else if rec.PGDB != "" {
		if rec.PGDB != rec.PlannedPGDB {
			return fmt.Errorf("PG 库名与记录不符（planned=%q actual=%q）：拒绝 DROP", rec.PlannedPGDB, rec.PGDB)
		}
		if err := dropStaleDatabase(rec.PGDB); err != nil {
			return fmt.Errorf("DROP PG 库失败: %w", err)
		}
	}
	// 3) Redis 预留：owner uuid 匹配才删；不匹配 => 保留记录 + 保留锁 + 返回可重试 error
	//    （调用方把 .reaping 恢复为正式记录，下次 sweep 重试；spec §9.4/§13）。
	if rec.AllocatedRedisDB >= e2eRedisDBMin && rec.LockUUID != "" {
		lock, err := readRedisLock(rec.AllocatedRedisDB)
		if err != nil {
			logf("e2e sweep: Redis 锁 db=%d 不可读，保留不删：%v", rec.AllocatedRedisDB, err)
		} else if lock == nil {
			// 已不存在 = 目标态
		} else if lock.UUID != rec.LockUUID {
			// owner 不匹配 = 该索引可能已被别的 run 接管：**保留记录与锁**并返回可重试
			// 错误，调用方把 .reaping 恢复为正式记录（保留追踪）——绝不删除记录让孤儿
			// 索引失去追踪（P0 review momus Minor-3 / spec §13）。下次 sweep 重试。
			return fmt.Errorf("Redis 锁 db=%d owner=%s 与记录 %s 不匹配：保留记录与锁（下次重试）",
				rec.AllocatedRedisDB, lock.UUID, rec.LockUUID)
		} else if err := os.Remove(redisLockPath(rec.AllocatedRedisDB)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("删除 Redis 锁 db=%d: %w", rec.AllocatedRedisDB, err)
		}
	}
	// 4) temp 目录（严格校验后删除）。
	if rec.TmpDir != "" {
		if !runTempDirValid(rec.TmpDir) {
			logf("e2e sweep: 拒绝删除可疑 temp 目录 %q", rec.TmpDir)
		} else if err := os.RemoveAll(rec.TmpDir); err != nil {
			return fmt.Errorf("删除 temp 目录: %w", err)
		}
	}
	return nil
}
