// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e 计费全链路端到端测试：真实网关 + fakeupstream + 真实 PostgreSQL。
//
// 运行（前置：本机 PostgreSQL 容器，见 TEST_DATABASE_URL 或默认
// postgres://postgres:c3api@localhost:15432/postgres；测试自行 DROP/CREATE
// c3api_e2e 库）：
//
//	go test -tags e2e -run TestBillingE2E ./tools/e2e -v -timeout 600s
//
// 覆盖：manual 设价（含 priority/fast/above 矩阵）→ usagelog cost 断言 +
// 余额毫分扣减 + FEFO 临时额度优先扣；余额不足/未设价 402；tier strip/reject
// 策略；组价格倍率 + 用户-组专属倍率（按组挂载，含 0 = 免费不扣费）；sync 后
// litellm 行矩阵填充且 manual 不被覆盖；usagelog 按日分区写入；SIGTERM 优雅
// 停机（流式中断 → 日志 cost 不丢，扣费完整 flush）。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const (
	adminToken = "e2e-admin-token"
	jwtSecret  = "e2e-jwt-secret-change-me"
	serverAddr = "127.0.0.1:18090" // 避开本机 VPN/代理已占用端口段（18080-18089 曾被占）
	upAddr     = "127.0.0.1:19110"
	dbName     = "c3api_e2e"
)

// adminURL / aiURL 端点基址。
func adminURL(p string) string { return "http://" + serverAddr + "/api/admin" + p }
func aiURL(p string) string    { return "http://" + serverAddr + p }

// pricesFixture 本地 litellm 价格表（fakeupstream 之外的独立 httptest 服务；
// 矩阵字段与 fetcher 精确 key 对齐，换算 ×1e11 毫分/1M tokens）。
const pricesFixture = `{
  "e2e-litellm-model": {
    "litellm_provider": "anthropic",
    "mode": "chat",
    "input_cost_per_token": 2.5e-6,
    "output_cost_per_token": 1e-5,
    "cache_read_input_token_cost": 2.5e-7,
    "cache_creation_input_token_cost": 1.25e-6,
    "input_cost_per_token_priority": 5e-6,
    "output_cost_per_token_priority": 2e-5,
    "cache_read_input_token_cost_priority": 5e-7,
    "cache_creation_input_token_cost_priority": 2.5e-6,
    "input_cost_per_token_flex": 2e-6,
    "output_cost_per_token_flex": 8e-6,
    "cache_read_input_token_cost_flex": 2e-7,
    "cache_creation_input_token_cost_flex": 1e-6,
    "input_cost_per_token_above_256k_tokens": 1.5e-6,
    "output_cost_per_token_above_256k_tokens": 7.5e-6,
    "cache_read_input_token_cost_above_256k_tokens": 1.5e-7,
    "cache_creation_input_token_cost_above_256k_tokens": 7.5e-7,
    "input_cost_per_token_above_256k_tokens_priority": 3e-6,
    "output_cost_per_token_above_256k_tokens_priority": 1.5e-5,
    "cache_read_input_token_cost_above_256k_tokens_priority": 3e-7,
    "cache_creation_input_token_cost_above_256k_tokens_priority": 1.5e-6,
    "input_cost_per_token_above_256k_tokens_flex": 1.2e-6,
    "output_cost_per_token_above_256k_tokens_flex": 6e-6,
    "cache_read_input_token_cost_above_256k_tokens_flex": 1.2e-7,
    "cache_creation_input_token_cost_above_256k_tokens_flex": 6e-7,
    "provider_specific_entry": { "fast": 6.0 },
    "max_input_tokens": 1000000,
    "max_output_tokens": 64000,
    "supports_prompt_caching": true
  },
  "e2e-manual-model": {
    "litellm_provider": "openai",
    "mode": "chat",
    "input_cost_per_token": 1e-6,
    "output_cost_per_token": 4e-6
  }
}`

type e2eEnv struct {
	t   *testing.T
	pg  *pgxpool.Pool // c3api_e2e 库（SQL 断言）
	tmp string
}

// admin 管理面请求：body nil = 无请求体；返回状态码 + 响应体。
func (e *e2eEnv) admin(method, path string, body any) (int, string) {
	e.t.Helper()
	return e.req(method, adminURL(path), "Bearer "+adminToken, body)
}

