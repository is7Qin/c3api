// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// 共享 e2e harness：唯一生命周期所有者（spec §3.1/§3.2/§4.1）。
//
// 场景只写 config/请求/断言；外部资源与进程生命周期全部在这里：
//
//	registry 先写 -> 专用栈（可选）-> 唯一 PG 库/Redis 逻辑库 -> build（每 run 一次）
//	-> run 级 lazy migrate -> start/ready -> stop/kill/restart -> cleanup。
//
// 硬边界（boundary gate 守门）：场景不得 exec 子进程、建删库、拼 -migrate、docker
// 调用、固定端口/库名、设置子进程 env。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/redisx"
)

const (
	e2eReadyPollInterval = 50 * time.Millisecond
	// e2eGatewayReadyTimeout 首次/重启就绪预算（migrate 已提前完成，正常 <1s）。
	e2eGatewayReadyTimeout = 90 * time.Second
	// e2eGatewayStartAttempts 首次启动动态换端口重试次数（spec §3.2/§4.1）。
	e2eGatewayStartAttempts = 3
	// e2eRestartBindBudget 重启必须复用原地址：3s 内 bind 不上 => fail+dump（不静默换端口）。
	e2eRestartBindBudget = 3 * time.Second
	// e2eGracefulStopTimeout 优雅停机等待预算。
	e2eGracefulStopTimeout = 30 * time.Second
	// e2eKillWaitTimeout 强杀后等待 Wait 返回的预算。
	e2eKillWaitTimeout = 20 * time.Second
	// e2eUpstreamReadyTimeout fakeupstream 监听就绪预算。
	e2eUpstreamReadyTimeout = 15 * time.Second
	// e2eBuildTimeout 单二进制构建预算（首次冷缓存；私有模块 auth 由完整 env 继承）。
	e2eBuildTimeout = 5 * time.Minute
	// e2eMigrateTimeout run 级 migrate up 预算。
	e2eMigrateTimeout = 2 * time.Minute
	e2eLogTailBytes   = 8 << 10
)

// e2eSweepOnce 每进程只做一次启动期 stale sweep（spec §3.2）；测试可显式再调。
var e2eSweepOnce sync.Once

// e2eHTTPClient 就绪/管理面轮询客户端（短超时；本轮询不代替断言）。
var e2eHTTPClient = &http.Client{Timeout: 3 * time.Second}

// e2eRunOptions 每 run 选项。
type e2eRunOptions struct {
	// DedicatedServices 使用自包含专用 compose 栈（不要求 base env；共享 base 永不被 mutate）。
	DedicatedServices bool
}

// gatewayInputs 渲染 config 的唯一输入面：动态端口 + 本 run 的 DSN/Redis 逻辑库。
type gatewayInputs struct {
	Addr      string
	DSN       string
	RedisAddr string
	RedisDB   int
}

// exitExpectation ExpectExit 期望（进程应在 Timeout 内退出）。
type exitExpectation struct {
	Code     int
	Contains string
	Timeout  time.Duration
}

// gatewaySpec 单个网关实例声明：场景只给 Name/Config/Ready/ExpectExit。
type gatewaySpec struct {
	Name       string
	Config     func(in gatewayInputs) string
	ExpectExit *exitExpectation
	Ready      func(baseURL string) bool
}

// upstreamSpec fakeupstream 声明：地址由 harness 分配（spec §9.5），场景只给
// Name/Flags。
type upstreamSpec struct {
	Name  string
	Flags []string
}

// resourcesSnapshot 本 run 的唯一资源证据（并发验收/registry 断言）。
type resourcesSnapshot struct {
	PGDB           string
	RedisDB        int
	Addr           string
	ComposeProject string
}

// childExit 子进程退出结果（monitor goroutine 写、wait 读；close(done) 建立序）。
type childExit struct {
	Code int
	Err  error
}

// child harness 持有的被测子进程句柄（server / fakeupstream）。
type child struct {
	kind    string
	cmd     *exec.Cmd
	rec     childProcess
	logPath string
	logFile *os.File
	done    chan struct{}
	res     childExit
}

