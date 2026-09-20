// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e 单实例智能路由端到端测试（charter Task 24）：真实网关 +
// fakeupstream + 真实 PostgreSQL + 真实 Redis。
//
// 运行（前置：PG localhost:15432，Redis localhost:16379；测试以 postgres
// 维护库自建 c3api_routing_e2e 库，跑完不删库如下轮 DROP 重建）：
//
//	$env:TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres"
//	$env:C3API_REDIS_ADDR="127.0.0.1:16379"
//	go test -tags e2e -run 'TestRoutingSmoke|TestIntelligentRoutingE2E' ./tools/e2e -v -timeout 600s
//
// 与 billing_e2e_test.go 同包：复用 e2eEnv/adminToken/jwtSecret/pollUntil/
// waitSnapshot/createUser/userKey/putPrice/jsonGet/stopGracefully/waitExit。
// 本文件所有新增符号一律 rt 前缀，禁与 billing 文件重名。
package e2e

import (
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
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/redisx"
)

const (
	rtServerAddr = "127.0.0.1:18093" // billing 用 18090-18092，本文件独立端口
	rtUpAddr     = "127.0.0.1:19111"
	rtDBName     = "c3api_routing_e2e" // 独立库：与 TestBillingE2E 的 c3api_e2e 隔离
	rtModel      = "rt-model"

	// 注入键：fakeupstream 按上游 key 注入确定性失败（健康键不受影响）。
	rtKeyA    = "rt-key-a"    // 健康账号 A（主组，dx 域，成本 ×1.0）
	rtKeyB    = "rt-key-b"    // 健康账号 B（主组，dx 域，成本 ×2.5）
	rtKeyE    = "rt-key-e"    // 健康账号 E（主组，dy 域，成本 ×1.0）
	rtKeyG    = "rt-key-g"    // 健康账号 G（failover 组）
	rtKey429  = "rt-key-429"  // 恒 429（throttle 组账号 C）
	rtKey500  = "rt-key-500"  // 恒 500（fail_account 组账号 D）
	rtKeyF429 = "rt-key-f429" // 恒 429（failover 组坏账号 F）
	rtKeyF500 = "rt-key-f500" // 恒 500（failover 组坏账号 H）
)

// rtPortFree 端口占用预检：被占直接 FailNow（僵尸进程曾污染单测，禁静默复用）。
func rtPortFree(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		_ = c.Close()
		t.Fatalf("端口 %s 已被占用（疑似僵尸 server/fakeupstream），先清理再跑", addr)
	}
}

