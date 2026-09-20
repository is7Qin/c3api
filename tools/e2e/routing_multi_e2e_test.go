// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e 多实例智能路由端到端测试（charter Task 25）：两网关进程共享
// PG + Redis（distinct 端口，same JWT secret + admin token）+ 双 fakeupstream
//（双 failure domain）+ 真实 outage 演练（docker stop/start）。
//
// 运行（前置：PG 127.0.0.1:15432，Redis 127.0.0.1:16379，docker CLI 可用；
// 测试以 postgres 维护库自建 c3api_routing_multi_e2e 库）：
//
//	$env:TEST_DATABASE_URL="postgres://postgres:c3api@127.0.0.1:15432/postgres?sslmode=disable"
//	$env:C3API_REDIS_ADDR="127.0.0.1:16379"
//	go test -tags e2e -count=1 -run TestIntelligentRoutingMultiInstanceE2E ./tools/e2e -v -timeout 1200s
//
// 与 billing/routing 单实例同包：复用 e2eEnv/adminToken/jwtSecret/pollUntil/
// waitSnapshot/createUser/userKey/putPrice/jsonGet/stopGracefully/waitExit +
// rt 前缀路由 helpers（rtPlan/rtFindRoute/rtWaitRoute/rtWaitGenBump/rtPollLong/
// rtSetRuleEnabled/rtCreateRule/rtRevision/rtAccountRead/rtAuditEntry）。
// 本文件新增符号一律 rm 前缀，禁与 billing/rt 文件重名。
// 生产代码零修改：断言失败只报现象 + 证据，不弱化不断言、不修生产代码。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/discovery"
	"github.com/is7qin/c3api/pkg/redisx"
)

const (
	rmAddrA = "127.0.0.1:18094" // 双实例端口：与 billing(18090-18092)/单实例(18093) 均错开
	rmAddrB = "127.0.0.1:18095"
	rmUp1   = "127.0.0.1:19112" // failure domain 1（tpl1 账号群）
	rmUp2   = "127.0.0.1:19113" // failure domain 2（incident 坏域；与单实例 19111 错开）
	rmDB    = "c3api_routing_multi_e2e"
	rmModel = "rm-model"

	rmKeyA     = "rm-key-a"     // 健康账号 A（gMain，dx 域，成本 ×1.0）
	rmKeyB     = "rm-key-b"     // 健康账号 B（gMain，dy 域，成本 ×1.0）
	rmKeyStorm = "rm-key-storm" // 恒 429（gStorm，无规则——风暴只产 unmatched 事件）
	rmKeyFail  = "rm-key-fail"  // 恒 500（gFail，fail_account 规则）
	rmKeyIA    = "rm-key-ia"    // incident 好域账号（gI，上游 up1）
	rmKeyIB1   = "rm-key-ib1"   // incident 坏域账号 1（gI，上游 up2，恒 500）
	rmKeyIB2   = "rm-key-ib2"   // incident 坏域账号 2（gI，上游 up2，恒 500）

	rmRedisContainer = "deploy-redis-1"
	rmPGContainer    = "deploy-db-1"
	rmMembersKey     = "c3api:cluster:members"
)

// rmPortFree 端口占用预检（僵尸进程禁静默复用，与 rtPortFree 同纪律）。
func rmPortFree(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		_ = c.Close()
		t.Fatalf("端口 %s 已被占用（疑似僵尸进程），先清理再跑", addr)
	}
}

// rmCluster 双实例簇句柄：共享 DB/Redis，上游与 server 进程统一收殓。
type rmCluster struct {
	pg        *pgxpool.Pool
	rc        *redis.Client
	adminDSN  string
	redisAddr string
	dsn       string
	tmpBase   string
	upBin     string
	srvBin    string
	up1       *exec.Cmd
	up2       *exec.Cmd
	srvA      *exec.Cmd
	srvB      *exec.Cmd
	envA      *e2eEnv
	envB      *e2eEnv
	tmpA      string
	tmpB      string
	procs     []*exec.Cmd
}

// rmKillAll 存活进程全杀（t.Cleanup 兜底，无僵尸无残留端口）。
func (c *rmCluster) rmKillAll() {
	for _, p := range c.procs {
		if p == nil || p.Process == nil {
			continue
		}
		if p.ProcessState != nil && p.ProcessState.Exited() {
			continue
		}
		_ = p.Process.Kill()
		_ = p.Wait()
	}
}

// rmForget 从收殓表摘除（优雅退出后已 Wait，无需再 Kill）。
func (c *rmCluster) rmForget(target *exec.Cmd) {
	kept := c.procs[:0]
	for _, p := range c.procs {
		if p != target {
			kept = append(kept, p)
		}
	}
	c.procs = kept
}

// rmKillHard 硬杀（模拟 crash：不走 Close/ZREM，discovery 走 memberTTL 剪除）。
func (c *rmCluster) rmKillHard(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// rmBoot 新鲜库 + 空 Redis + 双 fakeupstream + 实例 A；B 由场景按需后起
//（暖启动场景要求 B 在质量数据存在后才 boot）。
func rmBoot(t *testing.T) *rmCluster {
	t.Helper()
	for _, a := range []string{rmAddrA, rmAddrB, rmUp1, rmUp2} {
		rmPortFree(t, a)
	}
	ctx := context.Background()
	redisAddr := os.Getenv("C3API_REDIS_ADDR")
	require.NotEmpty(t, redisAddr, "C3API_REDIS_ADDR is required (e.g. 127.0.0.1:16379)")

	c := &rmCluster{redisAddr: redisAddr}

	rc, err := redisx.Open(redisx.Options{Addr: redisAddr})
	require.NoError(t, err, "redis open")
	c.rc = rc
	t.Cleanup(func() { _ = rc.Close() })
	require.NoError(t, rc.FlushDB(ctx).Err(), "harness redis isolation flush")

	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://postgres:c3api@localhost:15432/postgres"
	}
	c.adminDSN = adminDSN
	adminPool, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+rmDB+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `CREATE DATABASE `+rmDB)
	require.NoError(t, err)
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i+1] + rmDB
		if !strings.Contains(dsn, "?") {
			dsn += "?sslmode=disable"
		}
	}
	c.dsn = dsn
	pg, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	c.pg = pg

	c.tmpBase, err = os.MkdirTemp("", "rm-e2e-*")
	require.NoError(t, err, "mktemp")
	// 收殓序（LIFO 逆序执行）：先 RemoveAll 注册、后 Kill 注册 →  teardown
	// 时先杀进程再删目录（Windows 文件锁要求；t.TempDir 会反序故不用它）。
	t.Cleanup(func() { _ = os.RemoveAll(c.tmpBase) })
	t.Cleanup(c.rmKillAll)
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	build := func(pkg, name string) string {
		out := filepath.Join(c.tmpBase, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = repoRoot
		cmd.Stderr = os.Stderr
		require.NoError(t, cmd.Run(), "build %s", pkg)
		return out
	}
	upBin := build("./tools/fakeupstream", "rm-fakeupstream.exe")
	srvBin := build("./cmd/server", "rm-server.exe")
	c.upBin, c.srvBin = upBin, srvBin

	// up1：storm-429 + fail-500；up2：incident 坏域恒 500（静态 flag，
	// 坏域从 boot 起即坏——gI 在 incident 阶段前不打流量，current 窗口干净）。
	startUp := func(addr, name, fail429, fail500 string) *exec.Cmd {
		up := exec.Command(upBin, "-addr", addr, "-chunks", "5", "-latency", "2ms",
			"-fail429", fail429, "-fail500", fail500)
		up.Stdout, up.Stderr = os.Stdout, os.Stderr
		require.NoError(t, up.Start(), "start %s", name)
		c.procs = append(c.procs, up)
		return up
	}
	c.up1 = startUp(rmUp1, "up1", rmKeyStorm, rmKeyFail)
	c.up2 = startUp(rmUp2, "up2", "", rmKeyIB1+","+rmKeyIB2)

	c.envA, c.srvA, c.tmpA = rmStartInstance(t, c, rmAddrA, "rm-a")
	return c
}