// exited 非阻塞查询是否已退出。
func (c *child) exited() bool {
	if c == nil {
		return true
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait 等待退出；超时返回 (zero,false)。
func (c *child) wait(timeout time.Duration) (childExit, bool) {
	select {
	case <-c.done:
		return c.res, true
	case <-time.After(timeout):
		return childExit{}, false
	}
}

// monitor 是 Wait 的唯一调用点（goroutine 内 close(done) 之后再无人写 res）。
func (c *child) monitor() {
	err := c.cmd.Wait()
	code := -1
	if c.cmd.ProcessState != nil {
		code = c.cmd.ProcessState.ExitCode()
	}
	c.res = childExit{Code: code, Err: err}
	_ = c.logFile.Close()
	close(c.done)
}

// gateway 常驻网关句柄（config/log/addr 复用；Restart 原地替换子进程）。
type gateway struct {
	run        *e2eRun
	name       string
	Addr       string
	BaseURL    string
	ConfigPath string
	LogPath    string
	inputs     gatewayInputs
	ready      func(baseURL string) bool
	child      *child
	// expectExit=true 表示这是 ExpectExit 网关（已退出，无进程需要清理）。
	expectExit bool
	exitCode   int
}

// e2eRun 一次 e2e run（每 run 唯一 PG 库/Redis 逻辑库/动态端口/temp/log + registry）。
type e2eRun struct {
	t      *testing.T
	uuid   string
	opts   e2eRunOptions
	record *runRecord

	adminPool *pgxpool.Pool
	pgPool    *pgxpool.Pool
	dbName    string
	dsn       string
	redisAddr string
	redisDB   int
	redisCli  *redis.Client

	tmp    string
	logDir string

	srvBin string
	upBin  string

	buildOnce sync.Once
	buildErr  error

	services  e2eServices
	dedicated *dedicatedServices
	job       *procJob

	migrated bool
	children []*child
	gateways []*gateway
	adminJWT map[string]string
	seq      int
	closed   bool
	t0       time.Time
}

// logBaseDir E2E_LOG_DIR 或 os.TempDir()/e2e-logs（不在 t.TempDir：失败 artifact
// 需要在测试进程结束后仍可取）。
func logBaseDir() string {
	return envOr("E2E_LOG_DIR", filepath.Join(os.TempDir(), "e2e-logs"))
}

// repoRootDir 仓库根目录（从本文件位置上跳 3 层：tools/e2e -> 根）。
func repoRootDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller 失败")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile))), nil
}

// newE2ERun 建立一次 run 的唯一资源与生命周期（不 build、不起 gateway）。
//
// 顺序（spec §3.2/§9.4；registry 先写，资源随后）：
//  1. 启动期 stale sweep（每进程一次；保守规则见 harness_registry.go）；
//  2. 原子写 registry（state=starting；planned_pg_db/tmp/log/job 名已就位）；
//  3. DedicatedServices => 先写 compose project/file/labels 再启动专用栈（AV-β）；
//  4. 唯一 PG 库 + Redis 逻辑库（O_EXCL 预留文件整个 run 持有）+ flush；
//  5. 原子更新 state=resources_ready。
func newE2ERun(t *testing.T, opts e2eRunOptions) *e2eRun {
	t.Helper()
	uuid, err := newRunUUID()
	require.NoError(t, err, "生成 run uuid")
	dbName, err := newPGDatabaseName()
	require.NoError(t, err, "生成唯一 PG 库名")
	tmp, err := os.MkdirTemp("", e2eRunTmpDirPrefix+uuid[:8]+"-")
	require.NoError(t, err, "创建 run temp 目录")
	logDir := filepath.Join(logBaseDir(), "run-"+uuid[:8])
	require.NoError(t, os.MkdirAll(logDir, 0o755), "创建 run 日志目录")
	hostname, _ := os.Hostname()
	r := &e2eRun{
		t:        t,
		t0:       time.Now(),
		uuid:     uuid,
		opts:     opts,
		tmp:      tmp,
		logDir:   logDir,
		adminJWT: map[string]string{},
	}
	r.record = &runRecord{
		UUID:             uuid,
		PID:              os.Getpid(),
		Hostname:         hostname,
		Start:            time.Now(),
		State:            stateStarting,
		PlannedPGDB:      dbName,
		RedisDB:          -1,
		AllocatedRedisDB: -1,
		JobName:          "e2e-" + uuid[:16],
		TmpDir:           tmp,
		LogDir:           logDir,
	}
	t.Cleanup(func() { r.cleanup(t) })

	// 1) 启动期 stale sweep（只处理 hostname==本机 + 确定死亡的旧 run）。
	e2eSweepOnce.Do(func() {
		stats := sweepStaleRuns(func(format string, args ...any) {
			t.Logf("e2e sweep: "+format, args...)
		})
		if stats.Scanned > 0 {
			t.Logf("e2e sweep 汇总：scanned=%d reclaimed=%d kept=%d corrupt=%d",
				stats.Scanned, stats.Reclaimed, stats.Kept, stats.Corrupt)
		}
	})

	// 2) registry 先写（state=starting），之后所有更新 temp+rename 原子写。
	require.NoError(t, e2eRegistryEnsureDir(), "创建 registry 目录")
	require.NoError(t, saveRunRecord(r.record), "registry 先写（state=starting）")

	// Windows Job Object：KILL_ON_JOB_CLOSE => 测试进程被 kill -9 后内核自动终止
	// 全部子进程（OS 层无孤儿）；创建失败降级 creation-time + 逐 PID taskkill。
	job, jerr := newRunJob(r.record.JobName)
	if jerr != nil {
		t.Logf("warn: Job Object 创建失败，降级 creation-time + 逐 PID 强杀：%v", jerr)
	} else {
		r.job = job
	}

	// 3) 基础设施所有权：shared（只读 base）或 dedicated（自包含专用栈）。
	if opts.DedicatedServices {
		r.dedicated = newDedicatedServices(t, r)
		r.services = r.dedicated
		r.record.ComposeProject = r.dedicated.project
		r.record.ComposeFile = r.dedicated.composeFile
		r.record.ContainerLabels = r.dedicated.containerLabels()
		require.NoError(t, saveRunRecord(r.record), "registry 写 compose project/file/labels（专用栈启动前）")
		require.NoError(t, r.dedicated.up(t), "专用 compose 栈启动（docker compose v2+ 前置）")
		require.NoError(t, transitionRunRecord(r.record, stateServicesUp), "registry state=services_up")
	} else {
		r.services = newSharedServices(t)
	}

	// 4) 唯一 PG 库 + Redis 逻辑库。
	adminDSN := r.services.PGAdminDSN()
	adminPool, err := pgxpool.New(context.Background(), adminDSN)
	require.NoError(t, err, "admin pool")
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = adminPool.Ping(pingCtx)
	pingCancel()
	require.NoError(t, err, "base PostgreSQL 不可达（先 docker compose -f deploy/test-compose.yml up -d）")
	r.adminPool = adminPool
	require.NoError(t, createPGDatabase(context.Background(), adminDSN, dbName), "CREATE DATABASE 每 run 唯一库")
	r.dsn, err = replaceDSNDatabase(adminDSN, dbName)
	require.NoError(t, err, "派生每 run DSN")
	r.pgPool, err = pgxpool.New(context.Background(), r.dsn)
	require.NoError(t, err, "run PG pool")
	r.dbName = dbName
	r.redisAddr = r.services.RedisAddr()
	manualRedisDB := 0
	if opts.DedicatedServices {
		// 专用栈 redis 为本 run 独占实例：逻辑库固定 0，无需全局索引预留。
		r.redisDB = 0
		if os.Getenv("E2E_REDIS_DB") != "" {
			t.Logf("warn: DedicatedServices 模式忽略 E2E_REDIS_DB（专用栈 redis 已是独占实例）")
		}
	} else {
		manualRedisDB = e2eManualRedisDBOverride(t)
		r.redisDB, err = allocRedisIndex(uuid, manualRedisDB)
		require.NoError(t, err, "分配 Redis 逻辑库（1..15 预留文件）")
	}
	r.flushRedis(t)

	// 5) registry state=resources_ready（PG/Redis 已分配并写入 allocated uuid）。
	r.record.PGDB = dbName
	r.record.RedisDB = r.redisDB
	r.record.AllocatedRedisDB = r.redisDB
	if !opts.DedicatedServices {
		r.record.LockUUID = uuid
		r.record.ManualOverride = manualRedisDB > 0
	}
	require.NoError(t, transitionRunRecord(r.record, stateResourcesReady), "registry state=resources_ready")
	return r
}

// e2eManualRedisDBOverride E2E_REDIS_DB 手动覆盖（仅手动单 run 调试；CI/并发验收禁止）。
func e2eManualRedisDBOverride(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("E2E_REDIS_DB"))
	if raw == "" {
		return 0
	}
	var db int
	_, err := fmt.Sscanf(raw, "%d", &db)
	require.NoError(t, err, "E2E_REDIS_DB 必须是整数：%q", raw)
	t.Logf("warn: E2E_REDIS_DB=%d 手动覆盖 Redis 逻辑库（仅单 run 调试；并发验收禁止）", db)
	return db
}