// rtBoot 启动 fakeupstream + 网关，返回 env（pg 指向全新的 rtDBName）。
// 调用方不得再起第二个 server 实例（多实例是另一 task，scope 外）。
func rtBoot(t *testing.T) (*e2eEnv, *exec.Cmd, *exec.Cmd, string) {
	t.Helper()
	rtPortFree(t, rtServerAddr)
	rtPortFree(t, rtUpAddr)

	ctx := context.Background()
	redisAddr := os.Getenv("C3API_REDIS_ADDR")
	require.NotEmpty(t, redisAddr, "C3API_REDIS_ADDR is required (e.g. 127.0.0.1:16379)")

	// Redis 隔离（与 PG DROP+CREATE 对等）：FlushDB 清掉上轮残留的易失协
	// 调态。实证根因（h2）：stage-5 限流写入的 OPEN(C,*,rev1)/120s 在共享
	// Redis 存活；新鲜库账号 ID/revision 确定性重放（C恒ID5/rev1），下轮
	// health Sync 原样导入旧记录 → stage-5 全程误拒 C（上游零命中）。生产
	// 多实例本就共享这些记录（收敛语义），清的是测试隔离债。
	rc, err := redisx.Open(redisx.Options{Addr: redisAddr})
	require.NoError(t, err, "redis open for harness isolation flush")
	require.NoError(t, rc.FlushDB(ctx).Err(), "harness redis isolation flush")
	_ = rc.Close()

	// --- DB：postgres 维护库 DROP/CREATE 独立 e2e 库 ---
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://postgres:c3api@localhost:15432/postgres"
	}
	adminPool, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+rtDBName+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `CREATE DATABASE `+rtDBName)
	require.NoError(t, err)
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i+1] + rtDBName
		if !strings.Contains(dsn, "?") {
			dsn += "?sslmode=disable"
		}
	}
	pg, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)

	env := &e2eEnv{t: t, pg: pg, addr: rtServerAddr}
	env.tmp = t.TempDir()

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	build := func(pkg, name string) string {
		out := filepath.Join(env.tmp, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = repoRoot
		cmd.Stderr = os.Stderr
		require.NoError(t, cmd.Run(), "build %s", pkg)
		return out
	}
	upBin := build("./tools/fakeupstream", "rt-fakeupstream.exe")
	srvBin := build("./cmd/server", "rt-server.exe")

	up := exec.Command(upBin, "-addr", rtUpAddr, "-chunks", "5", "-latency", "2ms",
		"-fail429", rtKey429+","+rtKeyF429, "-fail500", rtKey500+","+rtKeyF500)
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	require.NoError(t, up.Start())
	t.Cleanup(func() {
		if up.Process != nil {
			_ = up.Process.Kill()
		}
	})

	// sync_interval=3s：plan 编译含事件驱动 + 周期 tick，短周期加速收敛不断言时序；
	// 其余键与 billing harness 同构（未知键 fail-fast，禁私自加键）。
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
`, rtServerAddr, adminToken, jwtSecret, dsn, redisAddr)
	cfgPath := filepath.Join(env.tmp, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	srv := exec.Command(srvBin, "-config", cfgPath)
	srvLog, err := os.Create(filepath.Join(env.tmp, "server.log"))
	require.NoError(t, err)
	srv.Stdout, srv.Stderr = srvLog, srvLog
	if isWindows() {
		srv.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	}
	require.NoError(t, srv.Start())
	t.Cleanup(func() {
		if srv.ProcessState != nil && srv.ProcessState.Exited() {
			_ = srvLog.Close()
			return
		}
		if srv.Process != nil {
			_ = srv.Process.Kill()
			_ = srv.Wait()
		}
		_ = srvLog.Close()
	})
	// 失败诊断：server.log 转储（Cleanup LIFO，先于 Kill 执行）。
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if data, err := os.ReadFile(filepath.Join(env.tmp, "server.log")); err == nil {
			t.Logf("--- rt server.log (test failed) ---\n%s", data)
		}
	})

	rtWaitReady(t, env, env.tmp)
	return env, srv, up, dsn
}

// rtWaitReady 轮询 /api/admin/settings 直到 200（migrate + 分区 bootstrap 完成）。
func rtWaitReady(t *testing.T, env *e2eEnv, tmp string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
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
				t.Logf("--- server.log ---\n%s", data)
			}
			t.Fatal("routing gateway 未在 60s 内就绪")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// rtPlan GET /routing/plan 原始体 + 解析后 generation。
func rtPlan(t *testing.T, env *e2eEnv) (int64, map[string]any) {
	t.Helper()
	c, rb := env.admin(http.MethodGet, "/routing/plan", nil)
	require.Equal(t, 200, c, "get routing plan: %s", rb)
	v := jsonGet(t, rb, "").(map[string]any)
	gen, ok := v["generation"].(float64)
	require.True(t, ok, "plan 缺 generation: %s", rb)
	return int64(gen), v
}

// rtWaitPlanGen 有界轮询直到 plan generation >= want（后台编译异步，禁裸 sleep）。
func rtWaitPlanGen(t *testing.T, env *e2eEnv, want int64, what string) map[string]any {
	t.Helper()
	var last map[string]any
	pollUntil(t, what, func() (bool, string) {
		gen, v := rtPlan(t, env)
		last = v
		return gen >= want, fmt.Sprintf("plan generation=%d want>=%d", gen, want)
	})
	return last
}

// rtFindRoute 在 plan routes 中按 (group,format,model) 定位路由；缺失返回 nil。
func rtFindRoute(plan map[string]any, groupID int64, format, model string) map[string]any {
	routes, _ := plan["routes"].([]any)
	for _, r := range routes {
		m, _ := r.(map[string]any)
		ref, _ := m["ref"].(map[string]any)
		if ref == nil {
			continue
		}
		gid, _ := ref["group_id"].(float64)
		if int64(gid) == groupID && ref["format"] == format && ref["model"] == model {
			return m
		}
	}
	return nil
}

// rtWaitRoute 有界轮询直到指定路由出现在 plan 中且候选非空（seed→编译事件
// 异步；路由目录可能先于候选装配出现，空候选不算发布）。超时前转储 plan /
// accounts / ops workers 诊断面再 FailNow。
func rtWaitRoute(t *testing.T, env *e2eEnv, groupID int64, format, model string) map[string]any {
	t.Helper()
	var found map[string]any
	var lastPlan map[string]any
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, plan := rtPlan(t, env)
		lastPlan = plan
		found = rtFindRoute(plan, groupID, format, model)
		if found != nil {
			if cands, _ := found["candidates"].([]any); len(cands) > 0 {
				return found
			}
		}
		if !time.Now().Before(deadline) {
			if raw, err := json.Marshal(lastPlan); err == nil {
				t.Logf("DIAG plan=%.4000s", raw)
			}
			if c, rb := env.admin(http.MethodGet, "/accounts?limit=100", nil); true {
				t.Logf("DIAG accounts status=%d body=%.3000s", c, rb)
			}
			if c, rb := env.admin(http.MethodGet, "/ops/workers", nil); true {
				t.Logf("DIAG ops/workers status=%d body=%.3000s", c, rb)
			}
			t.Fatalf("路由 (g=%d %s %s) 90s 内候选仍为空", groupID, format, model)
		}
		<-tick.C
	}
}

// rtChat 流式 chat（stream=true：usage pt=10/ct=20 确定性计费；且只有流式才
// 采集 TTFT——非流式永无 ttft_n，质量分级 TTFTCount>=30 恒不满足，Primary 永不
// 发布。见 evidence 缺陷注）。
func rtChat(t *testing.T, env *e2eEnv, key, model string, extra map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for k, v := range extra {
		body[k] = v
	}
	return env.aiReq(http.MethodPost, "/v1/chat/completions", key, body)
}

// rtAuditEntry fakeupstream /_audit 条目子集。
type rtAuditEntry struct {
	Seq    int64  `json:"seq"`
	Key    string `json:"key"`
	Model  string `json:"model"`
	Status int    `json:"status"`
	Path   string `json:"path"`
}

// rtAudit 拉取 fakeupstream 审计账目（哪个上游 key 真实处理了请求）。
func rtAudit(t *testing.T) []rtAuditEntry {
	t.Helper()
	resp, err := http.Get("http://" + rtUpAddr + "/_audit")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out []rtAuditEntry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// rtAuditCountByKey 按上游 key 统计终态 200 的处理次数。
func rtAuditCountByKey(t *testing.T, keys ...string) map[string]int {
	t.Helper()
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	got := map[string]int{}
	for _, e := range rtAudit(t) {
		if want[e.Key] && e.Status == http.StatusOK {
			got[e.Key]++
		}
	}
	return got
}

// rtAccountRead GET /accounts/{id} 全量（含 LifecycleRevision/FailedAt 等 PascalCase 面）。
func rtAccountRead(t *testing.T, env *e2eEnv, id int64) map[string]any {
	t.Helper()
	c, rb := env.admin(http.MethodGet, fmt.Sprintf("/accounts/%d", id), nil)
	require.Equal(t, 200, c, "read account %d: %s", id, rb)
	return jsonGet(t, rb, "").(map[string]any)
}

// rtRevision 取账号当前 LifecycleRevision（fenced 写前置）。
func rtRevision(t *testing.T, env *e2eEnv, id int64) int64 {
	t.Helper()
	m := rtAccountRead(t, env, id)
	rev, ok := m["LifecycleRevision"].(float64)
	require.True(t, ok, "account 缺 LifecycleRevision: %v", m)
	return int64(rev)
}

// TestRoutingSmoke 路由冒烟：boot → seed → 首个 plan 发布 → 一次成功代理 →
// Explore 断言。完整场景见 TestIntelligentRoutingE2E（同 harness 之上构建）。
func TestRoutingSmoke(t *testing.T) {
	env, _, _, _ := rtBoot(t)

	// --- 冷启动门：seed 前 AI 流量必须被拒绝（plan 未发布：代码 404，文档称
	// 503——此处不断言具体码，只断言拒绝 + 记录实测码，见 evidence 缺陷/差异注）。
	// 空库首个编译为空视图会开门，但开门也无账号可用，仍拒绝。
	c, rb := rtChat(t, env, "bogus-key", rtModel, nil)
	t.Logf("SMOKE pre-seed AI status=%d body=%.200s", c, rb)
	require.NotEqual(t, 200, c, "seed 前 AI 请求必须被拒绝（门控或无账号）：%s", rb)

	// --- seed：定价 → 模板 → 双账号 → 组 → 用户/key ---
	putPrice(t, env, rtModel, map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})
	tpl := env.create("/templates", map[string]any{
		"name": "rt-tpl", "base_url": "http://" + rtUpAddr,
		"supported_formats": []string{"openai-chat", "openai-responses", "anthropic"},
		"models":            []string{rtModel},
	})
	g := env.create("/groups", map[string]any{"name": "rt-grp"})
	accA := env.create("/accounts", map[string]any{
		"name": "rt-acc-a", "template_id": tpl, "upstream_key": rtKeyA,
		"group_ids": []int64{g},
	})
	accB := env.create("/accounts", map[string]any{
		"name": "rt-acc-b", "template_id": tpl, "upstream_key": rtKeyB,
		"group_ids": []int64{g},
	})
	u := createUser(t, env, "rt-smoke@example.com", 10.0)
	_, ukey := userKey(t, env, u, g)
	waitSnapshot()

	// --- 首个 plan 发布（事件驱动编译异步，有界轮询 generation>=1 且路由可见） ---
	route := rtWaitRoute(t, env, g, "openai-chat", rtModel)
	gen, _ := rtPlan(t, env)
	t.Logf("SMOKE plan generation=%d route=%v", gen, route["ref"])

	// --- 一次成功代理 + Explore 断言（全未知冷启动 → 100% Explore） ---
	c, rb = rtChat(t, env, ukey, rtModel, nil)
	require.Equal(t, 200, c, "首个代理请求必须 200: %s", rb)

	_, plan := rtPlan(t, env)
	route = rtFindRoute(plan, g, "openai-chat", rtModel)
	require.NotNil(t, route, "路由必须仍在 plan 中")
	primary, _ := route["primary"].([]any)
	require.Empty(t, primary, "冷启动 primary 必须为空（零质量样本）")
	explore, _ := route["explore"].(map[string]any)
	require.NotNil(t, explore, "冷启动必须有 explore 车道")
	ids, _ := explore["ids"].([]any)
	got := map[int64]bool{}
	for _, id := range ids {
		if f, ok := id.(float64); ok {
			got[int64(f)] = true
		}
	}
	require.True(t, got[accA] && got[accB],
		"冷启动 explore 必须含双账号（100%% Explore）：got=%v want=[%d %d]", got, accA, accB)

	// 上游归因：audit 证明请求真实到达 fakeupstream（任一健康 key）。
	hits := rtAuditCountByKey(t, rtKeyA, rtKeyB)
	require.Equal(t, 1, hits[rtKeyA]+hits[rtKeyB], "audit 必须恰好记录 1 次成功处理：%v", hits)
	t.Logf("SMOKE upstream hits=%v", hits)
}

// ---- TestIntelligentRoutingE2E 长程 helpers（rt 前缀，禁与 billing 文件重名） ----

// rtPollLong 有界长轮询（质量收敛/rollup/重编译分钟级链路，10s 默认不够）。
func rtPollLong(t *testing.T, what string, timeout time.Duration, observe func() (bool, string)) {
	t.Helper()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(timeout)
	for {
		done, last := observe()
		if done {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("轮询超时(%s) %s：%s", timeout, what, last)
		}
		<-tick.C
	}
}

// rtCreateRule 建规则（201），返回 ID（PascalCase 优先，兼容小写）。
func rtCreateRule(t *testing.T, env *e2eEnv, name string, prio int, when, then map[string]any) int64 {
	t.Helper()
	c, rb := env.admin(http.MethodPost, "/rules", map[string]any{
		"name": name, "priority": prio, "enabled": true, "when": when, "then": then,
	})
	require.Equal(t, 201, c, "create rule %s: %s", name, rb)
	if v := jsonGet(t, rb, "ID"); v != nil {
		return int64(v.(float64))
	}
	return int64(jsonGet(t, rb, "id").(float64))
}

// rtSetCost 改采购倍率（multiplier 0..10，1=×1）——走账号配置唯一写面。
func rtSetCost(t *testing.T, env *e2eEnv, acc int64, mult float64) {
	t.Helper()
	c, rb := env.admin(http.MethodPatch, fmt.Sprintf("/accounts/%d", acc), map[string]any{
		"upstream_cost_multiplier": mult,
	})
	require.Equal(t, 200, c, "set cost %d×%v: %s", acc, mult, rb)
}

// rtSetEnabled 启停账号——走账号配置唯一写面。
func rtSetEnabled(t *testing.T, env *e2eEnv, acc int64, en bool) {
	t.Helper()
	c, rb := env.admin(http.MethodPatch, fmt.Sprintf("/accounts/%d", acc), map[string]any{
		"enabled": en,
	})
	require.Equal(t, 200, c, "set enabled %d=%v: %s", acc, en, rb)
}

// rtRecover 失败账号恢复（rev+1 → PROBING）。
func rtRecover(t *testing.T, env *e2eEnv, acc int64) {
	t.Helper()
	c, rb := env.admin(http.MethodPost, fmt.Sprintf("/accounts/%d/recover", acc), map[string]any{
		"expected_revision": rtRevision(t, env, acc),
	})
	require.Equal(t, 200, c, "recover %d: %s", acc, rb)
}

// rtSetRuleEnabled 按名启停规则（种子规则 seed-429/seed-5xx 会以更高优先级
// 先中、塑造响应并遮蔽自建规则的 fail 动作——隔离组场景须先禁它们以保归因纯粹）。
func rtSetRuleEnabled(t *testing.T, env *e2eEnv, name string, enabled bool) {
	t.Helper()
	c, rb := env.admin(http.MethodGet, "/rules?limit=200", nil)
	require.Equal(t, 200, c, "list rules: %s", rb)
	var v any
	require.NoError(t, json.Unmarshal([]byte(rb), &v), "parse rules: %s", rb)
	m, _ := v.(map[string]any)
	var rows []any
	for _, k := range []string{"rows", "Rows"} {
		if r, ok := m[k].([]any); ok {
			rows = r
			break
		}
	}
	require.NotEmpty(t, rows, "rules 列表为空：%s", rb)
	var id float64
	found := false
	for _, r := range rows {
		rm, _ := r.(map[string]any)
		if rm == nil {
			continue
		}
		nm, _ := rm["Name"].(string)
		if nm == "" {
			nm, _ = rm["name"].(string)
		}
		if nm != name {
			continue
		}
		for _, k := range []string{"ID", "id"} {
			if f, ok := rm[k].(float64); ok {
				id, found = f, true
				break
			}
		}
	}
	require.True(t, found, "规则 %s 未找到：%s", name, rb)
	c, rb = env.admin(http.MethodPut, fmt.Sprintf("/rules/%d", int64(id)), map[string]any{"enabled": enabled})
	require.Equal(t, 200, c, "set rule %s enabled=%v: %s", name, enabled, rb)
}

// rtRouteHex 取路由 route_class_id（plan ref 面）。
func rtRouteHex(t *testing.T, env *e2eEnv, g int64, format, model string) string {
	t.Helper()
	_, plan := rtPlan(t, env)
	route := rtFindRoute(plan, g, format, model)
	require.NotNil(t, route, "路由 (g=%d %s %s) 不在 plan 中", g, format, model)
	ref, _ := route["ref"].(map[string]any)
	hex, _ := ref["route_class_id"].(string)
	require.NotEmpty(t, hex, "路由缺 route_class_id")
	return hex
}

// rtWaitGenBump 等待 plan generation 推进（启停/恢复触发重编译，异步）。
func rtWaitGenBump(t *testing.T, env *e2eEnv, before int64, what string) int64 {
	t.Helper()
	var gen int64
	rtPollLong(t, what, 60*time.Second, func() (bool, string) {
		gen, _ = rtPlan(t, env)
		return gen > before, fmt.Sprintf("generation=%d want>%d", gen, before)
	})
	return gen
}

// rtAuditHits 按 (key,status) 统计；seqAfter>0 时只计其后（增量归因）。
func rtAuditHits(t *testing.T, seqAfter int64, key string, status int) int {
	t.Helper()
	n := 0
	for _, e := range rtAudit(t) {
		if e.Seq > seqAfter && e.Key == key && e.Status == status {
			n++
		}
	}
	return n
}

// rtAuditMaxSeq 当前审计水位（增量窗口起点）。
func rtAuditMaxSeq(t *testing.T) int64 {
	t.Helper()
	var m int64
	for _, e := range rtAudit(t) {
		if e.Seq > m {
			m = e.Seq
		}
	}
	return m
}

// rtRollupBest DB 直读实例层按候选聚合 attempts（quality-sync 5s 节奏落库，
// 秒级可见；frontier 读 rollup 另有 5s 节奏滞后）。收敛门以实例层快速信号
// 为准，frontier 真实性另行断言（缺陷 C 修复后两者一致，不再是二选一）。
func rtRollupBest(t *testing.T, env *e2eEnv) (int, map[string]int64) {
	t.Helper()
	rows, err := env.pg.Query(context.Background(), `
		SELECT encode(candidate_fingerprint,'hex'), COALESCE(sum(attempts),0)
		FROM routing_quality_instance_minute GROUP BY 1`)
	if err != nil {
		return 0, nil
	}
	defer rows.Close()
	best := 0
	per := map[string]int64{}
	for rows.Next() {
		var fp string
		var n int64
		if err := rows.Scan(&fp, &n); err != nil {
			break
		}
		short := fp
		if len(short) > 12 {
			short = short[:12]
		}
		per[short] = n
		if int(n) > best {
			best = int(n)
		}
	}
	return best, per
}

// rtFlow GET /routing/flow 原始 map。
func rtFlow(t *testing.T, env *e2eEnv, routeHex, from, to string) map[string]any {
	t.Helper()
	c, rb := env.admin(http.MethodGet,
		fmt.Sprintf("/routing/flow?route=%s&from=%s&to=%s", routeHex, from, to), nil)
	require.Equal(t, 200, c, "get routing flow: %s", rb)
	return jsonGet(t, rb, "").(map[string]any)
}

// rtFrontier GET /routing/frontier 原始 map。
func rtFrontier(t *testing.T, env *e2eEnv, routeHex, from, to string) map[string]any {
	t.Helper()
	c, rb := env.admin(http.MethodGet,
		fmt.Sprintf("/routing/frontier?route=%s&from=%s&to=%s", routeHex, from, to), nil)
	require.Equal(t, 200, c, "get routing frontier: %s", rb)
	return jsonGet(t, rb, "").(map[string]any)
}

// TestIntelligentRoutingE2E 单实例智能路由全场景（与 TestRoutingSmoke 同 harness）：
// 冷启动 Explore → 亲和命中/溢出 → 质量收敛切 Primary → 429 窗口未中/命中限流 →
// failover 聚合验证 → fail_account/恢复 → 硬连续 → Sankey 守恒 → 多协议+计费。
func TestIntelligentRoutingE2E(t *testing.T) {
	env, _, _, _ := rtBoot(t)
	t0 := time.Now().UTC().Truncate(time.Second)
	fromParam := t0.Format(time.RFC3339)

	// ============ seed：价格/模板/多账号成本/多组/规则/缓存域 ============
	putPrice(t, env, rtModel, map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})
	c, rb := env.admin(http.MethodPut, "/prices/entry?model=rt-img-model",
		map[string]any{"mode": "image", "price_per_image": 0.05})
	require.Equal(t, 200, c, "put image price: %s", rb)

	tpl := env.create("/templates", map[string]any{
		"name": "rt-tpl", "base_url": "http://" + rtUpAddr,
		"supported_formats": []string{"openai-chat", "openai-responses", "anthropic", "openai-images"},
		"models":            []string{rtModel, "rt-img-model"},
	})
	g1 := env.create("/groups", map[string]any{"name": "rt-g1"})
	g2 := env.create("/groups", map[string]any{"name": "rt-g2-throttle"})
	g3 := env.create("/groups", map[string]any{"name": "rt-g3-fail"})
	g4 := env.create("/groups", map[string]any{"name": "rt-g4-failover"})
	// H（恒 500）独占 g5：5xx 按 replay 规则是 plan-terminal（不 failover），
	// 与 g4 的 429-failover 聚合断言互斥，隔离之（D 已覆盖 5xx 终端语义）。
	g5 := env.create("/groups", map[string]any{"name": "rt-g5-iso"})
	// 就绪门（canary）：首批创建的 NOTIFY 可能落在 scheduler 订阅之前（NOTIFY
	// 无持久化，丢失即长时间 stale）——先验证 canary 组/账号编译可见，证明
	// “创建→快照→编译”链路已接通，再种正式拓扑。
	t.Log("就绪门：canary 账号编译可见性")
	g0 := env.create("/groups", map[string]any{"name": "rt-g0-ready"})
	env.create("/accounts", map[string]any{
		"name": "rt-canary", "template_id": tpl, "upstream_key": rtKeyG,
		"group_ids": []int64{g0},
	})
	rtWaitRoute(t, env, g0, "openai-chat", rtModel)
	t.Log("就绪门通过：canary 候选已发布")
	domX, domY := "dx.example", "dy.example"
	// 缺陷 A 已修复：创建即带 cache_domain 与事后 PUT 等价（此前创建即带域
	// 会静默置 disabled、永不进入候选）。此处直接创建即带域——阶段 1 的
	// explore 断言即其编译可见性证明。
	accA := env.create("/accounts", map[string]any{
		"name": "rt-a", "template_id": tpl, "upstream_key": rtKeyA,
		"group_ids": []int64{g1}, "cache_domain": domX,
	})
	accB := env.create("/accounts", map[string]any{
		"name": "rt-b", "template_id": tpl, "upstream_key": rtKeyB,
		"group_ids": []int64{g1}, "cache_domain": domX,
	})
	accE := env.create("/accounts", map[string]any{
		"name": "rt-e", "template_id": tpl, "upstream_key": rtKeyE,
		"group_ids": []int64{g1}, "cache_domain": domY,
	})
	accC := env.create("/accounts", map[string]any{
		"name": "rt-c429", "template_id": tpl, "upstream_key": rtKey429,
		"group_ids": []int64{g2},
	})
	accD := env.create("/accounts", map[string]any{
		"name": "rt-d500", "template_id": tpl, "upstream_key": rtKey500,
		"group_ids": []int64{g3},
	})
	accF := env.create("/accounts", map[string]any{
		"name": "rt-f429", "template_id": tpl, "upstream_key": rtKeyF429,
		"group_ids": []int64{g4},
	})
	accH := env.create("/accounts", map[string]any{
		"name": "rt-h500", "template_id": tpl, "upstream_key": rtKeyF500,
		"group_ids": []int64{g5},
	})
	accG := env.create("/accounts", map[string]any{
		"name": "rt-g", "template_id": tpl, "upstream_key": rtKeyG,
		"group_ids": []int64{g4},
	})
	_ = accH // H 隔离在 g5（5xx-terminal 对照），g4 断言仅 F/G
	rtSetCost(t, env, accB, 2.5) // B 采购贵 2.5×：成本面可观测
	// 种子自检（fail-fast：组成员 + 缓存域回显；编译缺候选时先排除种子问题）。
	rtAssertGroups := func(acc int64, want ...int64) {
		t.Helper()
		c, rb := env.admin(http.MethodGet, fmt.Sprintf("/accounts/%d/groups", acc), nil)
		require.Equal(t, 200, c, "account %d groups: %s", acc, rb)
		for _, g := range want {
			require.Contains(t, rb, fmt.Sprintf("%d", g), "account %d 须在组 %d：%s", acc, g, rb)
		}
		m := rtAccountRead(t, env, acc)
		t.Logf("SEED acc=%d groups=%v CacheDomain=%v rev=%v cost=%v", acc, want, m["CacheDomain"], m["LifecycleRevision"], m["UpstreamCostMultiplier"])
	}
	rtAssertGroups(accA, g1)
	rtAssertGroups(accB, g1)
	rtAssertGroups(accE, g1)
	rtAssertGroups(accC, g2)
	rtAssertGroups(accD, g3)
	rtAssertGroups(accF, g4)
	rtAssertGroups(accG, g4)
	u1 := createUser(t, env, "rt-u1@example.com", 100.0) // 收敛循环烧钱，100 USD 余量
	_, k1 := userKey(t, env, u1, g1)
	u2 := createUser(t, env, "rt-u2@example.com", 20.0)
	_, k2 := userKey(t, env, u2, g2)
	u3 := createUser(t, env, "rt-u3@example.com", 20.0)
	_, k3 := userKey(t, env, u3, g3)
	u4 := createUser(t, env, "rt-u4@example.com", 20.0)
	_, k4 := userKey(t, env, u4, g4)
	balBefore := env.balance(u1)

	// 规则先行（事件计数依赖规则已存在）：C 账号 429 窗口限流；D 账号 5xx 即死。
	rtCreateRule(t, env, "rt-throttle-429", 810,
		map[string]any{"kind": "429", "account_id": accC, "count_429_ge": 3, "window_seconds": 120},
		map[string]any{"throttle": map[string]any{
			"scope": "account", "mode": "open", "duration_ms": 120000, "use_reset": false,
		}})
	rtCreateRule(t, env, "rt-kill-5xx", 805,
		map[string]any{"kind": "5xx", "account_id": accD, "count_failure_ge": 1, "window_seconds": 120},
		map[string]any{"fail_account": true})
	// 种子遮蔽：seed-429/seed-5xx 优先级更高且无窗口（首错即中），禁之保归因。
	rtSetRuleEnabled(t, env, "seed-429", false)
	rtSetRuleEnabled(t, env, "seed-5xx", false)
	waitSnapshot()
	// 隔离组路由就绪（g2/g3/g4 候选齐备再进场景；g1 在阶段 1 等）。
	t.Log("隔离组路由就绪等待")
	r2 := rtWaitRoute(t, env, g2, "openai-chat", rtModel)
	hasC := false
	for _, cc := range r2["candidates"].([]any) {
		if mm, ok := cc.(map[string]any); ok {
			if aid, ok := mm["account_id"].(float64); ok && int64(aid) == accC {
				hasC = true
			}
		}
	}
	require.True(t, hasC, "g2 候选须含 C")
	rtWaitRoute(t, env, g3, "openai-chat", rtModel)
	rtWaitRoute(t, env, g4, "openai-chat", rtModel)

	// ============ 1. 冷启动 100% Explore ============
	t.Log("阶段 1：冷启动全未知 → 100% Explore")
	route := rtWaitRoute(t, env, g1, "openai-chat", rtModel)
	rawRoute, _ := json.Marshal(route)
	t.Logf("阶段 1 route: %s", rawRoute)
	primary, _ := route["primary"].([]any)
	require.Empty(t, primary, "冷启动 primary 必须为空")
	exploreIDs := map[int64]bool{}
	if ex, _ := route["explore"].(map[string]any); ex != nil {
		for _, id := range ex["ids"].([]any) {
			if f, ok := id.(float64); ok {
				exploreIDs[int64(f)] = true
			}
		}
	}
	require.True(t, exploreIDs[accA] && exploreIDs[accB] && exploreIDs[accE],
		"冷启动 explore 须含 A/B/E：%v", exploreIDs)

	// ============ 2. 亲和命中：同 prompt_cache_key 粘同一账号 ============
	t.Log("阶段 2：亲和命中（同 key 粘滞）/  distinct key 铺开")
	chatKey := func(key, pck string) (int, string) {
		return rtChat(t, env, k1, rtModel, map[string]any{"prompt_cache_key": pck})
	}
	mark := rtAuditMaxSeq(t)
	for i := 0; i < 6; i++ {
		c, rb := chatKey(k1, "aff-hit-1")
		require.Equal(t, 200, c, "aff hit req %d: %s", i, rb)
	}
	perAcc := map[string]int{
		rtKeyA: rtAuditHits(t, mark, rtKeyA, 200),
		rtKeyB: rtAuditHits(t, mark, rtKeyB, 200),
		rtKeyE: rtAuditHits(t, mark, rtKeyE, 200),
	}
	stuck := 0
	for acc, n := range perAcc {
		if n == 6 {
			stuck++
			t.Logf("亲和命中：6 请求全粘 %s", acc)
		}
	}
	require.Equal(t, 1, stuck, "同 prompt_cache_key 必须全粘同一账号：%v", perAcc)

	mark = rtAuditMaxSeq(t)
	// FNV 前缀聚集：同前缀 key 落同域（离线测得 alpha/bravo/foxtrot/india→dx，
	// charlie→dy；见 evidence fnvprobe）。dx key 必须全落 {A,B} 且零 E；
	// dy key 落 E——逐 key 域路由的确定性证明（零概率成分）。
	for _, pck := range []string{"alpha-00", "bravo-00", "foxtrot-00", "india-00"} {
		c, rb := chatKey(k1, pck)
		require.Equal(t, 200, c, "dx req %s: %s", pck, rb)
	}
	require.Zero(t, rtAuditHits(t, mark, rtKeyE, 200), "dx 域 key 不得由 E 处理")
	dxAB := rtAuditHits(t, mark, rtKeyA, 200) + rtAuditHits(t, mark, rtKeyB, 200)
	require.Equal(t, 4, dxAB, "dx 域 4 key 必须全由 A/B 处理")
	for _, pck := range []string{"charlie-00", "charlie-01"} {
		c, rb := chatKey(k1, pck)
		require.Equal(t, 200, c, "dy req %s: %s", pck, rb)
	}
	require.Equal(t, 2, rtAuditHits(t, mark, rtKeyE, 200), "dy 域 key 必须由 E 处理")
	t.Log("亲和分域：dx→{A,B}×4，dy→E×2")

	// ============ 3. 亲和溢出：dy 域失能 → 同 key 溢到全局 ============
	t.Log("阶段 3：亲和溢出（域无容量 → 全局 fallback）")
	dyKey := ""
	mark = rtAuditMaxSeq(t)
	for i := 0; i < 12 && dyKey == ""; i++ {
		pck := fmt.Sprintf("aff-dy-%d", i)
		c, rb := chatKey(k1, pck)
		require.Equal(t, 200, c, "dy probe %d: %s", i, rb)
		if rtAuditHits(t, mark, rtKeyE, 200) > 0 {
			dyKey = pck
			mark = rtAuditMaxSeq(t)
		}
	}
	require.NotEmpty(t, dyKey, "12 个候选 key 无一命中 dy 域（E 从未被亲和选中）")
	t.Logf("亲和 dy 命中 key=%s", dyKey)
	gen, _ := rtPlan(t, env)
	rtSetEnabled(t, env, accE, false) // dy 域唯一账号失能 = 域无容量
	gen = rtWaitGenBump(t, env, gen, "E 失能后重编译")
	_ = gen
	mark = rtAuditMaxSeq(t)
	c, rb = chatKey(k1, dyKey)
	require.Equal(t, 200, c, "spill req: %s", rb)
	require.Zero(t, rtAuditHits(t, mark, rtKeyE, 200), "失能后 E 必须零处理")
	spilled := rtAuditHits(t, mark, rtKeyA, 200) + rtAuditHits(t, mark, rtKeyB, 200)
	require.Equal(t, 1, spilled, "dy key 必须溢到 dx 域处理")
	t.Logf("亲和溢出：%s → dx 域", dyKey)
	rtSetEnabled(t, env, accE, true)
	rtWaitRoute(t, env, g1, "openai-chat", rtModel)
	// E 恢复后须等其候选归位（三候选齐）再进收敛，否则指纹映射缺员。
	rtPollLong(t, "E 候选归位", 60*time.Second, func() (bool, string) {
		_, plan := rtPlan(t, env)
		r := rtFindRoute(plan, g1, "openai-chat", rtModel)
		if r == nil {
			return false, "路由暂不可见"
		}
		cands, _ := r["candidates"].([]any)
		return len(cands) >= 3, fmt.Sprintf("candidates=%d", len(cands))
	})

	// ============ 4a. 质量收敛 → 首个 Primary（事件驱动，无 nudge） ============
	// 收敛信号只随新尝试推进：每轮先打 10 个流式请求再读 DB 实例层，直到任一
	// 候选 attempts>=30（流式才有 TTFT；非流式 ttft_n 恒 0）。缺陷 B 已修复：
	// 质量落库边界直接驱逐编译道（PG flush→RequestCompile，去抖收敛），首个
	// 充分候选晋升 Primary——本阶段不再做任何管理面写 nudge，只等 primary 非空。
	// 语义注记（探索份额已接线）：首个晋升者承载绝大多数首尝试，但
	// explore-first 份额（bp>0）持续把首尝试注入未知候选——旧"三候选齐充分"
	// 门在修复后的系统中不可达（它恰是 plan 冻结 bug 的产物），此处只要求
	// 首个晋升；4b 先断言份额注活（零隔离），再以单隔离覆盖跟随者晋升。
	// 缺陷 C 已修复：fakeupstream 首帧限速使 TTFT 现实化（~2ms），durable 和
	// 非零、frontier ttft 真实——DB 和 + frontier 双断言（仅充分候选）。
	t.Log("阶段 4a：质量收敛 → 首个 Primary（事件驱动）")
	hexMain := rtRouteHex(t, env, g1, "openai-chat", rtModel)
	fpAcc := map[string]int64{}
	_, plan0 := rtPlan(t, env)
	if r0 := rtFindRoute(plan0, g1, "openai-chat", rtModel); r0 != nil {
		for _, cc := range r0["candidates"].([]any) {
			mm, _ := cc.(map[string]any)
			fp, _ := mm["fingerprint"].(string)
			aid, _ := mm["account_id"].(float64)
			if fp != "" {
				fpAcc[fp] = int64(aid)
			}
		}
	}
	require.Len(t, fpAcc, 3, "主路由须有 3 候选")
	// gen 起点取在收敛循环之前：收敛期间每次晋升发布都会推进 generation，
	// 收敛后读起点会漏掉已发生的推进。
	genBefore, _ := rtPlan(t, env)
	iters := 0
	rtPollLong(t, "质量收敛首候选 n>=30（DB 实例层）", 300*time.Second, func() (bool, string) {
		iters++
		if iters > 60 {
			return false, "60 轮（600 请求）仍无候选充分"
		}
		for i := 0; i < 10; i++ {
			if c, rb := rtChat(t, env, k1, rtModel, nil); c != 200 {
				return false, fmt.Sprintf("converge req 异常 status=%d body=%.120s", c, rb)
			}
		}
		rows, err := env.pg.Query(context.Background(), `
			SELECT fp, MAX(n) FROM (
				SELECT encode(candidate_fingerprint,'hex') AS fp, COALESCE(sum(attempts),0) AS n
				FROM routing_quality_instance_minute GROUP BY 1
				UNION ALL
				SELECT encode(candidate_fingerprint,'hex') AS fp, COALESCE(sum(attempts),0) AS n
				FROM routing_quality_rollup GROUP BY 1
			) s GROUP BY fp`)
		if err != nil {
			return false, err.Error()
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var fp string
			var n int64
			if err := rows.Scan(&fp, &n); err != nil {
				break
			}
			if aid, ok := fpAcc[fp]; ok {
				got[aid] = n
			}
		}
		for _, aid := range []int64{accA, accB, accE} {
			if got[aid] >= 30 {
				return true, fmt.Sprintf("首候选充分 acc=%d per=%v", aid, got)
			}
		}
		return false, fmt.Sprintf("iter=%d per=%v", iters, got)
	})
	// 事件驱动重编译：收敛达成后不做任何管理面写，只等 primary 非空——质量
	// 落库边界（PG flush）已直接驱逐编译道。超时即缺陷 B 回归。
	t.Logf("收敛达成（%d 轮），等待事件驱动重编译 primary 出现（gen 起点=%d）", iters, genBefore)
	var primaries []any
	rtPollLong(t, "事件驱动重编译后 Primary 非空（无 nudge）", 120*time.Second, func() (bool, string) {
		gen, plan := rtPlan(t, env)
		r := rtFindRoute(plan, g1, "openai-chat", rtModel)
		if r == nil {
			return false, "主路由暂不可见"
		}
		primaries, _ = r["primary"].([]any)
		return len(primaries) > 0, fmt.Sprintf("gen=%d primary=%v", gen, primaries)
	})
	genAfter, _ := rtPlan(t, env)
	t.Logf("事件驱动重编译后 gen=%d primary=%v", genAfter, primaries)
	require.Greater(t, genAfter, genBefore, "质量 influx 必须推进 plan generation（无管理面写）")
	// durable TTFT 和：实例层充分候选（n>=30）ttft 和/平方和非零（缺陷 C 特征
	// 即计数有、和零；首帧限速后 ~2ms 样本的对数和恒非零）。未充分候选豁免
	// （区间本就要求 n>=30）。
	rtPollLong(t, "实例层 TTFT 和非零（充分候选）", 120*time.Second, func() (bool, string) {
		rows, err := env.pg.Query(context.Background(), `
			SELECT encode(candidate_fingerprint,'hex') AS fp,
				COALESCE(sum(ttft_n),0) AS n,
				COALESCE(sum(ttft_sum_log_q32),0) AS s,
				COALESCE(sum(ttft_sumsq_log_q32),0) AS sq
			FROM routing_quality_instance_minute GROUP BY 1`)
		if err != nil {
			return false, err.Error()
		}
		defer rows.Close()
		got := map[int64][3]int64{}
		for rows.Next() {
			var fp string
			var n, s, sq int64
			if err := rows.Scan(&fp, &n, &s, &sq); err != nil {
				break
			}
			if aid, ok := fpAcc[fp]; ok {
				got[aid] = [3]int64{n, s, sq}
			}
		}
		qualified := 0
		for _, aid := range []int64{accA, accB, accE} {
			v := got[aid]
			if v[0] >= 30 {
				qualified++
				if v[1] == 0 || v[2] == 0 {
					return false, fmt.Sprintf("充分候选 acc=%d 和为零 per=%v", aid, got)
				}
			}
		}
		if qualified == 0 {
			return false, fmt.Sprintf("尚无充分候选 per=%v", got)
		}
		return true, fmt.Sprintf("充分候选 TTFT 和非零 per=%v", got)
	})
	// frontier 断言（API 面真实语义）：attempts>=30 的候选 ttft_known 且
	// ucb>1ms——缺陷 C 的 {1,1} 谎报在此被证伪（rollup 5s 节奏，有界轮询）。
	rtPollLong(t, "frontier TTFT 真实（充分候选 ttft_known 且 ucb>1ms）", 180*time.Second, func() (bool, string) {
		toP := time.Now().UTC().Format(time.RFC3339)
		frontier := rtFrontier(t, env, hexMain, fromParam, toP)
		cands, _ := frontier["candidates"].([]any)
		qualified := 0
		for _, c := range cands {
			m, _ := c.(map[string]any)
			aid, _ := m["account_id"].(float64)
			att, _ := m["attempts"].(float64)
			known, _ := m["ttft_known"].(bool)
			ucb, _ := m["ttft_ucb"].(float64)
			t.Logf("frontier acc=%v attempts=%v successes=%v insufficient=%v ttft_known=%v ttft_ucb=%v cost_known=%v",
				m["account_id"], m["attempts"], m["successes"], m["insufficient"], m["ttft_known"], m["ttft_ucb"], m["cost_known"])
			if att >= 30 {
				qualified++
				if !known || ucb <= 1.0 {
					return false, fmt.Sprintf("充分候选 acc=%v TTFT 不真实（known=%v ucb=%v）", aid, known, ucb)
				}
			}
		}
		if qualified == 0 {
			return false, "frontier 尚无充分候选"
		}
		return true, "充分候选 frontier TTFT 真实"
	})
	// ============ 4b. 探索份额注活 → 单隔离协同 + 成本序 ============
	// 探索份额已接线（§H）：Primary 在位时 explore-first 份额（bp>0，1%
	// floor）仍把首尝试注入未知候选，sticky-primary 饥饿不复存在。本阶段断
	// 言确定性性质而非脆弱的"三候选齐晋升"时序结果：
	// (i) 零隔离注活——有界轮询内至少一名非 primary 候选样本单调增长（份额
	// 活着的直接证明），且 primary 同步增长（流量整体活着）；
	// (ii) 单隔离协同——n>=30 是阈值门：份额终会推所有人过门，但为把 E2E
	// 压进分钟级，只失能当前 primary 名单一遍（fenced + 重编译 + 候选排除，
	// 与阶段 3 同款常规操作）：无 primary → bp=10000 全探索，全部流量确
	// 定性落到落后者；达标即恢复。多轮迭代体操已删除，单次隔离是本阶段唯
	// 一的管理面写。恢复后静态全量重编译看到全员充分（live cell 累计，无
	// 需新流量，确定性）→ Primary 全员按成本升序（B 贵 2.5× 居末）。
	t.Log("阶段 4b：探索份额注活 → 单隔离协同 + 成本序")
	require.NotEmpty(t, primaries, "4a 必须已产生领袖 primary：%v", primaries)
	// rtQualityCounts 实例层 attempts 快照（fp→aid 经 fpAcc 落定）。
	rtQualityCounts := func() map[int64]int64 {
		t.Helper()
		rows, err := env.pg.Query(context.Background(), `
			SELECT encode(candidate_fingerprint,'hex') AS fp, COALESCE(sum(attempts),0) AS n
			FROM routing_quality_instance_minute GROUP BY 1`)
		require.NoError(t, err)
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var fp string
			var n int64
			if err := rows.Scan(&fp, &n); err != nil {
				break
			}
			if aid, ok := fpAcc[fp]; ok {
				got[aid] = n
			}
		}
		return got
	}
	allIDs := []int64{accA, accB, accE}
	// rtPrimaryIDs 读 plan 主路由当前 primary 名单。
	rtPrimaryIDs := func() []int64 {
		t.Helper()
		_, plan := rtPlan(t, env)
		r := rtFindRoute(plan, g1, "openai-chat", rtModel)
		require.NotNil(t, r, "主路由必须在 plan 中")
		ids, _ := r["primary"].([]any)
		var out []int64
		for _, id := range ids {
			if f, ok := id.(float64); ok {
				out = append(out, int64(f))
			}
		}
		return out
	}

	// ---- 4b(i) 探索份额注活（零隔离）：Primary 在位时未知候选必须持续
	// 积累样本。基线快照后打节奏化流量（10/轮，与 4a 同节奏，flush 跟得
	// 上）：有界 300 请求内至少一名非 primary 候选严格增长即命中（此处
	// bp≈367，零注活概率 ~1e-5，early-exit，通常数十请求即命中），且
	// primary 同步增长（排除流量整体停滞的伪命中）。DB 实例层为断言源
	// （durable，非 live cell）。
	t.Log("4b(i)：探索份额注活（零隔离）")
	base := rtQualityCounts()
	leaders := map[int64]bool{}
	for _, aid := range rtPrimaryIDs() {
		leaders[aid] = true
	}
	var laggards []int64
	for _, aid := range allIDs {
		if !leaders[aid] {
			laggards = append(laggards, aid)
		}
	}
	t.Logf("4b(i) 基线 per=%v leaders=%v laggards=%v", base, rtPrimaryIDs(), laggards)
	// 条件断言：4a 收敛轮内若多名候选同轮过门，首个重编译可能直接全员协
	// 同——此时"非 primary 增长"空真（无非 primary 可增长），跳过注活轮询
	// （否则 300s 超时是测试自杀，不是产品问题）；4b(ii) 的免隔离分支与
	// 全员协同断言覆盖此形。
	grew := ""
	if len(laggards) == 0 {
		t.Log("4b(i) 跳过注活：4a 已全员协同（条件空真）")
	} else {
		rtPollLong(t, "非 primary 候选样本增长（探索份额注活）", 300*time.Second, func() (bool, string) {
			for j := 0; j < 10; j++ {
				if c, rb := rtChat(t, env, k1, rtModel, nil); c != 200 {
					return false, fmt.Sprintf("注活流量异常 status=%d body=%.120s", c, rb)
				}
			}
			got := rtQualityCounts()
			for _, aid := range laggards {
				if got[aid] > base[aid] {
					// 存活交叉检查：任一 leader 增长即证明流量整体活着。必须
					// 是"任一"而非"全部"——primary[1..] 会在 primary[0]
					// 健康时位置性冻结（同 sticky 逻辑在 primary 车道内；
					// benign：已充分收敛，只是不服务），全员增长永不成立。
					// leader 增长若尚未落库可见（flush 滞后），不判失败只
					// 继续轮询——有界轮询吸收滞后，无 sleep 同步。
					for lid := range leaders {
						if got[lid] > base[lid] {
							grew = fmt.Sprintf("acc=%d %d→%d per=%v", aid, base[aid], got[aid], got)
							return true, grew
						}
					}
					return false, fmt.Sprintf("laggard acc=%d 已增长，待 leader 交叉确认 per=%v base=%v", aid, got, base)
				}
			}
			return false, fmt.Sprintf("per=%v base=%v", got, base)
		})
		t.Logf("4b(i) 探索份额注活命中：%s", grew)
	}

	// ---- 4b(ii) 单隔离协同（有界、一次性）：若全员已充分（份额自然推过
	// 门），跳过隔离；否则只失能当前 primary 名单（通常 1 名）——无 primary
	// 即 bp=10000 全探索，全部流量确定性落到落后者。每轮 10 请求全落未失
	// 能集（鸽巢：落后者每轮 ≥5 样本），≤15 轮（150 请求）必全员充分；达标
	// 即恢复。失能/恢复均为常规 fenced 操作（阶段 3 同款），编译驱逐全程
	// 事件驱动，无 nudge。
	t.Log("4b(ii)：单隔离协同")
	got := rtQualityCounts()
	needIsolation := false
	for _, aid := range allIDs {
		if got[aid] < 30 {
			needIsolation = true
		}
	}
	disabled := map[int64]bool{}
	if !needIsolation {
		t.Logf("4b(ii) 免隔离：探索份额已自然推全员充分 per=%v", got)
	} else {
		leadersNow := rtPrimaryIDs()
		require.NotEmpty(t, leadersNow, "隔离前 primary 名单不得为空")
		gen, _ := rtPlan(t, env)
		for _, aid := range leadersNow {
			rtSetEnabled(t, env, aid, false)
			disabled[aid] = true
		}
		gen = rtWaitGenBump(t, env, gen, fmt.Sprintf("失能 %v 后重编译", leadersNow))
		t.Logf("失能 %v，gen=%d", leadersNow, gen)
		rtPollLong(t, fmt.Sprintf("失能 %v 从候选排除", leadersNow), 60*time.Second, func() (bool, string) {
			_, plan := rtPlan(t, env)
			r := rtFindRoute(plan, g1, "openai-chat", rtModel)
			if r == nil {
				return false, "主路由暂不可见"
			}
			cands, _ := r["candidates"].([]any)
			for _, cc := range cands {
				if m, ok := cc.(map[string]any); ok {
					if aid, ok := m["account_id"].(float64); ok && disabled[int64(aid)] {
						return false, fmt.Sprintf("acc=%d 仍在候选", int64(aid))
					}
				}
			}
			return true, "已排除"
		})
		// 节奏化隔离流量（10/轮 + 每轮读 DB）：背靠背连发会压垮 PG flush
		// 预算（2s/轮超时 → 延期重试 → durable 可见性滞后约 60s），轮询节奏
		// 让 flush 跟上；一旦全员充分即停。
		rtPollLong(t, "落后者全员 n>=30", 300*time.Second, func() (bool, string) {
			for j := 0; j < 10; j++ {
				if c, rb := rtChat(t, env, k1, rtModel, nil); c != 200 {
					return false, fmt.Sprintf("隔离晋升流量异常 status=%d body=%.120s", c, rb)
				}
			}
			got2 := rtQualityCounts()
			for _, aid := range allIDs {
				if got2[aid] < 30 {
					return false, fmt.Sprintf("per=%v", got2)
				}
			}
			return true, fmt.Sprintf("全员充分 per=%v", got2)
		})
		t.Logf("4b(ii) 隔离达标 per=%v，恢复 %v", rtQualityCounts(), disabled)
		genCur, _ := rtPlan(t, env)
		for aid := range disabled {
			rtSetEnabled(t, env, aid, true)
		}
		genCur = rtWaitGenBump(t, env, genCur, "恢复后重编译")
		t.Logf("恢复 %v，gen=%d", disabled, genCur)
	}
	got = rtQualityCounts()
	for _, aid := range allIDs {
		require.GreaterOrEqual(t, got[aid], int64(30), "全员须充分：per=%v", got)
	}
	// 全员协同断言：全员充分后静态全量重编译看到全员充分 → Primary 全员
	// 按成本升序（有隔离则恢复后重编译已发布；免隔离则事件驱动重编译已发
	// 布——两种路径收敛到同一 plan 态）。B 最贵 → 居末；A/E 同价 ×1.0 不锁相对序。
	var primariesB []any
	rtPollLong(t, "全员协同 Primary 且 B 居末", 120*time.Second, func() (bool, string) {
		_, plan := rtPlan(t, env)
		r := rtFindRoute(plan, g1, "openai-chat", rtModel)
		if r == nil {
			return false, "主路由暂不可见"
		}
		primariesB, _ = r["primary"].([]any)
		pos := map[int64]int{}
		for i, id := range primariesB {
			if f, ok := id.(float64); ok {
				pos[int64(f)] = i
			}
		}
		for _, aid := range []int64{accA, accB, accE} {
			if _, ok := pos[aid]; !ok {
				return false, fmt.Sprintf("primary=%v 缺 %d", primariesB, aid)
			}
		}
		if pos[accB] != len(primariesB)-1 {
			return false, fmt.Sprintf("B 须居末 primary=%v", primariesB)
		}
		return true, fmt.Sprintf("全员协同 primary=%v", primariesB)
	})
	// 成本面：B 候选 upstream_cost_multiplier_bp=25000，A=10000。
	_, planB := rtPlan(t, env)
	rB := rtFindRoute(planB, g1, "openai-chat", rtModel)
	require.NotNil(t, rB, "主路由必须在 plan 中")
	bp := map[int64]float64{}
	for _, cc := range rB["candidates"].([]any) {
		m, _ := cc.(map[string]any)
		aid, _ := m["account_id"].(float64)
		v, _ := m["upstream_cost_multiplier_bp"].(float64)
		bp[int64(aid)] = v
	}
	require.Equal(t, float64(10000), bp[accA], "A 成本 ×1.0 → 10000bp：%v", bp)
	require.Equal(t, float64(25000), bp[accB], "B 成本 ×2.5 → 25000bp：%v", bp)
	// 全员 durable 和 + frontier 真实（缺陷 C 在协同路径同样成立）。
	rtPollLong(t, "全员实例层 TTFT 和非零", 120*time.Second, func() (bool, string) {
		rows, err := env.pg.Query(context.Background(), `
			SELECT encode(candidate_fingerprint,'hex') AS fp,
				COALESCE(sum(ttft_n),0) AS n,
				COALESCE(sum(ttft_sum_log_q32),0) AS s,
				COALESCE(sum(ttft_sumsq_log_q32),0) AS sq
			FROM routing_quality_instance_minute GROUP BY 1`)
		if err != nil {
			return false, err.Error()
		}
		defer rows.Close()
		got := map[int64][3]int64{}
		for rows.Next() {
			var fp string
			var n, s, sq int64
			if err := rows.Scan(&fp, &n, &s, &sq); err != nil {
				break
			}
			if aid, ok := fpAcc[fp]; ok {
				got[aid] = [3]int64{n, s, sq}
			}
		}
		for _, aid := range []int64{accA, accB, accE} {
			v := got[aid]
			if v[0] < 30 || v[1] == 0 || v[2] == 0 {
				return false, fmt.Sprintf("per=%v", got)
			}
		}
		return true, fmt.Sprintf("全员 TTFT 和非零 per=%v", got)
	})
	rtPollLong(t, "全员 frontier TTFT 真实", 180*time.Second, func() (bool, string) {
		toP := time.Now().UTC().Format(time.RFC3339)
		frontier := rtFrontier(t, env, hexMain, fromParam, toP)
		cands, _ := frontier["candidates"].([]any)
		seen := map[int64]bool{}
		for _, c := range cands {
			m, _ := c.(map[string]any)
			aid, _ := m["account_id"].(float64)
			known, _ := m["ttft_known"].(bool)
			ucb, _ := m["ttft_ucb"].(float64)
			if known && ucb > 1.0 {
				seen[int64(aid)] = true
			}
		}
		for _, aid := range []int64{accA, accB, accE} {
			if !seen[aid] {
				return false, fmt.Sprintf("acc=%d TTFT 尚未真实（seen=%v）", aid, seen)
			}
		}
		return true, "全员 frontier TTFT 真实"
	})

	// ============ 5. 窗口 429：未中放行 / 命中限流 ============
	t.Log("阶段 5：窗口 429 miss 放行 → hit 限流（C 单候选隔离组）")
	mark = rtAuditMaxSeq(t)
	// miss 语义以上游审计为准（网关按约定归一化上游 429 文本为 rate
	// limited，不透传 injected 原文——零透传契约）。发送循环带显式上界：
	// 偶发 reserve 竞态（静态/决策视图交替瞬间，单候选门拒收）只产生网关
	// 侧 429、无上游痕迹——此类请求不计入 429 窗口（无事件），重发之；上界
	// 6 发内须集满 2 次上游 429（窗口阈值 3：恰好不提前触发限流）。持续拒
	// 收则转储诊断后失败（见下）。
	sent := 0
	for rtAuditHits(t, mark, rtKey429, 429) < 2 && sent < 6 {
		c, rb := rtChat(t, env, k2, rtModel, nil)
		t.Logf("429 miss req %d status=%d body=%.160s", sent, c, rb)
		require.Equal(t, 429, c, "单候选 429 应向客户端透 429：%s", rb)
		sent++
	}
	if got := rtAuditHits(t, mark, rtKey429, 429); got != 2 {
		_, plan := rtPlan(t, env)
		if raw, err := json.Marshal(rtFindRoute(plan, g2, "openai-chat", rtModel)); err == nil {
			t.Logf("DIAG g2-route=%.3000s", raw)
		}
		if c, rb := env.admin(http.MethodGet, fmt.Sprintf("/accounts/%d", accC), nil); true {
			t.Logf("DIAG accountC status=%d body=%.2000s", c, rb)
		}
		if c, rb := env.admin(http.MethodGet, "/ops/workers", nil); true {
			t.Logf("DIAG ops/workers status=%d body=%.2000s", c, rb)
		}
		require.Equal(t, 2, got, "miss 阶段上游须如实见到 2 次 429（已发 %d）", sent)
	}
	t.Logf("429 miss：%d 发集满 2 次上游 429", sent)
	// 第 3 次达阈值（count_429_ge=3）→ 规则引擎异步开限流。规则管线异步，
	// 限流生效点须轮询捕获：逐次发请求，出现“上游零新尝试”即 engaged。
	c, rb = rtChat(t, env, k2, rtModel, nil)
	t.Logf("429 hit req status=%d body=%.160s", c, rb)
	engaged := false
	rtPollLong(t, "限流生效（上游零新尝试）", 90*time.Second, func() (bool, string) {
		before := rtAuditMaxSeq(t)
		c, rb := rtChat(t, env, k2, rtModel, nil)
		if c != 429 {
			return false, fmt.Sprintf("限流期异常 status=%d body=%.120s", c, rb)
		}
		after := rtAuditMaxSeq(t)
		engaged = after == before
		return engaged, fmt.Sprintf("audit %d→%d", before, after)
	})
	require.True(t, engaged, "限流必须生效（上游零新尝试）")
	// 冻结验证：连续 3 次限流请求零上游痕迹。
	mark = rtAuditMaxSeq(t)
	for i := 0; i < 3; i++ {
		c, _ := rtChat(t, env, k2, rtModel, nil)
		require.Equal(t, 429, c, "冻结期仍 429")
	}
	require.Equal(t, mark, rtAuditMaxSeq(t), "限流冻结期上游必须零尝试")

	// ============ 6. failover 聚合验证：429 尝试必被救援（全 200） ============
	// 5xx 按 replay 规则 terminal（见 evidence），故 g4 只含 F429+G：F 命中
	// → failover → G 成功；G 命中 → 直接成功。H 隔离 g5。
	t.Log("阶段 6：failover——429 尝试 + 全员 200（g4：F429/G健康）")
	r4 := rtWaitRoute(t, env, g4, "openai-chat", rtModel)
	candIDs := map[int64]bool{}
	for _, cc := range r4["candidates"].([]any) {
		if mm, ok := cc.(map[string]any); ok {
			if aid, ok := mm["account_id"].(float64); ok {
				candIDs[int64(aid)] = true
			}
		}
	}
	require.True(t, candIDs[accF] && candIDs[accG],
		"g4 候选须含 F/G：%v", candIDs)
	mark = rtAuditMaxSeq(t)
	for i := 0; i < 15; i++ {
		c, rb := rtChat(t, env, k4, rtModel, nil)
		require.Equal(t, 200, c, "failover req %d 必须 200：%s", i, rb)
	}
	require.Equal(t, 15, rtAuditHits(t, mark, rtKeyG, 200), "G 必须成功 15 次")
	bad := rtAuditHits(t, mark, rtKeyF429, 429)
	require.GreaterOrEqual(t, bad, 1, "15 次内至少 1 次命中 F（否则 failover 未被演练）")
	t.Logf("failover：F429 尝试=%d，客户端 200×15（全部被救援）", bad)

	// ============ 7. fail_account → 恢复 → PROBING ============
	t.Log("阶段 7：fail_account（D）→ recover → PROBING 重 admission")
	mark = rtAuditMaxSeq(t)
	c, rb = rtChat(t, env, k3, rtModel, nil)
	t.Logf("D 首错 status=%d body=%.160s", c, rb)
	require.NotEqual(t, 200, c, "D 恒 500 不得成功：%s", rb)
	require.Equal(t, 1, rtAuditHits(t, mark, rtKey500, 500), "首错必须到达上游 D")
	rtPollLong(t, "D 被规则 fail（FailedAt 落盘）", 60*time.Second, func() (bool, string) {
		m := rtAccountRead(t, env, accD)
		fa := m["FailedAt"]
		return fa != nil, fmt.Sprintf("FailedAt=%v", fa)
	})
	t.Log("D 已 fail")
	mark = rtAuditMaxSeq(t)
	c, rb = rtChat(t, env, k3, rtModel, nil)
	require.NotEqual(t, 200, c, "fail 后 D 不得再被选中：%s", rb)
	require.Equal(t, mark, rtAuditMaxSeq(t), "fail 后上游 D 必须零尝试")
	revBefore := rtRevision(t, env, accD)
	rtRecover(t, env, accD)
	m := rtAccountRead(t, env, accD)
	require.Nil(t, m["FailedAt"], "recover 后 FailedAt 须清除")
	require.Equal(t, revBefore+1, int64(m["LifecycleRevision"].(float64)), "recover 后 revision +1")
	// recover  bump revision 后须等重编译（含 D 的新 plan 发布），否则旧 plan
	// 仍排除 D——与阶段 4 同一编译语义。
	genD, _ := rtPlan(t, env)
	rtWaitGenBump(t, env, genD, "recover 后重编译（含 D）")
	_, planD := rtPlan(t, env)
	rD := rtFindRoute(planD, g3, "openai-chat", rtModel)
	require.NotNil(t, rD, "g3 路由必须在 plan 中")
	inD := false
	for _, cc := range rD["candidates"].([]any) {
		if mm, ok := cc.(map[string]any); ok {
			if aid, ok := mm["account_id"].(float64); ok && int64(aid) == accD {
				inD = true
			}
		}
	}
	require.True(t, inD, "recover 后 D 必须回到 g3 候选")
	// PROBING 按 probe 节拍放行（非按需）：轮询发请求，首个到达上游 D 的即
	// probe admission（D 上游仍 500，状态码不断言，只看上游归因 +1）。
	rtPollLong(t, "PROBING probe admission（D 重被选中）", 90*time.Second, func() (bool, string) {
		before := rtAuditMaxSeq(t)
		c, rb := rtChat(t, env, k3, rtModel, nil)
		after := rtAuditMaxSeq(t)
		dHits := 0
		for _, e := range rtAudit(t) {
			if e.Seq > before && e.Key == rtKey500 {
				dHits++
			}
		}
		_ = after
		if dHits > 0 {
			return true, fmt.Sprintf("D probe admission：status=%d body=%.120s", c, rb)
		}
		return false, fmt.Sprintf("status=%d audit %d→%d", c, before, after)
	})

	// ============ 8. 硬连续：previous_response_id 跨 store 钉住 ============
	t.Log("阶段 8：硬连续 previous_response_id 钉住同账号")
	respBody := func(extra map[string]any) (int, string) {
		body := map[string]any{"model": rtModel, "input": "hi"}
		for kk, vv := range extra {
			body[kk] = vv
		}
		return env.aiReq(http.MethodPost, "/v1/responses", k1, body)
	}
	c, rb = respBody(nil)
	require.Equal(t, 200, c, "responses 首呼：%s", rb)
	rid, _ := jsonGet(t, rb, "id").(string)
	require.NotEmpty(t, rid, "responses 首呼须回 id：%s", rb)
	keyOfLastResp := func(after int64) string {
		var last string
		var ms int64 = -1
		for _, e := range rtAudit(t) {
			if e.Seq > after && e.Path == "/v1/responses" && e.Status == 200 && e.Seq > ms {
				ms = e.Seq
				last = e.Key
			}
		}
		return last
	}
	mark = rtAuditMaxSeq(t)
	k0 := keyOfLastResp(mark - 1000000) // 首呼即最近 responses 200（mark 前无其它）
	require.NotEmpty(t, k0, "首呼须有上游归因")
	for i := 0; i < 2; i++ {
		after := rtAuditMaxSeq(t)
		c, rb = respBody(map[string]any{"input": "follow", "previous_response_id": rid})
		require.Equal(t, 200, c, "连续跟随 %d：%s", i, rb)
		rid2, _ := jsonGet(t, rb, "id").(string)
		require.NotEmpty(t, rid2, "跟随须回新 id")
		require.Equal(t, k0, keyOfLastResp(after), "跟随必须钉住首呼账号 %s", k0)
		rid = rid2
	}
	c, rb = respBody(map[string]any{"input": "x", "previous_response_id": "rsp_nope_123"})
	require.Equal(t, 410, c, "未知 previous_response_id 须 410：%s", rb)

	// ============ 9. Sankey 流量守恒 + frontier ============
	t.Log("阶段 9：routing-flow 守恒（first_dispatch==terminal）")
	var flow map[string]any
	rtPollLong(t, "flow edges 落库可见", 90*time.Second, func() (bool, string) {
		toP := time.Now().UTC().Format(time.RFC3339)
		flow = rtFlow(t, env, hexMain, fromParam, toP)
		fd, _ := flow["first_dispatch_chains"].(float64)
		return fd > 0, fmt.Sprintf("first_dispatch_chains=%v", flow["first_dispatch_chains"])
	})
	toP2 := time.Now().UTC().Format(time.RFC3339)
	flow = rtFlow(t, env, hexMain, fromParam, toP2)
	fd, _ := flow["first_dispatch_chains"].(float64)
	tc, _ := flow["terminal_chains"].(float64)
	require.Equal(t, fd, tc, "守恒：first_dispatch 必须 == terminal（fd=%v tc=%v flow=%v）", fd, tc, flow)
	require.Greater(t, fd, float64(0), "主路由须有完整链")
	t.Logf("守恒 fd==tc==%v", fd)

	// ============ 10. 多协议 + 计费无回归 ============
	t.Log("阶段 10：messages/images/models + 计费落库")
	c, rb = env.aiReq(http.MethodPost, "/v1/messages", k1, map[string]any{
		"model": rtModel, "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "anthropic messages：%s", rb)
	c, rb = env.aiReq(http.MethodPost, "/v1/images/generations", k1, map[string]any{
		"model": "rt-img-model", "prompt": "a cat",
	})
	require.Equal(t, 200, c, "images：%s", rb)
	c, rb = env.aiReq(http.MethodGet, "/v1/models", k1, nil)
	require.Equal(t, 200, c, "models list：%s", rb)
	require.Contains(t, rb, rtModel, "models 须含 rt-model：%s", rb)
	// 计费：余额严格减少 + usage_logs 有成本行 + images 行 cost>0。
	balAfter := env.balance(u1)
	require.Less(t, balAfter, balBefore, "u1 余额必须减少（%d → %d）", balBefore, balAfter)
	n, err := env.dbInt(`SELECT count(*) FROM usage_logs WHERE user_id=$1 AND cost>0`, u1)
	require.NoError(t, err)
	require.Greater(t, n, int64(0), "u1 须有成本 usage 行")
	var imgCost int64
	rtPollLong(t, "images 成本行落库", 30*time.Second, func() (bool, string) {
		cost, err := env.dbInt(`SELECT cost FROM usage_logs WHERE user_id=$1 AND model='rt-img-model' ORDER BY id DESC LIMIT 1`, u1)
		if err != nil {
			return false, err.Error()
		}
		imgCost = cost
		return true, fmt.Sprintf("imgCost=%d", cost)
	})
	require.Greater(t, imgCost, int64(0), "images 行 cost>0")
	t.Logf("协议+计费 OK：余额 %d→%d，成本行 %d，images cost=%d", balBefore, balAfter, n, imgCost)
}