// rmStartInstance 以独立 tmp/config/log 起一个网关实例（同库同 Redis 同 secret）。
func rmStartInstance(t *testing.T, c *rmCluster, addr, tag string) (*e2eEnv, *exec.Cmd, string) {
	t.Helper()
	tmp := filepath.Join(c.tmpBase, tag)
	require.NoError(t, os.MkdirAll(tmp, 0o755))
	cfg := fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
admin = { token = "%s" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
redis = { addr = "%s" }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 3, usage_capture = true }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "3s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "5s" }
billing = { enabled = true, flush_interval = "300ms", balance_refresh_interval = "500ms" }
`, addr, adminToken, jwtSecret, c.dsn, c.redisAddr)
	cfgPath := filepath.Join(tmp, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	srv := exec.Command(c.srvBin, "-config", cfgPath)
	srvLog, err := os.Create(filepath.Join(tmp, "server.log"))
	require.NoError(t, err)
	srv.Stdout, srv.Stderr = srvLog, srvLog
	if isWindows() {
		srv.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	}
	require.NoError(t, srv.Start(), "start instance %s", tag)
	c.procs = append(c.procs, srv)
	env := &e2eEnv{t: t, pg: c.pg, addr: addr}
	env.tmp = tmp
	rmWaitReady(t, env, tmp, tag)
	// 失败诊断：server.log 转储。
	t.Cleanup(func() {
		if !t.Failed() {
			_ = srvLog.Close()
			return
		}
		_ = srvLog.Sync()
		if data, err := os.ReadFile(filepath.Join(tmp, "server.log")); err == nil {
			tail := data
			if len(tail) > 20000 {
				tail = tail[len(tail)-20000:]
			}
			t.Logf("--- %s server.log tail (test failed) ---\n%s", tag, tail)
		}
		_ = srvLog.Close()
	})
	return env, srv, tmp
}

// rmWaitReady 轮询 /api/admin/settings 直到 200（migrate + 分区 bootstrap）。
func rmWaitReady(t *testing.T, env *e2eEnv, tmp, tag string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodGet, env.adminURL("/settings"), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if !time.Now().Before(deadline) {
			if data, err := os.ReadFile(filepath.Join(tmp, "server.log")); err == nil {
				t.Logf("--- %s server.log ---\n%s", tag, data)
			}
			t.Fatalf("实例 %s 未在 90s 内就绪", tag)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// rmStopGraceful 优雅停机（CTRL_BREAK/SIGTERM + Wait 零退出码），成功后摘收殓。
func rmStopGraceful(t *testing.T, c *rmCluster, cmd **exec.Cmd, what string) {
	t.Helper()
	require.NoError(t, stopGracefully(*cmd), "graceful %s", what)
	waitExit(t, *cmd, 30*time.Second)
	c.rmForget(*cmd)
	*cmd = nil
}

// rmChat 流式 chat（stream=true：TTFT 样本 + 确定性计费），状态码不断言由调用方定。
func rmChat(t *testing.T, env *e2eEnv, key, model string, extra map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for k, v := range extra {
		body[k] = v
	}
	return env.aiReq(http.MethodPost, "/v1/chat/completions", key, body)
}

// rmChatRaw 无 require 的 chat（风暴 goroutine 用：传输错误走返回值，不
// FailNow；调用方记账+预算断言，避免工作 goroutine 内 Goexit 误判为挂起）。
func rmChatRaw(env *e2eEnv, key, model string, extra map[string]any) (int, string, error) {
	body := map[string]any{"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for k, v := range extra {
		body[k] = v
	}
	var rd *bytes.Reader
	b, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	rd = bytes.NewReader(b)
	req, err := http.NewRequest(http.MethodPost, env.aiURL("/v1/chat/completions"), rd)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String(), nil
}

// rmAudit fakeupstream /_audit 全拉（双域各自审计，不复用写死单地址的 rtAudit）。
func rmAudit(t *testing.T, upAddr string) []rtAuditEntry {
	t.Helper()
	resp, err := http.Get("http://" + upAddr + "/_audit")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out []rtAuditEntry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// rmAuditMaxSeq 审计水位（增量窗口起点）。
func rmAuditMaxSeq(t *testing.T, upAddr string) int64 {
	t.Helper()
	var m int64
	for _, e := range rmAudit(t, upAddr) {
		if e.Seq > m {
			m = e.Seq
		}
	}
	return m
}

// rmAuditHits 按 (key,status) 统计 seqAfter 之后条目。
func rmAuditHits(t *testing.T, upAddr string, seqAfter int64, key string, status int) int {
	t.Helper()
	n := 0
	for _, e := range rmAudit(t, upAddr) {
		if e.Seq > seqAfter && e.Key == key && e.Status == status {
			n++
		}
	}
	return n
}

// rmOpsWorkers GET /ops/workers → name→stats 映射。
func rmOpsWorkers(t *testing.T, env *e2eEnv) map[string]map[string]any {
	t.Helper()
	code, rb := env.admin(http.MethodGet, "/ops/workers", nil)
	require.Equal(t, 200, code, "get ops workers: %s", rb)
	v := jsonGet(t, rb, "").(map[string]any)
	out := map[string]map[string]any{}
	for _, w := range v["workers"].([]any) {
		m, _ := w.(map[string]any)
		name, _ := m["name"].(string)
		st, _ := m["stats"].(map[string]any)
		if name != "" && st != nil {
			out[name] = st
		}
	}
	return out
}

// rmInstances 某实例视角 discovery 活体数（instances 字段，冻结语义由 last_tick_ok 辨）。
func rmInstances(t *testing.T, env *e2eEnv) (int, bool) {
	t.Helper()
	st := rmOpsWorkers(t, env)["discovery"]
	require.NotNil(t, st, "ops 须含 discovery worker")
	ins, _ := st["instances"].(float64)
	ok, _ := st["last_tick_ok"].(bool)
	return int(ins), ok
}

// rmWaitInstances 有界轮询直到该实例视角活体数 == want 且 tick 健康。
func rmWaitInstances(t *testing.T, env *e2eEnv, want int, what string, timeout time.Duration) {
	t.Helper()
	rtPollLong(t, what, timeout, func() (bool, string) {
		got, ok := rmInstances(t, env)
		return got == want && ok, fmt.Sprintf("instances=%d want=%d tick_ok=%v", got, want, ok)
	})
}

// rmMembers Redis ZSET 活体成员（discovery 真值；ops 只暴露计数不暴露名单）。
func rmMembers(t *testing.T, c *rmCluster) []string {
	t.Helper()
	m, err := c.rc.ZRange(context.Background(), rmMembersKey, 0, -1).Result()
	require.NoError(t, err)
	return m
}

// rmWaitMembers 轮询直到 ZSET 成员数 == want。
func rmWaitMembers(t *testing.T, c *rmCluster, want int, what string, timeout time.Duration) {
	t.Helper()
	rtPollLong(t, what, timeout, func() (bool, string) {
		got := rmMembers(t, c)
		return len(got) == want, fmt.Sprintf("members=%v want=%d", got, want)
	})
}

// rmDocker docker stop/start（outage 演练；调用方保证 start 回补 + 健康轮询）。
func rmDocker(t *testing.T, op, container string) {
	t.Helper()
	cmd := exec.Command("docker", op, container)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "docker %s %s: %s", op, container, buf.String())
}

// rmWaitRedisUp 轮询直到 Redis 可 Ping（Open 自带 Ping）。
func rmWaitRedisUp(t *testing.T, addr, what string, timeout time.Duration) {
	t.Helper()
	rtPollLong(t, what, timeout, func() (bool, string) {
		rc, err := redisx.Open(redisx.Options{Addr: addr})
		if err != nil {
			return false, err.Error()
		}
		_ = rc.Close()
		return true, "redis ping ok"
	})
}

// rmWaitPGUp 轮询直到 PG 可 SELECT 1（新池直连，绕开旧池断线态）。
func rmWaitPGUp(t *testing.T, adminDSN, what string, timeout time.Duration) {
	t.Helper()
	rtPollLong(t, what, timeout, func() (bool, string) {
		p, err := pgxpool.New(context.Background(), adminDSN)
		if err != nil {
			return false, err.Error()
		}
		defer p.Close()
		var one int
		if err := p.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil {
			return false, err.Error()
		}
		return one == 1, "pg select 1 ok"
	})
}

// rmTCPDown 轮询直到 TCP 不可达（容器已停的外部可观测信号）。
func rmTCPDown(t *testing.T, addr, what string, timeout time.Duration) {
	t.Helper()
	rtPollLong(t, what, timeout, func() (bool, string) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return true, "tcp refused/closed"
		}
		_ = c.Close()
		return false, addr + " 仍可连接"
	})
}

// rmQualityInstance DB 直读实例层某指纹 attempts/successes（live+flush 未分区感知求和）。
func rmQualityInstance(t *testing.T, c *rmCluster, fpHex string) (att, suc int64) {
	t.Helper()
	_ = c.pg.QueryRow(context.Background(), `
		SELECT COALESCE(sum(attempts),0), COALESCE(sum(successes),0)
		FROM routing_quality_instance_minute WHERE encode(candidate_fingerprint,'hex')=$1`, fpHex).Scan(&att, &suc)
	return att, suc
}

// rmPlanCandidatesOf 取 plan 中指定组路由候选集合（证据记录用，nil 安全）。
func rmPlanCandidatesOf(plan map[string]any, groupID int64) any {
	r := rtFindRoute(plan, groupID, "openai-chat", rmModel)
	if r == nil {
		return "route-missing"
	}
	ids, _ := rmPlanCandidates(r)
	return ids
}

// rmPlanCandidates 取 plan 路由候选 account_id 集合 + 指纹映射。
func rmPlanCandidates(route map[string]any) (map[int64]bool, map[int64]string) {
	ids := map[int64]bool{}
	fps := map[int64]string{}
	for _, cc := range route["candidates"].([]any) {
		m, _ := cc.(map[string]any)
		if aid, ok := m["account_id"].(float64); ok {
			ids[int64(aid)] = true
			if fp, ok := m["fingerprint"].(string); ok {
				fps[int64(aid)] = fp
			}
		}
	}
	return ids, fps
}

// rmPlanPrimary 取 plan 路由 primary 名单。
func rmPlanPrimary(route map[string]any) []int64 {
	var out []int64
	for _, id := range route["primary"].([]any) {
		if f, ok := id.(float64); ok {
			out = append(out, int64(f))
		}
	}
	return out
}

// rmIncident 取 plan 路由 incident 面（mark-and-expose 证据字段）。
func rmIncident(route map[string]any) map[string]any {
	m, _ := route["incident"].(map[string]any)
	return m
}

// TestIntelligentRoutingMultiInstanceE2E 双实例智能路由全场景（charter Task 25）：
// 暖启动(PG 真相) → rendezvous 单 probe owner → 亲和跨实例确定 → 硬连续跨实例 →
// 规则风暴不拖垮 → fail 快速摘除 + NOTIFY 跨实例 → Redis  outage 保 plan/连续 fail-closed →
// PG outage 实时继续 → 实例死亡 TTL → 滚动/全重启 → MODEL/DOMAIN incident 标记只暴露。
func TestIntelligentRoutingMultiInstanceE2E(t *testing.T) {
	c := rmBoot(t)
	envA := c.envA
	ctx := context.Background()

	// ============ seed（经 A；B 后起，NOTIFY 收敛到 B 是被测行为之一） ============
	putPrice(t, envA, rmModel, map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})
	tpl1 := envA.create("/templates", map[string]any{
		"name": "rm-tpl1", "base_url": "http://" + rmUp1,
		"supported_formats": []string{"openai-chat", "openai-responses"},
		"models":            []string{rmModel},
	})
	tpl2 := envA.create("/templates", map[string]any{
		"name": "rm-tpl2", "base_url": "http://" + rmUp2,
		"supported_formats": []string{"openai-chat"},
		"models":            []string{rmModel},
	})
	gMain := envA.create("/groups", map[string]any{"name": "rm-g-main"})
	gStorm := envA.create("/groups", map[string]any{"name": "rm-g-storm"})
	gFail := envA.create("/groups", map[string]any{"name": "rm-g-fail"})
	gI := envA.create("/groups", map[string]any{"name": "rm-g-incident"})
	// 就绪门 canary（NOTIFY 订阅前丢失导致 stale 的 fail-fast，与单实例同纪律）。
	g0 := envA.create("/groups", map[string]any{"name": "rm-g0-ready"})
	envA.create("/accounts", map[string]any{
		"name": "rm-canary", "template_id": tpl1, "upstream_key": rmKeyA,
		"group_ids": []int64{g0},
	})
	rtWaitRoute(t, envA, g0, "openai-chat", rmModel)
	t.Log("就绪门通过：canary 候选已发布")
	accA := envA.create("/accounts", map[string]any{
		"name": "rm-a", "template_id": tpl1, "upstream_key": rmKeyA,
		"group_ids": []int64{gMain}, "cache_domain": "dx.example",
	})
	accB := envA.create("/accounts", map[string]any{
		"name": "rm-b", "template_id": tpl1, "upstream_key": rmKeyB,
		"group_ids": []int64{gMain}, "cache_domain": "dy.example",
	})
	_ = envA.create("/accounts", map[string]any{
		"name": "rm-storm", "template_id": tpl1, "upstream_key": rmKeyStorm,
		"group_ids": []int64{gStorm},
	})
	accFail := envA.create("/accounts", map[string]any{
		"name": "rm-fail", "template_id": tpl1, "upstream_key": rmKeyFail,
		"group_ids": []int64{gFail},
	})
	accIA := envA.create("/accounts", map[string]any{
		"name": "rm-ia", "template_id": tpl1, "upstream_key": rmKeyIA,
		"group_ids": []int64{gI},
	})
	accIB1 := envA.create("/accounts", map[string]any{
		"name": "rm-ib1", "template_id": tpl2, "upstream_key": rmKeyIB1,
		"group_ids": []int64{gI},
	})
	accIB2 := envA.create("/accounts", map[string]any{
		"name": "rm-ib2", "template_id": tpl2, "upstream_key": rmKeyIB2,
		"group_ids": []int64{gI},
	})
	uMain := createUser(t, envA, "rm-u-main@example.com", 100.0)
	_, kMain := userKey(t, envA, uMain, gMain)
	uStorm := createUser(t, envA, "rm-u-storm@example.com", 50.0)
	_, kStorm := userKey(t, envA, uStorm, gStorm)
	uFail := createUser(t, envA, "rm-u-fail@example.com", 20.0)
	_, kFail := userKey(t, envA, uFail, gFail)
	uI := createUser(t, envA, "rm-u-i@example.com", 50.0)
	_, kI := userKey(t, envA, uI, gI)
	// fail_account 规则（gFail 恒 500 首错即死；gStorm 故意无规则——风暴只走
	// unmatched 快路径，保“请求健康”归因纯粹）。
	rtCreateRule(t, envA, "rm-kill-5xx", 805,
		map[string]any{"kind": "5xx", "account_id": accFail, "count_failure_ge": 1, "window_seconds": 120},
		map[string]any{"fail_account": true})
	// 种子遮蔽：seed-429/seed-5xx 无窗口首错即中，禁之保归因（单实例同款隔离手段）。
	rtSetRuleEnabled(t, envA, "seed-429", false)
	rtSetRuleEnabled(t, envA, "seed-5xx", false)
	waitSnapshot()
	for _, g := range []int64{gMain, gStorm, gFail, gI} {
		rtWaitRoute(t, envA, g, "openai-chat", rmModel)
	}
	t.Log("种子就绪：四组路由候选已发布")

	// ============ 1. 质量收敛（A 单实例）→ PG 暖启动 B ============
	// B 在质量数据存在后才 boot；boot 前 FlushDB 证明 Redis 质量非持久真相。
	t.Log("阶段 1：A 收敛 → PG rollup 落盘 → FlushDB → B 暖启动")
	gen0, _ := rtPlan(t, envA)
	iters := 0
	rtPollLong(t, "A 首候选充分（DB 实例层 n>=30）", 300*time.Second, func() (bool, string) {
		iters++
		if iters > 60 {
			return false, "60 轮仍无候选充分"
		}
		for i := 0; i < 10; i++ {
			if code, rb := rmChat(t, envA, kMain, rmModel, nil); code != 200 {
				return false, fmt.Sprintf("warmup req 异常 status=%d body=%.120s", code, rb)
			}
		}
		best := 0
		rows, err := c.pg.Query(ctx, `
			SELECT COALESCE(sum(attempts),0) FROM routing_quality_instance_minute GROUP BY encode(candidate_fingerprint,'hex')`)
		if err != nil {
			return false, err.Error()
		}
		defer rows.Close()
		for rows.Next() {
			var n int64
			if err := rows.Scan(&n); err != nil {
				break
			}
			if int(n) > best {
				best = int(n)
			}
		}
		return best >= 30, fmt.Sprintf("iter=%d best=%d", iters, best)
	})
	var primA []int64
	rtPollLong(t, "A 事件驱动重编译 primary 非空", 120*time.Second, func() (bool, string) {
		_, plan := rtPlan(t, envA)
		r := rtFindRoute(plan, gMain, "openai-chat", rmModel)
		if r == nil {
			return false, "主路由暂不可见"
		}
		primA = rmPlanPrimary(r)
		gen, _ := rtPlan(t, envA)
		return len(primA) > 0, fmt.Sprintf("gen=%d primary=%v", gen, primA)
	})
	genA, _ := rtPlan(t, envA)
	require.Greater(t, genA, gen0, "质量 influx 必须推进 generation")
	// PG 真相断言：rollup 表有行（B 恢复的数据源）；行数>0 即可，live 另计。
	var rollupN int64
	require.NoError(t, c.pg.QueryRow(ctx, `SELECT count(*) FROM routing_quality_rollup`).Scan(&rollupN))
	t.Logf("A 收敛：primary=%v rollup_rows=%d（%d 轮）", primA, rollupN, iters)
	// Redis 质量非真相：清掉全部易失态后再起 B（discovery 心跳 1s 自愈）。
	require.NoError(t, c.rc.FlushDB(ctx).Err(), "warm-start 前 FlushDB")
	c.envB, c.srvB, c.tmpB = rmStartInstance(t, c, rmAddrB, "rm-b")
	envB := c.envB
	// B 从 PG 恢复：路由可见 + primary 非空（与 A 同 PG 真相收敛到同 plan 态）。
	var primB []int64
	rtPollLong(t, "B 暖启动 plan 恢复（primary 非空）", 180*time.Second, func() (bool, string) {
		gen, plan := rtPlan(t, envB)
		r := rtFindRoute(plan, gMain, "openai-chat", rmModel)
		if r == nil {
			return false, fmt.Sprintf("gen=%d 路由暂不可见", gen)
		}
		primB = rmPlanPrimary(r)
		return len(primB) > 0, fmt.Sprintf("gen=%d primary=%v", gen, primB)
	})
	require.ElementsMatch(t, primA, primB, "B 暖启动 primary 须与 A 一致（同 PG 真相）: A=%v B=%v", primA, primB)
	t.Logf("暖启动 OK：B primary=%v 与 A 一致（FlushDB 后恢复，Redis 非真相）", primB)

	// ============ 2. member rendezvous 单 probe owner ============
	// 可观测面：ops 只暴露计数（instances），成员名单读 Redis ZSET；owner 身份
	// 无任何 API 暴露——断言 = 双边成员一致 + 纯函数 rendezvous 恰出一主 +
	// PROBING→READY 进展（零 owner 则永不 READY）。
	t.Log("阶段 2：rendezvous 单 probe owner")
	rmWaitInstances(t, envA, 2, "A 视角成员=2", 30*time.Second)
	rmWaitInstances(t, envB, 2, "B 视角成员=2", 30*time.Second)
	rmWaitMembers(t, c, 2, "ZSET 成员=2", 30*time.Second)
	members := rmMembers(t, c)
	require.Len(t, members, 2, "ZSET 须恰 2 成员")
	owner := discovery.RendezvousOwner("rm:probe:health", members)
	require.Contains(t, members, owner, "owner 须为成员之一")
	require.Equal(t, owner, discovery.RendezvousOwner("rm:probe:health", members), "rendezvous 须确定性")
	t.Logf("成员一致：%v，probe owner=%s（恰一）", members, owner)
	// PROBING→READY 进展：失能再恢复 accB（rev+1 → PROBING），owner 探针放行。
	gen, _ := rtPlan(t, envA)
	_, _ = envA.admin(http.MethodPatch, fmt.Sprintf("/accounts/%d", accB), map[string]any{
		"enabled": false,
	})
	gen = rtWaitGenBump(t, envA, gen, "B 失能重编译")
	_, _ = envA.admin(http.MethodPatch, fmt.Sprintf("/accounts/%d", accB), map[string]any{
		"enabled": true,
	})
	rtWaitGenBump(t, envA, gen, "B 恢复重编译")
	// 恢复后候选归位即探针链路活着（双实例下恰一 owner  probing，无双主冲突）。
	rtPollLong(t, "B 候选归位（owner 探针链路活）", 90*time.Second, func() (bool, string) {
		_, plan := rtPlan(t, envA)
		r := rtFindRoute(plan, gMain, "openai-chat", rmModel)
		if r == nil {
			return false, "路由暂不可见"
		}
		ids, _ := rmPlanCandidates(r)
		return ids[accA] && ids[accB], fmt.Sprintf("candidates=%v", ids)
	})
	t.Log("rendezvous OK：双边 members=2 + 恰一 owner + 探针链路活")

	// ============ 3. soft affinity 跨实例稳定域 ============
	// 自校准：A 上找命中 A/B 键的 pck，再到 B 上复现——同 plan 同 ring 须同账号。
	t.Log("阶段 3：soft affinity 跨实例确定性")
	calib := map[string]string{} // wantKey -> pck
	mark := rmAuditMaxSeq(t, rmUp1)
	for i := 0; i < 40 && len(calib) < 2; i++ {
		pck := fmt.Sprintf("rm-aff-%d", i)
		code, rb := rmChat(t, envA, kMain, rmModel, map[string]any{"prompt_cache_key": pck})
		require.Equal(t, 200, code, "calib req: %s", rb)
		for _, k := range []string{rmKeyA, rmKeyB} {
			if _, done := calib[k]; !done && rmAuditHits(t, rmUp1, mark, k, 200) > 0 {
				calib[k] = pck
				mark = rmAuditMaxSeq(t, rmUp1)
			}
		}
	}
	require.Len(t, calib, 2, "40 个候选 pck 须覆盖 A/B 双键（各至少命中一次）")
	for wantKey, pck := range calib {
		after := rmAuditMaxSeq(t, rmUp1)
		code, rb := rmChat(t, envB, kMain, rmModel, map[string]any{"prompt_cache_key": pck})
		require.Equal(t, 200, code, "B 复现 pck=%s: %s", pck, rb)
		got := ""
		for _, e := range rmAudit(t, rmUp1) {
			if e.Seq > after && e.Status == 200 {
				got = e.Key
			}
		}
		require.Equal(t, wantKey, got, "同 pck 在 B 须落同账号（A 侧 %s）：pck=%s", wantKey, pck)
	}
	t.Logf("亲和跨实例 OK：%v 双边同账号", calib)

	// ============ 4. 硬连续跨实例（共享 Redis binding） ============
	t.Log("阶段 4：硬连续跨实例 + fail-closed")
	respCall := func(env *e2eEnv, extra map[string]any) (int, string) {
		body := map[string]any{"model": rmModel, "input": "hi"}
		for k, v := range extra {
			body[k] = v
		}
		return env.aiReq(http.MethodPost, "/v1/responses", kMain, body)
	}
	code, rb := respCall(envA, nil)
	require.Equal(t, 200, code, "A 建连续首呼：%s", rb)
	rid, _ := jsonGet(t, rb, "id").(string)
	require.NotEmpty(t, rid, "首呼须回 id")
	keyOfLastResp := func(upAddr string, after int64) string {
		var last string
		var ms int64 = -1
		for _, e := range rmAudit(t, upAddr) {
			if e.Seq > after && e.Path == "/v1/responses" && e.Status == 200 && e.Seq > ms {
				ms, last = e.Seq, e.Key
			}
		}
		return last
	}
	k0 := keyOfLastResp(rmUp1, 0)
	require.NotEmpty(t, k0, "首呼须有上游归因")
	after := rmAuditMaxSeq(t, rmUp1)
	code, rb = respCall(envB, map[string]any{"input": "follow", "previous_response_id": rid})
	require.Equal(t, 200, code, "B 跨实例跟随须 200：%s", rb)
	require.Equal(t, k0, keyOfLastResp(rmUp1, after), "B 跟随须钉住 A 首呼账号 %s", k0)
	t.Logf("跨实例连续 OK：B 钉住 %s", k0)
	code, rb = respCall(envB, map[string]any{"input": "x", "previous_response_id": "rsp_nope_123"})
	require.Equal(t, 410, code, "未知 binding 跨实例须 410：%s", rb)
	// 过期模拟：删 Redis binding（TTL 24h 等不起；DEL ≡ 过期后的 Lookup miss）。
	n, err := c.rc.Keys(ctx, "c3api:cont:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, n, "须有至少一个 continuation binding")
	require.NoError(t, c.rc.Del(ctx, n...).Err(), "删除 bindings 模拟过期")
	code, rb = respCall(envB, map[string]any{"input": "x", "previous_response_id": rid})
	require.Equal(t, 410, code, "binding 缺失/过期须 fail-closed 410：%s", rb)
	t.Log("硬连续 fail-closed OK（未知/过期 → 410，零上游拨号由 410 语义保证）")

	// ============ 5. 规则队列风暴不拖垮 ============
	// gStorm 恒 429 且无规则：每请求恰一 unmatched 事件；cap 4096（硬编码，
	// 无配置项——测试只读 ops，不调小队列）。核心断言：零挂起 + 健康组仍 200。
	t.Log("阶段 5：规则队列风暴（unmatched 事件洪峰）")
	st0 := rmOpsWorkers(t, envA)["rule-engine"]
	require.NotNil(t, st0, "ops 须暴露 rule-engine")
	require.Equal(t, float64(4096), st0["queue_cap"], "事件队列 cap 须 4096：%v", st0)
	_, hasDrop := st0["admission_dropped"]
	require.True(t, hasDrop, "drop 计数器须可见：%v", st0)
	const stormN, stormConc = 8000, 64
	stormMark := rmAuditMaxSeq(t, rmUp1) // 风暴前水位：上游承压归因用
	type stormRes struct {
		code int
		body string
		err  error
	}
	jobs := make(chan struct{}, stormN)
	res := make(chan stormRes, stormN)
	for i := 0; i < stormConc; i++ {
		go func() {
			for range jobs {
				// 传输级拒接（burst backlog 瞬断）立即重拨一次，不 sleep；
				// 仍失败才记账——与“队列拖垮挂起”（deadline 兜底）严格区分。
				cc, rb, err := rmChatRaw(envA, kStorm, rmModel, nil)
				if err != nil {
					cc, rb, err = rmChatRaw(envA, kStorm, rmModel, nil)
				}
				res <- stormRes{code: cc, body: rb, err: err}
			}
		}()
	}
	for i := 0; i < stormN; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	deadline := time.Now().Add(300 * time.Second)
	codes := map[int]int{}
	samples := map[int][]string{}
	got429, netErr := 0, 0
	var netErrs []string
	for i := 0; i < stormN; i++ {
		select {
		case r := <-res:
			if r.err != nil {
				netErr++
				if len(netErrs) < 5 {
					netErrs = append(netErrs, r.err.Error())
				}
				continue
			}
			codes[r.code]++
			if len(samples[r.code]) < 3 {
				rb := r.body
				if len(rb) > 200 {
					rb = rb[:200]
				}
				samples[r.code] = append(samples[r.code], rb)
			}
			if r.code == 429 {
				got429++
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("风暴 %d/%d 后挂起（队列拖垮）：codes=%v", i, stormN, codes)
		}
	}
	t.Logf("风暴状态码分布：%v 传输错误=%d %v", codes, netErr, netErrs)
	for code, ss := range samples {
		for _, s := range ss {
			t.Logf("风暴样本 status=%d body=%.200s", code, s)
		}
	}
	// 传输错误预算：burst 拨号瞬断 ≤1%（run5 实测单发 refused，重拨后 A 全程
	// 存活——纯测试侧 burst 噪声，非网关拖垮；deadline 仍兜挂起）。
	require.LessOrEqual(t, netErr, stormN/100, "风暴传输错误超预算：%v", netErrs)
	// 健康语义：网关全程有响应（零挂起零传输失败）；429 直透是主路径，
	// 持续 429 触发 health OPEN 后的 429-noavailable/503 同属终端有响应语义
	// （与单候选直透同为“请求健康”，非拖垮）——不断言全 429，只断言风暴
	// 确实到达上游（audit）+ 风暴后健康组仍 200。
	require.GreaterOrEqual(t, got429, 100, "风暴须有 ≥100 直透 429（上游真实承压）：%v", codes)
	upHits := rmAuditHits(t, rmUp1, stormMark, rmKeyStorm, 429)
	require.Greater(t, upHits, 0, "上游须见到风暴 429 尝试：%v", codes)
	st1 := rmOpsWorkers(t, envA)["rule-engine"]
	dropped, _ := st1["admission_dropped"].(float64)
	t.Logf("风暴 OK：%d 请求全完成（429×%d），admission_dropped=%v（>0 则溢出路径被演练，=0 则消费者跟上）", stormN, got429, dropped)
	code, rb = rmChat(t, envA, kMain, rmModel, nil)
	require.Equal(t, 200, code, "风暴后健康组须仍 200：%s", rb)
	t.Log("风暴后健康流量 OK")

	// ============ 6. fail 快速摘除 + NOTIFY 跨实例收敛 ============
	// 摘除的 serving 真相是 selection（快照 disabled 即排除），plan 字节是编译
	// 快照（fail 本身不触发重编译，见本阶段末证据记录）。流量只走 A：
	// B 侧 selection 变化只能来自 NOTIFY（B 零本地事件）——这才是跨实例证明。
	t.Log("阶段 6：fail_account 经 NOTIFY 跨实例摘除（selection 级）")
	_, planB0 := rtPlan(t, envB)
	rB0 := rtFindRoute(planB0, gFail, "openai-chat", rmModel)
	require.NotNil(t, rB0, "B 初始须见 gFail 路由")
	ids0, _ := rmPlanCandidates(rB0)
	require.True(t, ids0[accFail], "B 初始候选须含 fail 账号")
	markFail := rmAuditMaxSeq(t, rmUp1)
	code, rb = rmChat(t, envA, kFail, rmModel, nil)
	require.NotEqual(t, 200, code, "恒 500 首错不得成功：%s", rb)
	require.Equal(t, 1, rmAuditHits(t, rmUp1, markFail, rmKeyFail, 500), "首错须到达上游")
	rtPollLong(t, "fail 落盘（FailedAt）", 60*time.Second, func() (bool, string) {
		m := rtAccountRead(t, envA, accFail)
		return m["FailedAt"] != nil, fmt.Sprintf("FailedAt=%v", m["FailedAt"])
	})
	// A 侧 selection 即时排除（快照 disabled）：后继零上游尝试。
	markFail = rmAuditMaxSeq(t, rmUp1)
	code, rb = rmChat(t, envA, kFail, rmModel, nil)
	require.NotEqual(t, 200, code, "fail 后 A 不得再选中：%s", rb)
	require.Equal(t, markFail, rmAuditMaxSeq(t, rmUp1), "fail 后 A 侧上游须零尝试")
	// B 侧收敛（NOTIFY 唯一通道）：轮询直到 B 亦零上游尝试且非 200。
	rtPollLong(t, "B selection 经 NOTIFY 摘除（B 零本地事件）", 90*time.Second, func() (bool, string) {
		before := rmAuditMaxSeq(t, rmUp1)
		cc, rb := rmChat(t, envB, kFail, rmModel, nil)
		after := rmAuditMaxSeq(t, rmUp1)
		if cc == 200 {
			return false, fmt.Sprintf("B 仍选中 fail 账号且成功 body=%.120s", rb)
		}
		if after != before {
			return false, fmt.Sprintf("B 仍有上游尝试 audit %d→%d status=%d", before, after, cc)
		}
		return true, fmt.Sprintf("B 已排除 status=%d", cc)
	})
	t.Log("NOTIFY 跨实例 OK：A 侧 fail → 双边 selection 摘除（SDK fatal 同链，见 gap）")
	// plan 字节收敛记录（证据级：fail 是否推进 plan 以排除候选——若重编译推进
	// 后候选仍在，即 stale-plan 缺陷，不在此断言，只记录；serving 真相已定）。
	_, planA6 := rtPlan(t, envA)
	_, planB6 := rtPlan(t, envB)
	t.Logf("plan 字节现状 A gFail=%v B gFail=%v（stale 与否见 evidence）",
		rmPlanCandidatesOf(planA6, gFail), rmPlanCandidatesOf(planB6, gFail))

	// ============ 7. Redis outage：保旧 plan + 连续 fail-closed ============
	// 出 половины 前先建新 binding（outage 窗内跟随 → 503 fail-closed 证明）。
	t.Log("阶段 7：Redis outage")
	code, rb = respCall(envA, nil)
	require.Equal(t, 200, code, "outage 前建 binding：%s", rb)
	ridOut, _ := jsonGet(t, rb, "id").(string)
	require.NotEmpty(t, ridOut)
	genPreA, _ := rtPlan(t, envA)
	genPreB, _ := rtPlan(t, envB)
	rmDocker(t, "stop", rmRedisContainer)
	t.Cleanup(func() {
		// outage 纪律：PG/Redis 重启必回补（Cleanup 兜底，幂等 start）。
		_ = exec.Command("docker", "start", rmRedisContainer).Run()
	})
	rmTCPDown(t, c.redisAddr, "redis TCP 关闭", 60*time.Second)
	// 旧 plan 保留 + 请求继续被服务（Redis 只是易失协调；双实例世代各自独立，
	// 只断言不回退——outage 期无新编译是正常冻结，不是倒退）。
	for _, e := range []struct {
		env *e2eEnv
		pre int64
	}{{envA, genPreA}, {envB, genPreB}} {
		gen, plan := rtPlan(t, e.env)
		require.GreaterOrEqual(t, gen, e.pre, "outage 期 plan 不得回退（gen %d < %d）", gen, e.pre)
		require.NotNil(t, rtFindRoute(plan, gMain, "openai-chat", rmModel), "outage 期路由须仍在")
		cc, rrb := rmChat(t, e.env, kMain, rmModel, nil)
		require.Equal(t, 200, cc, "outage 期健康流量须仍 200：%s", rrb)
	}
	// 硬连续 outage 期 fail-closed（Redis miss 读不到 → 503，不迁移不 200）。
	code, rb = respCall(envB, map[string]any{"input": "follow", "previous_response_id": ridOut})
	require.Equal(t, 503, code, "outage 期跟随须 fail-closed 503：%s", rb)
	t.Log("Redis outage OK：双边保 plan + 200，连续 503 fail-closed")
	rmDocker(t, "start", rmRedisContainer)
	rmWaitRedisUp(t, c.redisAddr, "redis 恢复", 90*time.Second)
	rmWaitInstances(t, envA, 2, "redis 恢复后 A 视角=2", 60*time.Second)
	rmWaitInstances(t, envB, 2, "redis 恢复后 B 视角=2", 60*time.Second)
	code, rb = rmChat(t, envA, kMain, rmModel, nil)
	require.Equal(t, 200, code, "redis 恢复后流量须 200：%s", rb)
	// binding 随 Redis 丢失（无持久化）：跟随旧 id → 410；新建 → 200。
	code, rb = respCall(envB, map[string]any{"input": "follow", "previous_response_id": ridOut})
	require.Equal(t, 410, code, "redis 重启后旧 binding 须 410（易失语义）：%s", rb)
	t.Log("Redis 恢复 OK：discovery=2，旧 binding 410（易失），新流量 200")

	// ============ 8. PG outage：实时路径继续，恢复后 resume ============
	t.Log("阶段 8：PG outage")
	var ulogBefore int64
	require.NoError(t, c.pg.QueryRow(ctx, `SELECT count(*) FROM usage_logs`).Scan(&ulogBefore))
	rmDocker(t, "stop", rmPGContainer)
	t.Cleanup(func() {
		_ = exec.Command("docker", "start", rmPGContainer).Run()
	})
	rmTCPDown(t, "127.0.0.1:15432", "pg TCP 关闭", 60*time.Second)
	// 编译后 plan + 内存态：实时选号继续（不断言 admin/DB 面，只断言 AI 面）。
	outageEnd := time.Now().Add(45 * time.Second)
	for time.Now().Before(outageEnd) {
		for _, env := range []*e2eEnv{envA, envB} {
			cc, rrb := rmChat(t, env, kMain, rmModel, nil)
			if cc != 200 {
				t.Logf("PG outage 期非 200（记录，不断言单次）：status=%d body=%.120s", cc, rrb)
			}
		}
		// 进程存活检查：若 outage 打崩 server，这是生产缺陷（ evidence 在 log）。
		for _, p := range []*exec.Cmd{c.srvA, c.srvB} {
			if p != nil && p.ProcessState != nil && p.ProcessState.Exited() {
				t.Fatalf("PG outage 打崩 server 进程（生产缺陷嫌疑）：%v", p.ProcessState)
			}
		}
	}
	// outage 窗内至少多数成功（实时继续；flush 失败只影响落库时效不影响选号）。
	okWin := 0
	for i := 0; i < 10; i++ {
		if cc, _ := rmChat(t, envA, kMain, rmModel, nil); cc == 200 {
			okWin++
		}
		if cc, _ := rmChat(t, envB, kMain, rmModel, nil); cc == 200 {
			okWin++
		}
	}
	require.GreaterOrEqual(t, okWin, 10, "PG outage 期双边 20 请求须至少半数 200（实时继续）：%d", okWin)
	t.Logf("PG outage 期实时继续 OK：20 请求中 200×%d", okWin)
	rmDocker(t, "start", rmPGContainer)
	rmWaitPGUp(t, c.adminDSN, "pg 恢复", 120*time.Second)
	// 恢复：管理面 + 新流量 + usage 落库 resume。
	rtPollLong(t, "pg 恢复后 admin 可读", 90*time.Second, func() (bool, string) {
		cc, rb := envA.admin(http.MethodGet, "/settings", nil)
		return cc == 200, fmt.Sprintf("status=%d body=%.120s", cc, rb)
	})
	c2, rb := rmChat(t, envA, kMain, rmModel, nil)
	require.Equal(t, 200, c2, "pg 恢复后流量须 200：%s", rb)
	rtPollLong(t, "usage 落库 resume（行数增长）", 90*time.Second, func() (bool, string) {
		var n int64
		if err := c.pg.QueryRow(ctx, `SELECT count(*) FROM usage_logs`).Scan(&n); err != nil {
			return false, err.Error()
		}
		return n > ulogBefore, fmt.Sprintf("usage_logs %d→%d", ulogBefore, n)
	})
	t.Log("PG 恢复 OK：admin + 流量 + 落库 resume")

	// ============ 9. 实例死亡 TTL + 滚动/全重启 ============
	t.Log("阶段 9：实例死亡 TTL（硬杀 B，无 ZREM）")
	c.rmKillHard(c.srvB)
	c.srvB = nil
	// 15s memberTTL + 心跳节拍：30s 预算剪除（graceful Close 只需 ~2s，crash 走 TTL）。
	rmWaitMembers(t, c, 1, "ZSET 剪除 B（TTL 路径）", 40*time.Second)
	rmWaitInstances(t, envA, 1, "A 视角收敛到 1", 40*time.Second)
	// 幸存者继续服务（死亡不拖垮对端）。
	for i := 0; i < 5; i++ {
		cc, rrb := rmChat(t, envA, kMain, rmModel, nil)
		require.Equal(t, 200, cc, "对端死亡期 A 须仍 200：%s", rrb)
	}
	t.Log("死亡 TTL OK：剪除 + 幸存者 200")
	// B 重启 = 滚动重启恢复：PG 重载 plan republishes。
	c.envB, c.srvB, c.tmpB = rmStartInstance(t, c, rmAddrB, "rm-b2")
	envB = c.envB
	rmWaitMembers(t, c, 2, "B 重启后 ZSET=2", 30*time.Second)
	rmWaitInstances(t, envA, 2, "A 视角回到 2", 30*time.Second)
	rtPollLong(t, "B 重启后 plan republish", 120*time.Second, func() (bool, string) {
		gen, plan := rtPlan(t, envB)
		r := rtFindRoute(plan, gMain, "openai-chat", rmModel)
		if r == nil {
			return false, fmt.Sprintf("gen=%d 路由暂不可见", gen)
		}
		return len(rmPlanPrimary(r)) > 0, fmt.Sprintf("gen=%d primary=%v", gen, rmPlanPrimary(r))
	})
	t.Log("滚动重启 OK：B republish + members=2")
	// A 优雅滚动重启（B 在线服务不断）：停 A 期 B 全 200。
	rmStopGraceful(t, c, &c.srvA, "A 滚动重启")
	for i := 0; i < 5; i++ {
		cc, rrb := rmChat(t, envB, kMain, rmModel, nil)
		require.Equal(t, 200, cc, "A 重启期 B 须仍 200：%s", rrb)
	}
	c.envA, c.srvA, c.tmpA = rmStartInstance(t, c, rmAddrA, "rm-a2")
	envA = c.envA
	rmWaitInstances(t, envA, 2, "A 重启后视角=2", 30*time.Second)
	rtWaitRoute(t, envA, gMain, "openai-chat", rmModel)
	t.Log("A 滚动重启 OK")
	// 全重启（双停双起）：记录黑窗，只允许 plan-not-ready 503 语义内的恢复。
	rmStopGraceful(t, c, &c.srvA, "全重启停 A")
	rmStopGraceful(t, c, &c.srvB, "全重启停 B")
	c.envA, c.srvA, c.tmpA = rmStartInstance(t, c, rmAddrA, "rm-a3")
	envA = c.envA
	c.envB, c.srvB, c.tmpB = rmStartInstance(t, c, rmAddrB, "rm-b3")
	envB = c.envB
	seenCodes := map[int]bool{}
	rtPollLong(t, "全重启后双边 200（黑窗仅容 503）", 180*time.Second, func() (bool, string) {
		ca, _ := rmChat(t, envA, kMain, rmModel, nil)
		cb, _ := rmChat(t, envB, kMain, rmModel, nil)
		seenCodes[ca] = true
		seenCodes[cb] = true
		okA := ca == 200
		okB := cb == 200
		if !okA || !okB {
			// 黑窗内只容忍 plan-not-ready 503（+Retry-After），其它码直接失败。
			for _, code := range []int{ca, cb} {
				if code != 200 && code != 503 {
					return false, fmt.Sprintf("黑窗出现非 503 码 status=%d（A=%d B=%d）", code, ca, cb)
				}
			}
			return false, fmt.Sprintf("恢复中 A=%d B=%d", ca, cb)
		}
		return okA && okB, "双边 200"
	})
	t.Logf("全重启 OK：双边 200，黑窗见码=%v（仅容 503）", seenCodes)

	// ============ 10. MODEL/DOMAIN incident（mark-and-expose） ============
	// 构造：gI 三候选取自双域（IA@up1 好域；IB1/IB2@up2 坏域恒 500）。
	// baseline = 回填 settled 历史（生产 24h 压缩为直接写同表同查询路径）；
	// current = 实时 live + settled。比较器：Wilson 区间不重叠。
	t.Log("阶段 10：DOMAIN incident（坏域双候选 vs 好域单候选）")
	_, planI0 := rtPlan(t, envA)
	rI0 := rtFindRoute(planI0, gI, "openai-chat", rmModel)
	require.NotNil(t, rI0, "gI 路由须在 plan 中")
	inc0 := rmIncident(rI0)
	require.NotNil(t, inc0, "plan 路由须带 incident 面")
	require.Equal(t, false, inc0["active"], "incident 初始须 inactive：%v", inc0)
	idsI, fpsI := rmPlanCandidates(rI0)
	require.True(t, idsI[accIA] && idsI[accIB1] && idsI[accIB2], "gI 候选须齐三员：%v", idsI)
	routeHex, _ := rI0["ref"].(map[string]any)["route_class_id"].(string)
	require.NotEmpty(t, routeHex, "gI 路由缺 route_class_id")
	// 先打健康流量取 (identity_version, quality_class_id)（同 format+model 跨组同类）。
	for i := 0; i < 12; i++ {
		if cc, rb := rmChat(t, envA, kMain, rmModel, nil); cc != 200 {
			t.Fatalf("incident 基线流量异常：%d %s", cc, rb)
		}
	}
	var ver int16
	var qc []byte
	require.NoError(t, c.pg.QueryRow(ctx, `
		SELECT identity_version, quality_class_id
		FROM routing_quality_rollup LIMIT 1`).Scan(&ver, &qc),
		"须有至少一行 live rollup 行以取 version/class")
	// route_class 必须取 gI 路由自身的 route_class_id（plan ref 面 hex）——
	// baseline/current 查询都按 (route, fp) 精确键匹配，错路即零基线。
	// 回填 baseline：三候选 × 三分钟（M-62/-61/-60，落在 [M-24h,M-5m) 内），
	// 45/45 全胜（Wilson LCB≈0.92）——与坏域 current 0% 拉开可判定差距。
	nowM := time.Now().UTC().Truncate(time.Minute)
	fpOf := map[int64]string{accIA: fpsI[accIA], accIB1: fpsI[accIB1], accIB2: fpsI[accIB2]}
	for _, acc := range []int64{accIA, accIB1, accIB2} {
		require.NotEmpty(t, fpOf[acc], "候选 %d 缺 fingerprint", acc)
		for _, back := range []time.Duration{62, 61, 60} {
			bm := nowM.Add(-back * time.Minute)
			_, err := c.pg.Exec(ctx, `
				INSERT INTO routing_quality_rollup (identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute, attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images, updated_at)
				VALUES ($1,decode($2,'hex'),$3,decode($4,'hex'),$5, 15,15, 0,0,0,0, 0,0,0, ARRAY[0,0,0,0,0,0,0,0,0,0]::bigint[], 0,0,0,0, 0,0, now())
				ON CONFLICT (bucket_minute, candidate_fingerprint, quality_class_id, route_class_id, identity_version)
				DO UPDATE SET attempts=15, successes=15, updated_at=now()`, ver, routeHex, qc, fpOf[acc], bm)
			require.NoError(t, err, "回填 baseline acc=%d minute=%v", acc, bm)
		}
	}
	var baseRows int64
	require.NoError(t, c.pg.QueryRow(ctx, `
		SELECT count(*) FROM routing_quality_rollup WHERE bucket_minute < $1 AND attempts=15`, nowM.Add(-5*time.Minute)).Scan(&baseRows))
	require.Equal(t, int64(9), baseRows, "baseline 须回填 9 行（3 候选×3 分钟）")
	t.Log("baseline 回填 OK：9 行全胜历史（PG ground truth）")
	// current 构造：gI 持续流量（容忍 500——坏域 terminal 本就向客户端 500），
	// 直到三候选 current 均 ≥30（instance 层直读，live 未 flush 也可见）。
	rtPollLong(t, "gI 三候选 current 充分（instance 层 n>=30）", 600*time.Second, func() (bool, string) {
		for i := 0; i < 20; i++ {
			_, _ = envA.aiReq(http.MethodPost, "/v1/chat/completions", kI, map[string]any{
				"model": rmModel, "stream": true,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
			_, _ = envB.aiReq(http.MethodPost, "/v1/chat/completions", kI, map[string]any{
				"model": rmModel, "stream": true,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
		}
		parts := []string{}
		ok := true
		for _, acc := range []int64{accIA, accIB1, accIB2} {
			a, s := rmQualityInstance(t, c, fpOf[acc])
			parts = append(parts, fmt.Sprintf("%d:n=%d/s=%d", acc, a, s))
			if a < 30 {
				ok = false
			}
		}
		return ok, strings.Join(parts, " ")
	})
	t.Log("gI current 充分：三候选 n>=30（坏域恒 500，好域 200）")
	// incident 评估由编译火触发——持续涓流 + 轮询 plan，直到 mark 出现。
	var inc map[string]any
	rtPollLong(t, "plan 暴露 DOMAIN incident", 600*time.Second, func() (bool, string) {
		for i := 0; i < 10; i++ {
			_, _ = rmChat(t, envA, kI, rmModel, nil)
		}
		_, plan := rtPlan(t, envA)
		r := rtFindRoute(plan, gI, "openai-chat", rmModel)
		if r == nil {
			return false, "gI 路由暂不可见"
		}
		inc = rmIncident(r)
		if inc == nil {
			return false, "路由缺 incident 面"
		}
		active, _ := inc["active"].(bool)
		if active {
			return true, fmt.Sprintf("incident=%v", inc)
		}
		// 未激活时的窗口水位（诊断：current 是否进窗、baseline 是否命中）。
		var curN, baseN int64
		_ = c.pg.QueryRow(ctx, `
			SELECT COALESCE(sum(attempts),0) FROM routing_quality_rollup
			WHERE route_class_id=decode($1,'hex') AND bucket_minute >= date_trunc('minute', now()) - interval '5 minutes'`,
			routeHex).Scan(&curN)
		_ = c.pg.QueryRow(ctx, `
			SELECT COALESCE(sum(attempts),0) FROM routing_quality_rollup
			WHERE route_class_id=decode($1,'hex') AND bucket_minute < date_trunc('minute', now()) - interval '5 minutes'`,
			routeHex).Scan(&baseN)
		return false, fmt.Sprintf("incident=%v settled_cur5m=%d settled_base=%d", inc, curN, baseN)
	})
	require.Equal(t, "domain", inc["kind"], "坏域单域降级须 kind=domain：%v", inc)
	require.Equal(t, float64(3), inc["comparable"], "三候选须全 comparable：%v", inc)
	require.Equal(t, float64(2), inc["degraded"], "坏域双候选须 degraded：%v", inc)
	require.Equal(t, float64(2), inc["domains"], "须见双域：%v", inc)
	require.Greater(t, inc["evaluated_minute"], float64(0), "须有评估分钟：%v", inc)
	// mark-and-expose only：候选零摘除 + 好域领袖仍 primary（incident 不重排车道）。
	_, planI1 := rtPlan(t, envA)
	rI1 := rtFindRoute(planI1, gI, "openai-chat", rmModel)
	require.NotNil(t, rI1)
	idsI1, _ := rmPlanCandidates(rI1)
	require.True(t, idsI1[accIA] && idsI1[accIB1] && idsI1[accIB2], "incident 不得摘除候选：%v", idsI1)
	primI := rmPlanPrimary(rI1)
	require.Contains(t, primI, accIA, "好域 IA 须仍 primary（质量领袖不受 mark 影响）：%v", primI)
	t.Logf("incident OK：%v，好域仍 primary=%v", inc, primI)

	// ============ 收尾：残留检查 ============
	t.Log("收尾：双边 plan 世代 + usage 落库交叉")
	genFA, _ := rtPlan(t, envA)
	genFB, _ := rtPlan(t, envB)
	t.Logf("终态 generation A=%d B=%d", genFA, genFB)
	var costRows int64
	require.NoError(t, c.pg.QueryRow(ctx, `SELECT count(*) FROM usage_logs WHERE cost>0`).Scan(&costRows))
	require.Greater(t, costRows, int64(0), "须有成本 usage 行")
	t.Logf("成本 usage 行=%d", costRows)
}