// buildBinaries 每 run 只构建一次 server/fakeupstream（lazy；任何 gateway/fakeUpstream
// 首次使用时触发 => 场景不可能漏构建）。build 继承完整 env（私有模块 auth 有效）。
func (r *e2eRun) buildBinaries(t *testing.T) {
	t.Helper()
	r.buildOnce.Do(func() {
		root, err := repoRootDir()
		if err != nil {
			r.buildErr = err
			return
		}
		exe := ""
		if runtime.GOOS == "windows" {
			exe = ".exe"
		}
		r.srvBin = filepath.Join(r.tmp, "server"+exe)
		r.upBin = filepath.Join(r.tmp, "fakeupstream"+exe)
		started := time.Now()
		if r.buildErr = e2eGoBuild(root, r.srvBin, "./cmd/server"); r.buildErr != nil {
			return
		}
		r.buildErr = e2eGoBuild(root, r.upBin, "./tools/fakeupstream")
		t.Logf("harness 计时：run 级 build（server+fakeupstream）%s", time.Since(started).Round(time.Millisecond))
	})
	require.NoError(t, r.buildErr, "run 级二进制构建")
}

// e2eGoBuild go build 包装（完整 env 继承；失败带 combined output 尾部）。
func e2eGoBuild(root, out, pkg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), e2eBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, pkg)
	cmd.Dir = root
	cmd.Env = os.Environ()
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build %s: %w (%s)", pkg, err, tailText(string(combined), 4000))
	}
	return nil
}

// startChild 启动被测子进程并立即登记 registry（PID/PGID/creation_time + Job 归属），
// 早于 readiness 轮询（spec §9.3）；任何子进程 env 都经 deny-list 过滤。
func (r *e2eRun) startChild(t *testing.T, kind, bin string, args []string, logPath string) *child {
	t.Helper()
	logFile, err := os.Create(logPath)
	require.NoError(t, err, "创建子进程日志文件 %s", logPath)
	cmd := exec.Command(bin, args...)
	cmd.Env = subprocessEnv()
	setProcAttrs(cmd)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start(), "启动 %s", kind)
	rec := childProcess{
		PID:  cmd.Process.Pid,
		PGID: cmd.Process.Pid, // Unix：Setpgid 后组 id = pid；Windows 无 PGID 语义（Job 回收）
		Kind: kind,
	}
	if ct, ok := procCreationTime(rec.PID); ok {
		rec.CreationTime = ct
	}
	if r.job != nil {
		if err := r.job.assign(rec.PID); err != nil {
			// spec §10.9：Assign 可能失败（进程已退出/竞态）=> warn 并走 creation-time + 逐 PID 降级。
			t.Logf("warn: Job Object assign pid=%d 失败，降级逐 PID 强杀：%v", rec.PID, err)
		}
	}
	if err := addChildProcess(r.record, rec); err != nil {
		_ = forceKillTree(rec)
		_ = logFile.Close()
		t.Fatalf("registry 登记子进程 pid=%d: %v", rec.PID, err)
	}
	r.seq++
	c := &child{kind: kind, cmd: cmd, rec: rec, logPath: logPath, logFile: logFile, done: make(chan struct{})}
	go c.monitor()
	r.children = append(r.children, c)
	return c
}

// noteChildExit 子进程退出后摘除 registry 登记（失败只 warn：进程已退出时以
// registry 记录更保守是可接受的，cleanup 会删除整条记录）。
func (r *e2eRun) noteChildExit(t *testing.T, c *child) {
	if c == nil {
		return
	}
	if err := removeChildProcess(r.record, c.rec.PID); err != nil {
		t.Logf("warn: 摘除 registry 子进程 pid=%d: %v", c.rec.PID, err)
	}
}

// killChild 强杀单个子进程并等待退出（返回是否确认退出）。
func (r *e2eRun) killChild(c *child) bool {
	if c == nil || c.exited() {
		return true
	}
	_ = forceKillTree(c.rec)
	_, ok := c.wait(e2eKillWaitTimeout)
	return ok
}