// user 用户面请求（JWT）；key 为 AI 请求（/v1）鉴权 key 时走 ai。
func (e *e2eEnv) req(method, url, auth string, body any) (int, string) {
	e.t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	require.NoError(e.t, err)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// aiReq AI 请求（/v1，Bearer <key> 鉴权）。
func (e *e2eEnv) aiReq(method, path, key string, body any) (int, string) {
	e.t.Helper()
	return e.req(method, aiURL(path), "Bearer "+key, body)
}

// dbVal 单值查询。
func (e *e2eEnv) dbVal(query string, args ...any) (string, error) {
	e.t.Helper()
	var v string
	err := e.pg.QueryRow(context.Background(), query, args...).Scan(&v)
	return v, err
}

// dbInt 单值 int64 查询。
func (e *e2eEnv) dbInt(query string, args ...any) (int64, error) {
	e.t.Helper()
	var v int64
	err := e.pg.QueryRow(context.Background(), query, args...).Scan(&v)
	return v, err
}

// balance 用户余额（毫分）。
func (e *e2eEnv) balance(userID int64) int64 {
	e.t.Helper()
	v, err := e.dbInt(`SELECT balance FROM users WHERE id=$1`, userID)
	require.NoError(e.t, err)
	return v
}

// sleepFlush 等待 flusher 周期落库（配置 flush_interval=300ms，留足余量）。
func sleepFlush() { time.Sleep(900 * time.Millisecond) }

// waitSnapshot 等待去抖窗口 + 一次重载完成（O2：管理面变更生效延迟 ≤200ms
// 窗口 + 一次重载时长——变更后的断言性请求须落在重载之后，等效旧实现同步
// invalidate 的"变更即生效"，只是生效点推迟到窗口到点）。
func waitSnapshot() { time.Sleep(300 * time.Millisecond) }

// lastLog 最新一条计费日志的计费列。
type billLogRow struct {
	Cost       int64
	Tier       string
	AboveHit   bool
	Overdraft  bool
	StatusCode int
	ErrorType  string
}

// lastLogFor 按 model 取最新日志行（计费列断言）。usage_logs 瘦身（分表设计）：
// status_code/error_message 已移除（错误审计归 err_logs），error_type 保留
// （值域收敛 none/abort）。
func (e *e2eEnv) lastLogFor(model string) billLogRow {
	e.t.Helper()
	var r billLogRow
	err := e.pg.QueryRow(context.Background(), `
		SELECT cost, COALESCE(billing_tier,''), above_hit, overdraft, COALESCE(error_type,'')
		FROM usage_logs WHERE model=$1 ORDER BY id DESC LIMIT 1`, model).
		Scan(&r.Cost, &r.Tier, &r.AboveHit, &r.Overdraft, &r.ErrorType)
	require.NoError(e.t, err)
	return r
}

func TestBillingE2E(t *testing.T) {
	env := &e2eEnv{t: t}
	ctx := context.Background()

	// --- 0. 数据库准备：DROP + CREATE c3api_e2e ---
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://postgres:c3api@localhost:15432/postgres"
	}
	// TEST_DATABASE_URL 指向 c3api_test 库：取其 host/port/api/user/password，目标库换 c3api_e2e。
	adminPool, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `CREATE DATABASE `+dbName)
	require.NoError(t, err)
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i+1] + dbName
		if !strings.Contains(dsn, "?") {
			dsn += "?sslmode=disable"
		}
	}
	env.pg, err = pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(env.pg.Close)

	// --- 1. 构建并启动 fakeupstream + 网关 ---
	env.tmp = t.TempDir()
	// go test 的 cwd = 包目录；构建路径相对仓库根（本文件位于 tools/e2e/）。
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
	upBin := build("./tools/fakeupstream", "fakeupstream.exe")
	srvBin := build("./cmd/server", "server.exe")

	up := exec.Command(upBin, "-addr", upAddr, "-chunks", "10", "-latency", "5ms", "-fail400", "fail400-key")
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	require.NoError(t, up.Start())
	t.Cleanup(func() { _ = up.Process.Kill() })

	// 本地价格表（sync 场景）
	pricesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pricesFixture))
	}))
	t.Cleanup(pricesSrv.Close)

	cfg := fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