// migrateUp run 级 lazy migrate（唯一入口；首次常驻 gateway 前执行一次）。
func (r *e2eRun) migrateUp(t *testing.T, cfgPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eMigrateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.srvBin, "-config", cfgPath, "-migrate", "up")
	cmd.Env = subprocessEnv() // 迁移进程同样不得继承 C3API_*/E2E_*（配置只来自 TOML）
	started := time.Now()
	combined, err := cmd.CombinedOutput()
	require.NoError(t, err, "run 级 lazy migrate up 失败：%s", tailText(string(combined), 8000))
	t.Logf("harness 计时：run 级 lazy migrate up %s", time.Since(started).Round(time.Millisecond))
	r.migrated = true
}

// migrateOnce 首次常驻 gateway 前执行（迁移只属于 harness；场景不可能漏）。
func (r *e2eRun) migrateOnce(t *testing.T, cfgPath string) {
	t.Helper()
	if r.migrated {
		return
	}
	r.migrateUp(t, cfgPath)
}

// gatewayReadyz 默认就绪判据：/readyz 200（唯一摘流信号；schema gate 在此表达）。
func gatewayReadyz(baseURL string) bool {
	resp, err := e2eHTTPClient.Get(baseURL + "/readyz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode == http.StatusOK
}

// waitGatewayReady 有界轮询就绪（子进程提前退出 => 立即失败，不空等）。
func (r *e2eRun) waitGatewayReady(t *testing.T, baseURL string, ready func(string) bool, c *child, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(e2eReadyPollInterval)
	defer tick.Stop()
	for {
		if ready(baseURL) {
			return true
		}
		if c.exited() {
			return false
		}
		if !time.Now().Before(deadline) {
			return false
		}
		<-tick.C
	}
}

// gateway 启动常驻网关（lazy build + run 级 lazy migrate + 动态端口 + 就绪轮询）；
// ExpectExit 网关单独走 startExpectExitGateway（不可作为首个 gateway）。
func (r *e2eRun) gateway(t *testing.T, spec gatewaySpec) *gateway {
	t.Helper()
	r.buildBinaries(t)
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("gateway-%d", len(r.gateways)+1)
	}
	require.NotNil(t, spec.Config, "gateway %s 必须提供 Config 渲染函数", spec.Name)
	if spec.ExpectExit != nil {
		require.True(t, r.migrated,
			"ExpectExit 网关不能是首个 gateway：先启动一个常驻 gateway 完成 run 级 lazy migrate（spec §3.2）")
		return r.startExpectExitGateway(t, spec)
	}
	gw := r.startReadyGateway(t, spec)
	r.gateways = append(r.gateways, gw)
	return gw
}

// startReadyGateway 首次启动：动态端口 + 最多 3 次换端口重试；成功后 registry ready。
func (r *e2eRun) startReadyGateway(t *testing.T, spec gatewaySpec) *gateway {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= e2eGatewayStartAttempts; attempt++ {
		addr, err := allocLoopbackAddr()
		require.NoError(t, err, "分配动态端口")
		inputs := gatewayInputs{Addr: addr, DSN: r.dsn, RedisAddr: r.redisAddr, RedisDB: r.redisDB}
		cfgText := spec.Config(inputs)
		require.NotEmpty(t, cfgText, "gateway %s 的 Config 返回空 TOML", spec.Name)
		cfgPath := filepath.Join(r.tmp, fmt.Sprintf("%s-%d.toml", sanitizeFileName(spec.Name), attempt))
		require.NoError(t, os.WriteFile(cfgPath, []byte(cfgText), 0o644), "写 gateway 配置")
		r.migrateOnce(t, cfgPath)
		if r.record.State == stateResourcesReady {
			require.NoError(t, transitionRunRecord(r.record, stateSpawning), "registry state=spawning")
		}
		logPath := filepath.Join(r.logDir, sanitizeFileName(spec.Name)+".log")
		c := r.startChild(t, "gateway:"+spec.Name, r.srvBin, []string{"-config", cfgPath}, logPath)
		ready := spec.Ready
		if ready == nil {
			ready = gatewayReadyz
		}
		baseURL := "http://" + addr
		started := time.Now()
		if r.waitGatewayReady(t, baseURL, ready, c, e2eGatewayReadyTimeout) {
			t.Logf("harness 计时：gateway %s 首次启动就绪 %s（addr=%s）", spec.Name, time.Since(started).Round(time.Millisecond), addr)
			if r.record.State == stateSpawning {
				require.NoError(t, transitionRunRecord(r.record, stateReady), "registry state=ready")
			}
			return &gateway{
				run: r, name: spec.Name, Addr: addr, BaseURL: baseURL,
				ConfigPath: cfgPath, LogPath: logPath, inputs: inputs, ready: ready, child: c,
			}
		}
		lastErr = fmt.Errorf("gateway %s 启动失败（尝试 %d/%d，addr=%s）：%s",
			spec.Name, attempt, e2eGatewayStartAttempts, addr, readLogTail(logPath, e2eLogTailBytes))
		if !r.killChild(c) {
			t.Logf("warn: 启动失败的 gateway 子进程 pid=%d 强杀后未确认退出", c.rec.PID)
		}
		r.noteChildExit(t, c)
	}
	t.Fatalf("%v", lastErr)
	return nil
}

// startExpectExitGateway 启动一个应自行退出的进程并断言退出码/日志包含/超时
// （spec §3.2：不可作为首个 gateway；启动 PID 写入 registry 后清除）。
func (r *e2eRun) startExpectExitGateway(t *testing.T, spec gatewaySpec) *gateway {
	t.Helper()
	addr, err := allocLoopbackAddr()
	require.NoError(t, err, "分配动态端口")
	inputs := gatewayInputs{Addr: addr, DSN: r.dsn, RedisAddr: r.redisAddr, RedisDB: r.redisDB}
	cfgPath := filepath.Join(r.tmp, sanitizeFileName(spec.Name)+".toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(spec.Config(inputs)), 0o644), "写 ExpectExit 配置")
	logPath := filepath.Join(r.logDir, sanitizeFileName(spec.Name)+".log")
	c := r.startChild(t, "expectexit:"+spec.Name, r.srvBin, []string{"-config", cfgPath}, logPath)
	timeout := spec.ExpectExit.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	res, ok := c.wait(timeout)
	if !ok {
		_ = forceKillTree(c.rec)
		_, _ = c.wait(e2eKillWaitTimeout)
		r.noteChildExit(t, c)
		t.Fatalf("ExpectExit gateway %s 未在 %s 内退出；日志尾部：%s",
			spec.Name, timeout, readLogTail(logPath, e2eLogTailBytes))
	}
	r.noteChildExit(t, c)
	require.Equal(t, spec.ExpectExit.Code, res.Code,
		"ExpectExit gateway %s 退出码不符；Err=%v；日志尾部：%s", spec.Name, res.Err, readLogTail(logPath, e2eLogTailBytes))
	if spec.ExpectExit.Contains != "" {
		logText := readLogTail(logPath, 64<<10)
		require.Contains(t, logText, spec.ExpectExit.Contains,
			"ExpectExit gateway %s 日志必须包含 %q", spec.Name, spec.ExpectExit.Contains)
	}
	return &gateway{
		run: r, name: spec.Name, Addr: addr, BaseURL: "http://" + addr,
		ConfigPath: cfgPath, LogPath: logPath, inputs: inputs,
		expectExit: true, exitCode: res.Code,
	}
}

// Stop 优雅停机：平台 graceful 信号 -> 等待 -> 超时强杀；退出码必须 0。
func (g *gateway) Stop(t *testing.T) {
	t.Helper()
	if g.expectExit {
		return
	}
	if g.child == nil || g.child.exited() {
		g.run.noteChildExit(t, g.child)
		return
	}
	require.NoError(t, gracefulTerminate(g.child.cmd), "gateway %s 优雅停机信号", g.name)
	res, ok := g.child.wait(e2eGracefulStopTimeout)
	if !ok {
		t.Logf("warn: gateway %s 未在 %s 内优雅退出，强杀", g.name, e2eGracefulStopTimeout)
		_ = forceKillTree(g.child.rec)
		res, ok = g.child.wait(e2eKillWaitTimeout)
	}
	require.True(t, ok, "gateway %s 未确认退出", g.name)
	g.run.noteChildExit(t, g.child)
	require.Equal(t, 0, res.Code, "gateway %s 优雅退出码（Err=%v）", g.name, res.Err)
}

// Kill 强杀（模拟崩溃）：等待 Wait 返回（任意退出码）。
func (g *gateway) Kill(t *testing.T) {
	t.Helper()
	if g.expectExit {
		return
	}
	if g.child == nil || g.child.exited() {
		g.run.noteChildExit(t, g.child)
		return
	}
	require.NoError(t, forceKillTree(g.child.rec), "gateway %s 强杀", g.name)
	res, ok := g.child.wait(e2eKillWaitTimeout)
	require.True(t, ok, "gateway %s 强杀后未确认退出", g.name)
	g.run.noteChildExit(t, g.child)
	_ = res
}

// Restart 复用原地址/原配置重启（同端口 bind 3s 内重试；失败 fail+dump，不静默换端口）。
func (g *gateway) Restart(t *testing.T) *gateway {
	t.Helper()
	r := g.run
	if g.child != nil && !g.child.exited() {
		g.Stop(t)
	} else if g.child != nil {
		r.noteChildExit(t, g.child)
	}
	deadline := time.Now().Add(e2eRestartBindBudget)
	for {
		c := r.startChild(t, "gateway:"+g.name, r.srvBin, []string{"-config", g.ConfigPath}, g.LogPath)
		if r.waitGatewayReady(t, g.BaseURL, g.ready, c, e2eGatewayReadyTimeout) {
			g.child = c
			return g
		}
		if !r.killChild(c) {
			t.Logf("warn: 重启失败的 gateway 子进程 pid=%d 强杀后未确认退出", c.rec.PID)
		}
		r.noteChildExit(t, c)
		if time.Now().After(deadline) {
			t.Fatalf("gateway %s 重启后 %s 内未在 %s 恢复就绪（必须复用原地址，不静默换端口）；日志尾部：%s",
				g.name, e2eRestartBindBudget, g.Addr, readLogTail(g.LogPath, e2eLogTailBytes))
		}
	}
}

// Exited 是否已退出（ExpectExit 网关恒 true）。
func (g *gateway) Exited() bool {
	if g.expectExit {
		return true
	}
	return g.child == nil || g.child.exited()
}

// ExitCode 已知退出码（未退出 = -1；ExpectExit 网关为实际退出码）。
func (g *gateway) ExitCode() int {
	if g.expectExit {
		return g.exitCode
	}
	if g.child == nil || !g.child.exited() {
		return -1
	}
	res, _ := g.child.wait(0)
	return res.Code
}

// fakeUpstream 真实 fakeupstream 进程句柄（地址由 harness 分配）。
type fakeUpstream struct {
	Addr    string
	LogPath string
	run     *e2eRun
	child   *child
}

// fakeUpstream 启动真实 fakeupstream（不迁移）：harness 分配 127.0.0.1:0 端口 ->
// 进程绑定 -> TCP 就绪轮询；bind 竞争时换端口重试（最多 3 次）。
func (r *e2eRun) fakeUpstream(t *testing.T, spec upstreamSpec) *fakeUpstream {
	t.Helper()
	r.buildBinaries(t)
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("fakeupstream-%d", r.seq+1)
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		addr, err := allocLoopbackAddr()
		require.NoError(t, err, "分配 fakeupstream 动态端口")
		args := append([]string{"-addr", addr}, spec.Flags...)
		logPath := filepath.Join(r.logDir, sanitizeFileName(spec.Name)+".log")
		c := r.startChild(t, "fakeupstream:"+spec.Name, r.upBin, args, logPath)
		if waitTCPReady(addr, e2eUpstreamReadyTimeout, c) {
			return &fakeUpstream{Addr: addr, LogPath: logPath, run: r, child: c}
		}
		lastErr = fmt.Errorf("fakeupstream %s 未就绪（尝试 %d/3，addr=%s）：%s",
			spec.Name, attempt, addr, readLogTail(logPath, e2eLogTailBytes))
		_ = r.killChild(c)
		r.noteChildExit(t, c)
	}
	t.Fatalf("%v", lastErr)
	return nil
}