admin = { token = "%s" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = true }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "10s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "5s" }
billing = { enabled = true, flush_interval = "300ms", balance_refresh_interval = "500ms" }
`, serverAddr, adminToken, jwtSecret, dsn)
	cfgPath := filepath.Join(env.tmp, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	srv := exec.Command(srvBin, "-config", cfgPath)
	srvLog, err := os.Create(filepath.Join(env.tmp, "server.log"))
	require.NoError(t, err)
	srv.Stdout, srv.Stderr = srvLog, srvLog
	// 优雅停机信号：非 Windows 直接 SIGTERM；Windows 上 Go 的 Process.Signal
	// 仅支持 Kill（os/exec_windows.go），需 CREATE_NEW_PROCESS_GROUP +
	// GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, pid) 投递控制台事件（main
	// 对 os.Interrupt 与 SIGTERM 走同一 NotifyContext 优雅路径）。
	if isWindows() {
		srv.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	}
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Process.Kill() })

	// 就绪：轮询 /api/admin/settings 直到 200（ent migrate + 分区 bootstrap 完成）。
	// 就绪前连接被拒属正常（启动中），原始请求不中断测试；须带 admin token
	// （否则 401 恒不满足）。
	ready := false
	deadline := time.Now().Add(60 * time.Second)
	for !ready && time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, adminURL("/settings"), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			ready = resp.StatusCode == http.StatusOK
		}
		if !ready {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !ready { // 失败诊断：进程状态 + 服务日志
		t.Logf("server ProcessState=%v", srv.ProcessState)
		if data, err := os.ReadFile(filepath.Join(env.tmp, "server.log")); err == nil {
			t.Logf("--- server.log ---\n%s", data)
		}
		t.Fatalf("server 未在 60s 内就绪")
	}
	// 就绪后关闭日志文件句柄（Windows 上句柄未关可能阻止后续读取；退出码仍由 Wait 捕获）
	_ = srvLog.Close()

	// 失败诊断（O1 收尾）：任何场景失败 → 转储内置网关 server.log 与最新
	// usage_logs 行（flusher 落库时序/DB 状态疑点直接可见——此前失败无日志
	// 难定位）。Cleanup LIFO：先于 srv.Kill / pool.Close 执行，数据完整。
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if data, err := os.ReadFile(filepath.Join(env.tmp, "server.log")); err == nil {
			t.Logf("--- server.log (test failed) ---\n%s", data)
		}
		rows, err := env.pg.Query(ctx, `SELECT id, user_id, model, cost, COALESCE(error_type,''), created_at FROM usage_logs ORDER BY id DESC LIMIT 20`)
		if err != nil {
			t.Logf("usage_logs 状态转储失败: %v", err)
			return
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var id, uid, cost int64
			var model, etype string
			var created time.Time
			if err := rows.Scan(&id, &uid, &model, &cost, &etype, &created); err != nil {
				t.Logf("usage_logs 扫描失败: %v", err)
				break
			}
			fmt.Fprintf(&sb, "id=%d user=%d model=%s cost=%d err=%s created=%s\n",
				id, uid, model, cost, etype, created.Format(time.RFC3339Nano))
		}
		t.Logf("--- usage_logs latest 20 (test failed) ---\n%s", sb.String())
	})

	// --- 2. 基建：模板/账号/组 ---
	tplID := env.create("/templates", map[string]any{
		"name": "e2e-tpl", "base_url": "http://" + upAddr,
		"supported_formats": []string{"openai-chat", "openai-responses", "anthropic"},
		"models":            []string{"e2e-model", "e2e-matrix-model", "e2e-mult-model", "e2e-litellm-model", "e2e-manual-model", "e2e-noprice-model"},
	})
	g1 := env.create("/groups", map[string]any{"name": "e2e-grp"})
	g2 := env.create("/groups", map[string]any{"name": "e2e-grp2", "price_multiplier": 2.0})
	g3 := env.create("/groups", map[string]any{"name": "e2e-grp3"})
	env.create("/accounts", map[string]any{
		"name": "e2e-acc", "template_id": tplID, "upstream_key": "up-key-1",
		"group_ids": []int64{g1, g2, g3},
	})

	// --- 3. manual 设价（e2e-model 基础 + fast；e2e-matrix-model 全矩阵；
	// e2e-mult-model 基础；API 输入 USD/1M 正常值，存储毫分）---
	// USD/1M：prompt 100.0（$100）、completion 200.0（$200）。
	putPrice(t, env, "e2e-model", map[string]any{
		"prompt_price_per_million": 100.0, "completion_price_per_million": 200.0,
		"fast_multiplier": 2.0,
	})
	// 矩阵：priority/flex 替换档 + above_threshold 5 + above 三组 + fast ×2。
	// auto:   pt10→(5×10+5×5)=75   ct20→(5×20+15×10)=250  → 325
	// priority: pt10→(5×15+5×7)=110  ct20→(5×25+15×12)=305 → 415
	// flex:    pt10→(5×12+5×4)=80   ct20→(5×18+15×8)=210  → 290
	// fast:    325 × 2 = 650
	// anthropic 中止（仅 pt=10）：(5×10+5×5)=75
	putPrice(t, env, "e2e-matrix-model", map[string]any{
		"prompt_price_per_million": 100.0, "completion_price_per_million": 200.0,
		"priority_prompt_price_per_million": 150.0, "priority_completion_price_per_million": 250.0,
		"flex_prompt_price_per_million": 120.0, "flex_completion_price_per_million": 180.0,
		"above_threshold":                5,
		"above_prompt_price_per_million": 50.0, "above_completion_price_per_million": 100.0,
		"above_priority_prompt_price_per_million": 70.0, "above_priority_completion_price_per_million": 120.0,
		"above_flex_prompt_price_per_million": 40.0, "above_flex_completion_price_per_million": 80.0,
		"fast_multiplier": 2.0,
	})
	putPrice(t, env, "e2e-mult-model", map[string]any{
		"prompt_price_per_million": 100.0, "completion_price_per_million": 200.0,
	})

	// --- 4. 用户/密钥 ---
	u1 := createUser(t, env, "e2e-user@example.com", 10.0) // 1,000,000 毫分
	_, u1Key := userKey(t, env, u1, g1)
	// O2 去抖：u1 须在余额快照中才能通过计费预检（窗口 200ms + 重载）。
	waitSnapshot()

	// ============ 场景 1：矩阵计费 + 余额毫分扣减 ============
	t.Log("场景 1：manual 矩阵设价 → usagelog cost 断言 + 余额毫分扣减")
	bal := int64(1000000)
	chatReq := func(model string, extra map[string]any) int {
		body := map[string]any{"model": model, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for k, v := range extra {
			body[k] = v
		}
		code, resp := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, body)
		require.Equal(t, 200, code, "chat %s: %s", model, resp)
		return 200
	}

	// auto：cost 325，above_hit
	chatReq("e2e-matrix-model", nil)
	sleepFlush()
	r := env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(325), r.Cost, "auto 档矩阵计费")
	require.Equal(t, "auto", r.Tier)
	require.True(t, r.AboveHit, "pt/ct 均超阈值 → above_hit")
	require.False(t, r.Overdraft)
	bal -= 325
	require.Equal(t, bal, env.balance(u1), "余额毫分扣减")

	// priority：cost 415
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "priority"})
	sleepFlush()
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(415), r.Cost, "priority 档计费")
	require.Equal(t, "priority", r.Tier)
	bal -= 415
	require.Equal(t, bal, env.balance(u1))

	// flex：cost 290
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "flex"})
	sleepFlush()
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(290), r.Cost, "flex 档计费")
	require.Equal(t, "flex", r.Tier)
	bal -= 290
	require.Equal(t, bal, env.balance(u1))

	// fast：基础档 × fast_multiplier 2 → 650
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "fast"})
	sleepFlush()
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(650), r.Cost, "fast 倍率计费")
	require.Equal(t, "fast", r.Tier)
	bal -= 650
	require.Equal(t, bal, env.balance(u1))

	// ============ 场景 2：FEFO 临时额度优先扣 + 余额不足 402 ============
	t.Log("场景 2：FEFO 临时额度优先扣；余额不足 402")
	u2 := createUser(t, env, "fefo@example.com", 0.01) // 1,000 毫分
	u2Token, u2Key := userKey(t, env, u2, g1)
	// O2 去抖：u2 须在余额快照中（下方首个请求的计费预检依赖）。
	waitSnapshot()
	// temp_balance 兑换码 500 毫分（API 面值 USD：0.005 = $0.005 = 500 毫分）
	codeResp, respBody := env.admin(http.MethodPost, "/redemption-codes", map[string]any{
		"type": "temp_balance", "value": 0.005, "resource_expires_at": "2030-01-01T00:00:00Z", "count": 1,
	})
	require.Equal(t, 200, codeResp, "gen code: %s", respBody)
	code := jsonGet(t, respBody, "codes", 0, "Code")
	rec, rb := env.req(http.MethodPost, aiURL("/api/user/redemptions"), "Bearer "+u2Token, map[string]any{"code": code})
	require.Equal(t, 200, rec, "redeem: %s", rb)

	// 请求 cost 500：临时额度优先扣（temp 500 → 0，余额不动）
	code2, resp2 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, code2, "fefo req: %s", resp2)
	sleepFlush()
	tempLeft, err := env.dbInt(`SELECT COALESCE(SUM(amount),0) FROM temp_balances WHERE user_id=$1 AND amount>0`, u2)
	require.NoError(t, err)
	require.Zero(t, tempLeft, "临时额度扣至 0")
	require.Equal(t, int64(1000), env.balance(u2), "余额未被临时额度请求消耗")
	r = env.lastLogFor("e2e-model")
	require.Equal(t, int64(500), r.Cost)

	// 第二笔：余额 1000 → 500；第三笔：500 → 0；第四笔：0 仍放行（spec 2026-08-15 余额 0 放行，FEFO 覆盖）→ -500 overdraft；第五笔：负余额预检 402
	for _, want := range []int64{500, 0} {
		c, rb2 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
			"model": "e2e-model", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		require.Equal(t, 200, c, "drain: %s", rb2)
		sleepFlush()
		require.Equal(t, want, env.balance(u2))
	}
	// 余额 0 仍放行一次（overdraft），见 proxy/billing_test.go:691 “余额 0 放行”
	c, rbOver := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "overdraft must still 200 (bal 0 → -500): %s", rbOver)
	sleepFlush()
	require.Equal(t, int64(-500), env.balance(u2), "透支后余额 -500")
	rOver := env.lastLogFor("e2e-model")
	require.True(t, rOver.Overdraft, "透支行 overdraft=true")
	c, rb3 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 402, c, "insufficient must 402: %s", rb3)

	// ============ 场景 3：未设价 402 ============
	t.Log("场景 3：未设价模型 → 402（error_type=billing）")
	c, rb4 := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-noprice-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 402, c, "no price must 402: %s", rb4)
	// 402 拒绝行（分表裁决 R3-M1）：错误审计面归 err_logs——error_type=billing
	// 全值 + status_code；usage_logs 零行（放行路径语义：失败行不入 usage_logs）。
	sleepFlush() // 等待 errlog worker 落库（独立 500ms 批间隔）
	code3, resp3 := env.admin(http.MethodGet, "/err_logs?model=e2e-noprice-model&from=2000-01-01T00:00:00Z&to=2030-01-01T00:00:00Z", nil)
	require.Equal(t, 200, code3, "err logs: %s", resp3)
	require.Contains(t, resp3, `"billing"`, "402 拒绝行 err_logs error_type=billing")
	// R4-M3：HTTP 面 ↔ DB 面交叉验证（同一条拒绝行经 errlog worker 落库 → HTTP 查询可见）
	var dbEType string
	err = env.pg.QueryRow(context.Background(), `
		SELECT error_type FROM err_logs
		WHERE model='e2e-noprice-model' ORDER BY id DESC LIMIT 1`).Scan(&dbEType)
	require.NoError(t, err)
	require.Equal(t, "billing", dbEType, "err_logs DB 面与 HTTP 面一致")
	code3u, resp3u := env.admin(http.MethodGet, "/usage_logs?model=e2e-noprice-model&from=2000-01-01T00:00:00Z&to=2030-01-01T00:00:00Z", nil)
	require.Equal(t, 200, code3u, "usage logs: %s", resp3u)
	// 新分页契约：游标分页无 total，空结果为 rows 空数组（旧 total:0 已移除）
	require.Contains(t, resp3u, `"rows":[]`, "402 失败行不入 usage_logs（放行路径语义）")

	// ============ 场景 4：tier strip/reject 策略（settings 三 key） ============
	t.Log("场景 4：service_tier_policy_priority strip/reject")
	// reject → 400 拒绝（不转发）
	c, rb5 := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_priority", "value": "reject",
	})
	require.Equal(t, 200, c, "set reject: %s", rb5)
	c, rb6 := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-matrix-model", "stream": true, "service_tier": "priority",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 400, c, "reject must 400: %s", rb6)
	// strip → 200 转发（转发体剥 service_tier；计费照常按 priority 档）
	c, rb7 := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_priority", "value": "strip",
	})
	require.Equal(t, 200, c, "set strip: %s", rb7)
	c, rb8 := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-matrix-model", "stream": true, "service_tier": "priority",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "strip must forward: %s", rb8)
	sleepFlush()
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, "priority", r.Tier, "strip 照常计费 priority 档")
	require.Equal(t, int64(415), r.Cost)
	bal -= 415
	require.Equal(t, bal, env.balance(u1))
	// 恢复 passthrough
	c, rb9 := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_priority", "value": "passthrough",
	})
	require.Equal(t, 200, c, "restore passthrough: %s", rb9)

	// ============ 场景 4b：fast 档同策略（M-1 回归：caller 门控含 TierFast） ============
	t.Log("场景 4b：service_tier_policy_fast strip/reject")
	// reject → 400 拒绝（不转发）
	c, rba := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_fast", "value": "reject",
	})
	require.Equal(t, 200, c, "set fast reject: %s", rba)
	c, rbb := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-matrix-model", "stream": true, "service_tier": "fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 400, c, "fast reject must 400: %s", rbb)
	// strip → 200 转发（转发体剥 service_tier；计费照常按 fast 档 ×2）
	c, rbc := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_fast", "value": "strip",
	})
	require.Equal(t, 200, c, "set fast strip: %s", rbc)
	c, rbd := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-matrix-model", "stream": true, "service_tier": "fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "fast strip must forward: %s", rbd)
	sleepFlush()
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, "fast", r.Tier, "strip 照常计费 fast 档")
	require.Equal(t, int64(650), r.Cost, "fast ×2：325×2 = 650")
	bal -= 650
	require.Equal(t, bal, env.balance(u1))
	// 恢复 passthrough
	c, rbe := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_fast", "value": "passthrough",
	})
	require.Equal(t, 200, c, "restore fast passthrough: %s", rbe)

	// ============ 场景 5：价格倍率（组倍率 / 用户-组专属倍率覆盖 / 0 免费） ============
	t.Log("场景 5：组倍率 ×2 → 扣费 ×2；用户-组专属倍率覆盖组（按组挂载）；0 = 免费不扣费")
	u4 := createUser(t, env, "mult@example.com", 10.0) // grp2 倍率 2.0
	_, u4Key := userKey(t, env, u4, g2)
	// O2 去抖：u4 须在余额快照中（下方首个请求的计费预检依赖）。
	waitSnapshot()
	chat := func(key string, model string) {
		c, rb := env.aiReq(http.MethodPost, "/v1/chat/completions", key, map[string]any{
			"model": model, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		require.Equal(t, 200, c, "chat: %s", rb)
		sleepFlush()
	}
	// 组倍率 2.0 → 500×2 = 1000
	chat(u4Key, "e2e-mult-model")
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(1000), r.Cost, "组倍率 ×2")
	require.Equal(t, int64(1000000-1000), env.balance(u4))

	// 用户-组专属倍率覆盖组（T3.5 修正：按组挂载，经 assignments 的 multipliers）：
	// 0.5 → 500×0.5 = 250
	c, rb10 := env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments",
		map[string]any{"user_ids": []int64{u4}, "multipliers": map[string]any{strconv.FormatInt(u4, 10): 0.5}})
	require.Equal(t, 200, c, "set assignment mult: %s", rb10)
	waitSnapshot() // O2 去抖：新倍率须已进快照（请求按快照计费）
	chat(u4Key, "e2e-mult-model")
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(250), r.Cost, "用户-组专属倍率覆盖组倍率")
	require.Equal(t, int64(1000000-1000-250), env.balance(u4))

	// 专属倍率 0 = 免费：cost 0 不扣费
	c, _ = env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments",
		map[string]any{"user_ids": []int64{u4}, "multipliers": map[string]any{strconv.FormatInt(u4, 10): 0.0}})
	require.Equal(t, 200, c, "set free mult")
	waitSnapshot() // O2 去抖：新倍率须已进快照
	chat(u4Key, "e2e-mult-model")
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(0), r.Cost, "0 = 免费（cost 0）")
	require.Equal(t, int64(1000000-1000-250), env.balance(u4), "免费不扣费")

	// 组倍率 0 = 免费 + 余额 0 预检放行（免费用户不 402）
	u5 := createUser(t, env, "free@example.com", 0.0) // 余额 0
	_, u5Key := userKey(t, env, u5, g3)
	c, rb11 := env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g3, 10), map[string]any{"name": "e2e-grp3", "price_multiplier": 0.0})
	require.Equal(t, 200, c, "set group free: %s", rb11)
	waitSnapshot() // O2 去抖：u5 入余额快照 + g3 倍率 0 进倍率快照（同窗口合并一次重载）
	chat(u5Key, "e2e-mult-model")
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(0), r.Cost, "组倍率 0 = 免费")
	require.Equal(t, int64(0), env.balance(u5), "余额 0 免费用户不扣费不 402")

	// ============ 场景 5.5：授予读取 + 用户维度分组（GET assignments / GET+PUT users/{id}/groups） ============
	t.Log("场景 5.5：组授予读取与用户维度分组端点（与组维度 PUT 交叉验证）")
	// 用户维度 PUT：u4 → [g2, g3]，g2 专属倍率 1.5（正常值 → 万分数 15000 存储）
	c, rb30 := env.admin(http.MethodPut, "/users/"+strconv.FormatInt(u4, 10)+"/groups",
		map[string]any{"group_ids": []int64{g2, g3}, "multipliers": map[string]any{strconv.FormatInt(g2, 10): 1.5}})
	require.Equal(t, 200, c, "put user groups: %s", rb30)
	require.Contains(t, rb30, strconv.FormatInt(g2, 10), "响应含 g2")
	require.Contains(t, rb30, strconv.FormatInt(g3, 10), "响应含 g3")
	// 组维度读取交叉验证：g2 含 u4 且专属倍率 1.5 回显
	c, rb31 := env.admin(http.MethodGet, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments", nil)
	require.Equal(t, 200, c, "get group assignments: %s", rb31)
	require.Contains(t, rb31, strconv.FormatInt(u4, 10), "g2 授予含 u4")
	require.Contains(t, rb31, `"`+strconv.FormatInt(u4, 10)+`":1.5`, "g2 的 u4 专属倍率 1.5")
	// 用户维度 GET 回读
	c, rb32 := env.admin(http.MethodGet, "/users/"+strconv.FormatInt(u4, 10)+"/groups", nil)
	require.Equal(t, 200, c, "get user groups: %s", rb32)
	require.Contains(t, rb32, `"`+strconv.FormatInt(g2, 10)+`":1.5`, "用户视角倍率 1.5")
	// 替换语义：只留 g3 → g2 撤销（g2 不再含 u4）
	c, rb33 := env.admin(http.MethodPut, "/users/"+strconv.FormatInt(u4, 10)+"/groups",
		map[string]any{"group_ids": []int64{g3}})
	require.Equal(t, 200, c, "replace user groups: %s", rb33)
	c, rb34 := env.admin(http.MethodGet, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments", nil)
	require.Equal(t, 200, c, "get g2 after replace: %s", rb34)
	require.NotContains(t, rb34, strconv.FormatInt(u4, 10), "撤销后 g2 不含 u4")
	// 空数组 = 清空；缺失资源 → 404
	c, rb35 := env.admin(http.MethodPut, "/users/"+strconv.FormatInt(u4, 10)+"/groups", map[string]any{"group_ids": []int64{}})
	require.Equal(t, 200, c, "clear user groups: %s", rb35)
	c, rb36 := env.admin(http.MethodGet, "/users/999999/groups", nil)
	require.Equal(t, 404, c, "missing user: %s", rb36)
	c, rb37 := env.admin(http.MethodGet, "/groups/999999/assignments", nil)
	require.Equal(t, 404, c, "missing group: %s", rb37)

	// ============ 场景 6：sync 后 litellm 行矩阵填充 + manual 不被覆盖 ============
	t.Log("场景 6：sync → litellm 行 22 列填充；manual 行不被覆盖")
	c, rb12 := env.admin(http.MethodPut, "/settings", map[string]any{"key": "price_source_url", "value": pricesSrv.URL + "/prices.json"})
	require.Equal(t, 200, c, "set price url: %s", rb12)
	c, rb13 := env.admin(http.MethodPost, "/pricing/sync", nil)
	require.Equal(t, 200, c, "sync: %s", rb13)
	require.Contains(t, rb13, `"rows":2`)
	c, rb14 := env.admin(http.MethodGet, "/pricing?model=e2e-litellm-model", nil)
	require.Equal(t, 200, c, "pricing list: %s", rb14)
	row := jsonGet(t, rb14, "rows", 0, "").(map[string]any)
	for field, want := range map[string]any{
		"PromptPricePerMillion":                  float64(2.5),
		"CompletionPricePerMillion":              float64(10.0),
		"CacheReadPricePerMillion":               float64(0.25),
		"CacheCreationPricePerMillion":           float64(1.25),
		"PriorityPromptPricePerMillion":          float64(5.0),
		"PriorityCacheCreationPricePerMillion":   float64(2.5),
		"FlexCompletionPricePerMillion":          float64(8.0),
		"AboveThreshold":                         float64(256000),
		"AbovePromptPricePerMillion":             float64(1.5),
		"AbovePriorityCompletionPricePerMillion": float64(15.0),
		"AboveFlexCacheReadPricePerMillion":      float64(0.12),
		"FastMultiplier":                         float64(6.0),
	} {
		got, ok := row[field]
		require.True(t, ok, "litellm 行含 %s", field)
		require.Equal(t, want, got, "字段 %s 值", field)
	}
	require.Equal(t, "litellm", row["Source"], "sync 行 source=litellm")

	// manual 接管后 sync 不覆盖
	putPrice(t, env, "e2e-manual-model", map[string]any{
		"prompt_price_per_million": 1.23456, "completion_price_per_million": 6.54321,
	})
	c, rb15 := env.admin(http.MethodPost, "/pricing/sync", nil)
	require.Equal(t, 200, c, "sync2: %s", rb15)
	require.Contains(t, rb15, `"updated":1`, "manual 行不计入 updated（litellm 行仍更新）")
	c, rb16 := env.admin(http.MethodGet, "/pricing?model=e2e-manual-model", nil)
	require.Equal(t, 200, c, "pricing manual: %s", rb16)
	row = jsonGet(t, rb16, "rows", 0, "").(map[string]any)
	require.Equal(t, "manual", row["Source"], "manual 行不被 sync 覆盖")
	require.Equal(t, float64(1.23456), row["PromptPricePerMillion"], "manual 价保持（USD/1M 回显）")

	// ============ 场景 7：usagelog 按日分区 ============
	t.Log("场景 7：usagelog 当日分区存在且行落入正确分区")
	part, err := env.dbVal(`SELECT to_regclass('usage_logs_' || to_char(now(),'YYYYMMDD'))::text`)
	require.NoError(t, err)
	require.NotEqual(t, "<NULL>", part, "当日分区存在", part)
	require.True(t, part != "" && part != "NULL", "当日分区存在: %s", part)
	total, err := env.dbInt(`SELECT count(*) FROM usage_logs`)
	require.NoError(t, err)
	partTotal, err := env.dbInt(`SELECT count(*) FROM usage_logs_` + strings.TrimPrefix(part, "usage_logs_"))
	require.NoError(t, err)
	require.Equal(t, total, partTotal, "全部行落入当日分区")

	// ============ 场景 8：新用户 402 窗口回归（评审 M-2 / O2） ============
	// 建用户 → 立即请求（<0.5s）→ 200：新用户必须在去抖窗口 + 一次重载后
	// 即刻进入余额快照（防 ≤10s BalanceRefreshInterval 402 窗口）。
	t.Log("场景 8：建用户 → 立即请求（<0.5s）→ 200（去抖窗口内余额快照收敛）")
	uNew := createUser(t, env, "fresh-e2e@example.com", 10.0) // 1,000,000 毫分
	waitSnapshot()                                            // 去抖窗口（200ms）+ 重载；随后请求须落在重载之后
	_, uNewKey := userKey(t, env, uNew, g1)
	// 评审 I-1：t0 从 userKey 返回后起算（用户已就绪、密钥已取）——createUser/
	// login 的 API 往返不计入 <0.5s 预算，只测"用户就绪后首次请求"的去抖收敛链。
	t0 := time.Now()
	c, rbNew := env.aiReq(http.MethodPost, "/v1/chat/completions", uNewKey, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "新用户立即请求必须 200（余额快照未收敛 → 402）: %s", rbNew)
	require.Less(t, time.Since(t0), 500*time.Millisecond,
		"userKey 后 <0.5s（去抖窗口 200ms + 一次重载 + 请求余量；若 invalidate 未按矩阵去抖合并会超时或 402）")
	sleepFlush()
	r = env.lastLogFor("e2e-model")
	require.Equal(t, int64(500), r.Cost, "新用户计费正常（快照内余额扣减）")
	require.Equal(t, int64(1000000-500), env.balance(uNew), "新用户余额扣减")

	// ============ 场景 8.5：上游 4xx 错误文本落盘（部署故障修复） ============
	// 独立模板/账号/组：上游 key = fail400-key → fakeupstream 注入 400（透传，
	// 不转移）。分表设计（用户裁决）：4xx 失败行不入 usage_logs——err_logs
	// error_message 必须落上游错误 body（根因锁定靠文本）。
	t.Log("场景 8.5：上游 4xx → err_logs error_message 落盘")
	tpl400 := env.create("/templates", map[string]any{
		"name": "e2e-tpl-400", "base_url": "http://" + upAddr,
		"supported_formats": []string{"openai-chat"},
		"models":            []string{"e2e-model"},
	})
	g400 := env.create("/groups", map[string]any{"name": "e2e-grp-400"})
	env.create("/accounts", map[string]any{
		"name": "e2e-acc-400", "template_id": tpl400, "upstream_key": "fail400-key",
		"group_ids": []int64{g400},
	})
	u400 := createUser(t, env, "e2e-400@example.com", 10.0)
	_, u400Key := userKey(t, env, u400, g400)
	waitSnapshot() // O2 去抖：u400 须在余额快照中（计费预检依赖）
	c, rb400 := env.aiReq(http.MethodPost, "/v1/chat/completions", u400Key, map[string]any{
		"model": "e2e-model", "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 400, c, "4xx 透传: %s", rb400)
	require.Contains(t, rb400, "injected 400", "4xx 必须透传上游原始 body")
	sleepFlush() // errlog worker 周期落盘（err_logs 独立管道，500ms 批间隔）
	var errMsg string
	err = env.pg.QueryRow(context.Background(), `
		SELECT COALESCE(error_message,'') FROM err_logs
		WHERE model='e2e-model' ORDER BY id DESC LIMIT 1`).Scan(&errMsg)
	require.NoError(t, err)
	require.Contains(t, errMsg, "injected 400", "4xx 错误文本必须落盘 err_logs.error_message")
	require.LessOrEqual(t, len(errMsg), 500, "error_message 域内截断 500")
	// 分表验证：4xx 失败行不入 usage_logs（仅成功/abort 放行路径行）
	var nRows int
	err = env.pg.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM usage_logs WHERE model='e2e-model' AND error_type <> 'none'`).Scan(&nRows)
	require.NoError(t, err)
	require.Zero(t, nRows, "4xx 失败行不入 usage_logs（error_type 值域收敛 none/abort——错误审计归 err_logs）")

	// ============ 场景 9：SIGTERM 优雅停机（流式中断 → 日志 cost 不丢） ============
	t.Log("场景 9：优雅停机——流式中断计费完整 flush")
	balBefore := env.balance(u1)
	// anthropic 长流（chunks 1000 × 5ms ≈ 5s > Shutdown 2s 优雅窗口：强断发生
	// 时流仍在途）：message_start 已带 pt=10，停机强断后按已累积 token 计费：
	// auto 档 (5×10+5×5)=75 毫分
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequest(http.MethodPost, aiURL("/v1/messages"), strings.NewReader(
			`{"model":"e2e-matrix-model","stream":true,"max_tokens":64,"chunks":1000,`+
				`"messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+u1Key)
		req.Header.Set("Content-Type", "application/json")
		_, _ = http.DefaultClient.Do(req) // 连接被强断 → 错误忽略（网关侧已计费）
	}()
	time.Sleep(800 * time.Millisecond) // 等 message_start 已消费、流仍在途

	require.NoError(t, stopGracefully(srv), "优雅停机信号")
	waitExit(t, srv, 20*time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("流式请求 goroutine 未退出")
	}
	sleepFlush()
	// 流式中断日志：cost 完整（不丢计费）+ 余额扣减（排空落库）
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(75), r.Cost, "优雅停机后流式中断日志 cost 不丢")
	require.Equal(t, "auto", r.Tier)
	require.True(t, r.AboveHit)
	require.Equal(t, balBefore-75, env.balance(u1), "优雅停机排空扣费完整")

	// R3-I1：abort 双轨关联——同一 request_id 在 usage_logs（计费明细）与
	// err_logs（错误审计）各一行（豁免队列恒落盘，停机排空后可见）
	var rid string
	err = env.pg.QueryRow(context.Background(), `
		SELECT request_id FROM usage_logs
		WHERE model='e2e-matrix-model' ORDER BY id DESC LIMIT 1`).Scan(&rid)
	require.NoError(t, err)
	require.NotEmpty(t, rid, "usage_logs 停机排空行必须有 request_id")
	var dbN int
	err = env.pg.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM err_logs WHERE request_id=$1`, rid).Scan(&dbN)
	require.NoError(t, err)
	require.Equal(t, 1, dbN, "abort 双轨：同一 request_id 在 err_logs 一行")
	var dbSC int
	err = env.pg.QueryRow(context.Background(), `
		SELECT status_code FROM err_logs WHERE request_id=$1`, rid).Scan(&dbSC)
	require.NoError(t, err)
	require.Greater(t, dbSC, 0, "err_logs 双轨行含 status_code 全值（错误审计面）")
}