// waitTCPReady 有界轮询 TCP 监听成功（子进程提前退出 => 立即失败）。
func waitTCPReady(addr string, timeout time.Duration, c *child) bool {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(e2eReadyPollInterval)
	defer tick.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if c != nil && c.exited() {
			return false
		}
		if !time.Now().Before(deadline) {
			return false
		}
		<-tick.C
	}
}

// bootstrapAdmin 幂等引导 platform_admin：fresh DB 首个注册即 admin；已 bootstrap
// （多 gateway/多实例/重启）走登录。缓存按 gateway 地址（JWT 同 secret 可复用）。
//
// 多实例语义：后起实例的鉴权快照在启动时加载，若 bootstrap 用户是之后才注册的，
// /api/admin/settings 会 fail-closed 401 直到快照重新加载（生产语义，不是 flake）——
// 因此这里**有界轮询** platform_admin 判定，而不是一次性断言。
func (r *e2eRun) bootstrapAdmin(t *testing.T, gw *gateway) string {
	t.Helper()
	if token := r.adminJWT[gw.Addr]; token != "" {
		return token
	}
	const (
		email    = "e2e-bootstrap@loadtest.test"
		password = "loadtest-pass-1"
	)
	post := func(path string) (string, int) {
		body := []byte(`{"email":"` + email + `","password":"` + password + `"}`)
		resp, err := http.Post(gw.BaseURL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			return "", 0
		}
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Token, resp.StatusCode
	}
	token, code := post("/api/user/auth/register")
	if token == "" || code == http.StatusConflict {
		// 409 email exists ≠ 已 bootstrap（该邮箱可能只是普通用户）：登录后校验。
		token, _ = post("/api/user/auth/login")
	}
	require.NotEmpty(t, token, "bootstrap 未取得 JWT（register/login 失败）")

	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var lastCode int
	for {
		req, err := http.NewRequest(http.MethodGet, gw.BaseURL+"/api/admin/settings", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e2eHTTPClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			lastCode = resp.StatusCode
			if lastCode == http.StatusOK {
				r.adminJWT[gw.Addr] = token
				return token
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("bootstrap user is not platform_admin（30s 内 /api/admin/settings 非 200，last=%d）", lastCode)
		}
		<-tick.C
	}
}

// env 构造 e2eEnv 兼容视图（P0 过渡：迁移后的场景继续复用既有管理面 helper，
// 只有资源/进程生命周期改由 harness 独占）。P1 迁移 billing 时把 e2eEnv/helper 搬进 harness。
func (r *e2eRun) env(gw *gateway) *e2eEnv {
	return &e2eEnv{t: r.t, pg: r.pgPool, tmp: r.tmp, addr: gw.Addr, adminJWT: r.bootstrapAdmin(r.t, gw)}
}

// pg 本 run 唯一库的 pool（场景 SQL 断言唯一入口）。
func (r *e2eRun) pg(t *testing.T) *pgxpool.Pool {
	t.Helper()
	require.NotNil(t, r.pgPool, "run PG pool 未初始化")
	return r.pgPool
}

// redisClient 本 run 逻辑库的 Redis 客户端（redisx 是唯一构造点）。
func (r *e2eRun) redisClient(t *testing.T) *redis.Client {
	t.Helper()
	if r.redisCli != nil {
		return r.redisCli
	}
	cli, err := redisx.Open(redisx.Options{Addr: r.redisAddr, DB: r.redisDB})
	require.NoError(t, err, "打开本 run Redis 逻辑库 %d", r.redisDB)
	r.redisCli = cli
	return cli
}

// flushRedis 清空本 run 逻辑库（回收的 stale 逻辑库可能残留旧 run 的易失态）。
func (r *e2eRun) flushRedis(t *testing.T) {
	t.Helper()
	cli := r.redisClient(t)
	require.NoError(t, cli.FlushDB(context.Background()).Err(), "flush 本 run Redis 逻辑库 %d", r.redisDB)
}

// resources 本 run 唯一资源快照（PG 库/Redis 逻辑库/最后一个 gateway 地址/专用栈 project）。
func (r *e2eRun) resources() resourcesSnapshot {
	snap := resourcesSnapshot{PGDB: r.dbName, RedisDB: r.redisDB}
	if len(r.gateways) > 0 {
		snap.Addr = r.gateways[len(r.gateways)-1].Addr
	}
	if r.dedicated != nil {
		snap.ComposeProject = r.dedicated.project
	}
	return snap
}

// registryRecord 读回磁盘上的 registry 记录（自测断言/排障用）。
func (r *e2eRun) registryRecord(t *testing.T) *runRecord {
	t.Helper()
	rec, err := readRunRecord(runRecordPath(r.uuid))
	require.NoError(t, err, "读 registry 记录")
	return rec
}

// tailText 见 harness_compose.go。

// readLogTail 读日志文件尾部（失败诊断不整段刷屏；文件不存在 = 空串）。
func readLogTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size == 0 {
		return ""
	}
	if int64(n) < size {
		if _, err := f.Seek(size-int64(n), io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// dumpDiagnostics 失败诊断：先 dump server/fakeupstream/专用栈日志，再清理资源
// （spec §3.2 cleanup 顺序：失败先 dump）。
func (r *e2eRun) dumpDiagnostics(t *testing.T) {
	t.Helper()
	for _, gw := range r.gateways {
		if text := readLogTail(gw.LogPath, e2eLogTailBytes); text != "" {
			t.Logf("--- gateway %s (%s) 日志尾部 ---%s", gw.name, gw.LogPath, text)
		}
	}
	for _, c := range r.children {
		if strings.HasPrefix(c.kind, "fakeupstream:") {
			if text := readLogTail(c.logPath, e2eLogTailBytes); text != "" {
				t.Logf("--- %s 日志尾部 ---%s", c.kind, text)
			}
		}
	}
	if r.dedicated != nil {
		t.Logf("--- 专用栈 logs ---%s", r.dedicated.Logs(t))
	}
}

// cleanup 反向生命周期：失败先 dump -> 强杀全部子进程/Job -> 池/库/栈/Redis 锁/
// registry/temp。失败只 warn（不把 cleanup 抖动放大成测试失败；registry 记录保留
// 供下次 sweep 兜底——除非记录本身也删不掉）。
func (r *e2eRun) cleanup(t *testing.T) {
	t.Helper()
	if r.closed {
		return
	}
	r.closed = true
	t.Logf("harness 计时：run %s 资源生命周期总耗时 %s（pg=%s redis=%d gateways=%d）",
		r.uuid[:8], time.Since(r.t0).Round(time.Millisecond), r.dbName, r.redisDB, len(r.gateways))
	if t.Failed() {
		r.dumpDiagnostics(t)
	}
	// 1) 强杀全部存活子进程（先逐 PID + 进程组，再 Job 兜底）。
	for _, c := range r.children {
		if c.exited() {
			continue
		}
		if !r.killChild(c) {
			t.Logf("warn: 子进程 %s pid=%d 强杀后未确认退出", c.kind, c.rec.PID)
		}
	}
	if r.job != nil {
		if err := r.job.terminate(); err != nil {
			t.Logf("warn: Job Object terminate: %v", err)
		}
		if err := r.job.close(); err != nil {
			t.Logf("warn: Job Object close: %v", err)
		}
	}
	// 2) 关闭场景 pool（LIFO），随后回收库/栈。
	if r.pgPool != nil {
		r.pgPool.Close()
		r.pgPool = nil
	}
	cleanupOK := true
	if r.dedicated != nil {
		// 库随 compose down -v 的卷一起消失。
		if err := r.dedicated.Close(t); err != nil {
			t.Logf("warn: 专用栈清理失败，保留 registry 记录（下次 sweep 重试）：%v", err)
			cleanupOK = false
		}
	} else if r.adminPool != nil && r.dbName != "" {
		if err := r.dropOwnDatabase(t); err != nil {
			t.Logf("warn: 本 run 库清理失败，保留 registry 记录（下次 sweep 重试）：%v", err)
			cleanupOK = false
		}
	}
	if r.adminPool != nil {
		r.adminPool.Close()
		r.adminPool = nil
	}
	// 3) Redis 客户端 + 逻辑库预留（owner uuid 校验）。
	if r.redisCli != nil {
		_ = r.redisCli.Close()
		r.redisCli = nil
	}
	if !r.opts.DedicatedServices && r.redisDB >= e2eRedisDBMin {
		if err := releaseRedisIndex(r.redisDB, r.uuid); err != nil {
			t.Logf("warn: 释放 Redis 逻辑库预留失败：%v", err)
		}
	}
	// 4) registry 记录：资源回收成功才删除；失败保留（state=finishing 供下次 sweep 兜底）。
	if cleanupOK {
		if err := transitionRunRecord(r.record, stateFinishing); err != nil {
			t.Logf("warn: registry state=finishing: %v", err)
		}
		if err := os.Remove(runRecordPath(r.uuid)); err != nil && !os.IsNotExist(err) {
			t.Logf("warn: 删除 registry 记录失败（sweep 兜底）：%v", err)
		}
	}
	// 5) temp 目录（严格校验后删除）。
	if runTempDirValid(r.tmp) {
		if err := os.RemoveAll(r.tmp); err != nil {
			t.Logf("warn: 删除 run temp 目录失败：%v", err)
		}
	}
}

// dropOwnDatabase 本 run 库清理（WITH (FORCE) 兜底）；失败返回 error => cleanup 保留
// registry 记录，下一次 stale sweep 重试（绝不静默泄漏库且不留证据）。
func (r *e2eRun) dropOwnDatabase(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var active int
	if err := r.adminPool.QueryRow(ctx,
		"SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()", r.dbName).Scan(&active); err == nil && active > 0 {
		t.Logf("warn: 本 run 库 %s 仍有 %d 个连接，DROP DATABASE WITH (FORCE) 兜底", r.dbName, active)
	}
	if _, err := r.adminPool.Exec(ctx,
		"DROP DATABASE IF EXISTS "+pgx.Identifier{r.dbName}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("DROP 本 run 库 %s: %w", r.dbName, err)
	}
	return nil
}

// harnessDeadPID 取一个「确定已死亡」的 PID（spawn 短命进程并等它退出）。
// 用 Wait 返回确保不是僵尸：sweep 看到的必须是确定死亡（certain=true）。
func harnessDeadPID(t *testing.T) int {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "0")
	} else {
		cmd = exec.Command("true")
	}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())
	alive, certain := procAlive(pid)
	require.True(t, certain, "短命进程 pid=%d 死亡状态必须确定", pid)
	require.False(t, alive, "短命进程 pid=%d 应已死亡", pid)
	return pid
}

// selftestSyntheticRecord 预置一条合成 registry 记录 + 对应真实 PG 库/Redis 逻辑库
// 预留（AV-A/AV-S2 的最小复现基座；不启动任何进程）。
func selftestSyntheticRecord(t *testing.T, run *e2eRun, state runState, pid int, start time.Time) *runRecord {
	t.Helper()
	uuid, err := newRunUUID()
	require.NoError(t, err, "合成 run uuid")
	dbName, err := newPGDatabaseName()
	require.NoError(t, err, "合成 PG 库名")
	require.NoError(t, createPGDatabase(context.Background(), run.services.PGAdminDSN(), dbName), "合成记录 CREATE DATABASE")
	redisDB, err := allocRedisIndex(uuid, 0)
	require.NoError(t, err, "合成记录 Redis 索引预留")
	hostname, _ := os.Hostname()
	tmp, err := os.MkdirTemp("", e2eRunTmpDirPrefix+"synthetic-")
	require.NoError(t, err, "合成记录 temp 目录")
	rec := &runRecord{
		UUID:             uuid,
		PID:              pid,
		Hostname:         hostname,
		Start:            start,
		State:            state,
		PlannedPGDB:      dbName,
		PGDB:             dbName,
		RedisDB:          redisDB,
		AllocatedRedisDB: redisDB,
		LockUUID:         uuid,
		TmpDir:           tmp,
	}
	require.NoError(t, saveRunRecord(rec), "写合成 registry 记录")
	// 断言中途 Fatal 时尽力回收（幂等；sweep 可能已回收，故全部吞错）。
	t.Cleanup(func() {
		_ = os.Remove(runRecordPath(uuid))
		_ = releaseRedisIndex(redisDB, uuid)
		_ = dropStaleDatabase(dbName)
		if runTempDirValid(tmp) {
			_ = os.RemoveAll(tmp)
		}
	})
	return rec
}