// ---- helpers ----

func isWindows() bool { return os.PathSeparator == '\\' }

// createNewProcessGroup CREATE_NEW_PROCESS_GROUP（Windows 控制台事件投递用）。
const createNewProcessGroup = 0x00000200

// stopGracefully 优雅停机信号：非 Windows 用 SIGTERM；Windows 用
// GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, 子进程组)（Go Process.Signal
// 仅支持 Kill）。main 对 os.Interrupt/SIGTERM 同一 NotifyContext 优雅路径。
func stopGracefully(cmd *exec.Cmd) error {
	if isWindows() {
		// 子进程以 CREATE_NEW_PROCESS_GROUP 启动 → 组 id = 子进程 pid
		return windowsCtrlBreak(cmd.Process.Pid)
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

// windowsCtrlBreak 通过 x/sys/windows 投递 CTRL_BREAK_EVENT 到指定进程组。
func windowsCtrlBreak(pid int) error {
	return windowsGenerateCtrlBreak(uint32(pid))
}

func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }()
	select {
	case err := <-ch:
		require.NoError(t, err, "server 优雅退出（退出码 0）")
	case <-time.After(timeout):
		t.Fatalf("server 未在 %s 内退出", timeout)
	}
}

// putPrice manual 设价（断言 200）。model 走 query 参数——模型名可含 `/`，
// 路径参数形态已在价格表重构时废弃（openapi /pricing PUT/DELETE）。
func putPrice(t *testing.T, env *e2eEnv, model string, body map[string]any) {
	t.Helper()
	c, rb := env.admin(http.MethodPut, "/pricing?model="+url.QueryEscape(model), body)
	require.Equal(t, 200, c, "put price %s: %s", model, rb)
}