// harnessPGDatabaseExists 库是否存在（连 base admin 池查 pg_database，不依赖业务库）。
func harnessPGDatabaseExists(t *testing.T, run *e2eRun, dbName string) bool {
	t.Helper()
	var exists bool
	err := run.adminPool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", dbName).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// testHarnessStaleReclaim AV-A 最小验证（真实 PG/Redis 资源 + 合成 registry 记录）：
//  1. 活 PID + 旧时间戳 => 记录/库/锁全部保留（活 run 永不被回收，无论 age）；
//  2. 死 PID + 旧时间戳 + 无活跃连接 => 记录/库/锁全部回收（先 CAS 独占再清理）；
//  3. 死 PID + 旧时间戳 + 有活跃连接 => 只 warn 保留；断开后再 sweep 回收。
func testHarnessStaleReclaim(t *testing.T) {
	run := newE2ERun(t, e2eRunOptions{})
	sweepLog := func(format string, args ...any) { t.Logf("sweep: "+format, args...) }
	old := time.Now().Add(-1 * time.Hour)

	// 1) 活 PID（本测试进程）+ 旧时间戳 + state=ready => 永不回收。
	live := selftestSyntheticRecord(t, run, stateReady, os.Getpid(), old)
	stats := sweepStaleRuns(sweepLog)
	require.GreaterOrEqual(t, stats.Kept, 1, "活 run 记录必须计入 kept")
	require.True(t, harnessFileExists(runRecordPath(live.UUID)), "活 PID 记录永不回收")
	require.True(t, harnessPGDatabaseExists(t, run, live.PGDB), "活 run 的库保留")
	lockLive, err := readRedisLock(live.AllocatedRedisDB)
	require.NoError(t, err)
	require.NotNil(t, lockLive, "活 run 的 Redis 锁保留")
	require.Equal(t, live.UUID, lockLive.UUID)

	// 2) 死 PID + 旧时间戳 + 无活跃连接 => 回收（记录/库/锁）。
	dead := selftestSyntheticRecord(t, run, stateReady, harnessDeadPID(t), old)
	sweepStaleRuns(sweepLog)
	require.False(t, harnessFileExists(runRecordPath(dead.UUID)), "死 run 记录必须被回收")
	require.False(t, harnessPGDatabaseExists(t, run, dead.PGDB), "死 run 的库必须 DROP")
	lockDead, err := readRedisLock(dead.AllocatedRedisDB)
	require.NoError(t, err)
	require.Nil(t, lockDead, "死 run 的 Redis 锁必须删除")

	// 3) 死 PID + 旧时间戳 + 有活跃连接 => 只 warn 保留；断开后再 sweep 回收。
	busy := selftestSyntheticRecord(t, run, stateReady, harnessDeadPID(t), old)
	busyDSN, err := replaceDSNDatabase(run.services.PGAdminDSN(), busy.PGDB)
	require.NoError(t, err)
	busyPool, err := pgxpool.New(context.Background(), busyDSN)
	require.NoError(t, err)
	require.NoError(t, busyPool.Ping(context.Background()))
	sweepStaleRuns(sweepLog)
	require.True(t, harnessFileExists(runRecordPath(busy.UUID)), "有活跃连接 => 记录保留（只 warn）")
	require.True(t, harnessPGDatabaseExists(t, run, busy.PGDB), "有活跃连接 => 库保留")
	lockBusy, err := readRedisLock(busy.AllocatedRedisDB)
	require.NoError(t, err)
	require.NotNil(t, lockBusy, "有活跃连接 => Redis 锁保留")
	busyPool.Close()
	sweepStaleRuns(sweepLog)
	require.False(t, harnessFileExists(runRecordPath(busy.UUID)), "连接断开后死 run 被回收")
	require.False(t, harnessPGDatabaseExists(t, run, busy.PGDB))

	t.Logf("AV-A 最小验证通过：活 run 保留（库=%s redis=%d）/ 死 run 回收 / 活跃连接保留后回收",
		live.PGDB, live.AllocatedRedisDB)
}