// createUser 管理面创建用户（balance USD float64），返回 userID。
func createUser(t *testing.T, env *e2eEnv, email string, balanceUSD float64) int64 {
	t.Helper()
	c, rb := env.admin(http.MethodPost, "/users", map[string]any{
		"email": email, "password": "s3cret-pass", "balance": balanceUSD,
	})
	require.Equal(t, 200, c, "create user %s: %s", email, rb)
	return int64(jsonGet(t, rb, "ID").(float64))
}

// userKey 登录拿 JWT + 在组内建 key，返回 (token, key 明文)。
func userKey(t *testing.T, env *e2eEnv, userID, groupID int64) (string, string) {
	t.Helper()
	// 登录：邮箱需还原——users 表按 id 查 email
	email, err := env.dbVal(`SELECT email FROM users WHERE id=$1`, userID)
	require.NoError(t, err)
	c, rb := env.req(http.MethodPost, aiURL("/api/user/auth/login"), "", map[string]any{
		"email": email, "password": "s3cret-pass",
	})
	require.Equal(t, 200, c, "login: %s", rb)
	token := jsonGet(t, rb, "token").(string)
	c, rb = env.req(http.MethodPost, aiURL("/api/user/keys"), "Bearer "+token, map[string]any{
		"name": "k-" + email, "group_id": groupID,
	})
	require.Equal(t, 200, c, "create key: %s", rb)
	return token, jsonGet(t, rb, "key").(string)
}

// create POST 创建资源并取回 ID（响应 map 顶层的 ID 字段）。
func (e *e2eEnv) create(path string, body map[string]any) int64 {
	e.t.Helper()
	c, rb := e.admin(http.MethodPost, path, body)
	require.Equal(e.t, 200, c, "create %s: %s", path, rb)
	return int64(jsonGet(e.t, rb, "ID").(float64))
}

// jsonGet 逐层取 JSON 值（key 为空 = 返回当前层 map；idx 用于数组层）。
func jsonGet(t *testing.T, body string, keys ...any) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(body), &v), "json parse: %s", body)
	for _, k := range keys {
		switch kk := k.(type) {
		case string:
			if kk == "" {
				continue
			}
			m, ok := v.(map[string]any)
			require.True(t, ok, "expect map at %v in %s", kk, body)
			v = m[kk]
		case int:
			arr, ok := v.([]any)
			require.True(t, ok, "expect array at %v in %s", kk, body)
			v = arr[kk]
		}
	}
	return v
}
