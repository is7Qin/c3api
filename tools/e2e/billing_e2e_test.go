// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e 计费全链路端到端测试：真实网关 + fakeupstream + 真实 PostgreSQL。
//
// 资源/进程生命周期由共享 harness 独占（每 run 唯一 PG 库 / Redis 逻辑库 / 动态端口 /
// run 级 build 一次 / run 级 lazy migrate / 子进程登记与回收）：本文件只写配置、请求
// 与断言。主网关 + 两个辅助网关（billing off / usage_capture=false）共用同一 DSN 与
// run 级 Redis 逻辑库、配置不同；aux 由 harness gateway() 多次启动，lazy migrate 在
// run 内只发生一次。
//
// 运行（前置：base PostgreSQL + Redis，deploy/test-compose.yml 缺省 15432/16379；
// P1 起进独立 e2e CI job）：
//
//	TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres" \
//	  C3API_REDIS_ADDR="127.0.0.1:16379" \
//	  go test -tags e2e -run TestBillingE2E ./tools/e2e -v -timeout 600s
//
// 覆盖：manual 设价（含 priority/fast/above 矩阵）→ usagelog cost 断言 +
// 余额毫分扣减 + FEFO 临时额度优先扣；余额不足/未设价 402；tier strip/reject
// 策略；组价格倍率 + 用户-组专属倍率（按组挂载，含 0 = 免费不扣费）；sync 后
// litellm 行矩阵填充且 manual 不被覆盖；usagelog 按日分区写入；SIGTERM 优雅
// 停机（流式中断 → 日志 cost 不丢，扣费完整 flush）；辅助实例 billing off /
// usage_capture=false 的 quota 独立语义。
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// billingCfgTOML 主网关配置（与迁移前逐键一致；动态 addr / 每 run 唯一 DSN / Redis
// 逻辑库全部来自 harness 的 gatewayInputs——场景不拼资源字面量、不设子进程 env）。
func billingCfgTOML(in gatewayInputs) string {
	return fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
redis = { addr = "%s", db = %d }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = true }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "10s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "5s" }
billing = { enabled = true, flush_interval = "300ms", balance_refresh_interval = "500ms" }
`, in.Addr, jwtSecret, in.DSN, in.RedisAddr, in.RedisDB)
}

// billingAuxCfgTOML 辅助实例配置：同 DSN 同 Redis 逻辑库同 secret，仅翻转
// billing.enabled / proxy.usage_capture，并把 quota_flush_interval 调到 1s 加速收敛
// （与迁移前 startAuxGateway 逐键一致）。
func billingAuxCfgTOML(in gatewayInputs, billingOn, usageCaptureOn bool) string {
	return fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
redis = { addr = "%s", db = %d }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = %v }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "10s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "1s" }
billing = { enabled = %v, flush_interval = "300ms", balance_refresh_interval = "500ms" }
`, in.Addr, jwtSecret, in.DSN, in.RedisAddr, in.RedisDB, usageCaptureOn, billingOn)
}

// pollBalance 轮询用户余额至期望毫分值。余额收敛蕴含对应 usage_logs 行已落库
// 且被 billing 消费——其后的 lastLogFor 断言无需再等落库。
func pollBalance(t *testing.T, env *e2eEnv, userID, want int64) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("user=%d 余额→%d", userID, want), func() (bool, string) {
		got := env.balance(userID)
		return got == want, fmt.Sprintf("balance got=%d want=%d", got, want)
	})
}

// pollUserLastLogCost 轮询某用户最新 usage_logs 行落库且 cost 收敛到期望值
// （cost=0 免费场景余额不动，无法以余额为收敛信号，退化为行级轮询）。
func pollUserLastLogCost(t *testing.T, env *e2eEnv, userID, want int64) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("user=%d 最新日志 cost→%d", userID, want), func() (bool, string) {
		cost, err := env.dbInt(`SELECT cost FROM usage_logs WHERE user_id=$1 ORDER BY id DESC LIMIT 1`, userID)
		if err != nil {
			return false, err.Error()
		}
		return cost == want, fmt.Sprintf("cost got=%d want=%d", cost, want)
	})
}

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
	run := newE2ERun(t, e2eRunOptions{})
	up := run.fakeUpstream(t, upstreamSpec{
		Name:  "billing-fakeupstream",
		Flags: []string{"-chunks", "10", "-latency", "5ms", "-fail400", "fail400-key"},
	})
	upAddr := up.Addr

	// 本地价格表（sync 场景；进程内 httptest，不是外部进程）。
	pricesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pricesFixture))
	}))
	t.Cleanup(pricesSrv.Close)

	// 主网关：每 run 唯一 PG 库/Redis 逻辑库 + 动态端口 + run 级 lazy migrate（建表只归
	// harness，场景不可能漏 migrate）；失败诊断由 harness 统一 dump。
	gw := run.gateway(t, gatewaySpec{Name: "billing-main", Config: billingCfgTOML})

	// env 管理面 helper 兼容视图：pg 本 run 唯一库 / addr 主网关动态地址 /
	// adminJWT bootstrap 后的 platform_admin（多 gateway 幂等登录由 harness 负责）。
	env := run.env(gw)

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
	// 统一价格表契约：基础价 PUT /prices/entry?model=（token 档 input_per_m/
	// output_per_m），矩阵档 PUT /prices/variants?model=（service_tier 替换档 /
	// ctx_min 分段 / multiplier 倍数 0..10）。模型 ID 含 "/"，一律查询参数。
	// USD/1M：prompt 100.0（$100）、completion 200.0（$200）。
	putPrice(t, env, "e2e-model", map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})
	putVariants(t, env, "e2e-model", []map[string]any{
		{"seq": 1, "service_tier": "fast", "multiplier": 2},
	})
	// 变体语义 = 整单切换（旧 above 分段计价已随三表退役）：pt10/ct20 每 token
	// 基数 in=10/out=20。
	// auto:     首中通配长上下文兜底 seq4 → 整单 (5,10)：10×5+20×10 = 250
	// priority: 首中 seq1 → (15,25)：150+500 = 650
	// flex:     首中 seq2 → (12,18)：120+360 = 480
	// fast:     首中 seq3 → 基础 500 ×2.0 = 1000
	// anthropic 中止（仅 pt=10）：seq4 → 10×5 = 50
	putPrice(t, env, "e2e-matrix-model", map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})
	putVariants(t, env, "e2e-matrix-model", []map[string]any{
		{"seq": 1, "service_tier": "priority", "set_input_per_m": 150.0, "set_output_per_m": 250.0},
		{"seq": 2, "service_tier": "flex", "set_input_per_m": 120.0, "set_output_per_m": 180.0},
		{"seq": 3, "service_tier": "fast", "multiplier": 2},
		// 通配长上下文兜底必须排在档位专属之后——首中即停，先匹配者赢
		{"seq": 4, "ctx_min": 5, "set_input_per_m": 50.0, "set_output_per_m": 100.0},
	})
	putPrice(t, env, "e2e-mult-model", map[string]any{
		"input_per_m": 100.0, "output_per_m": 200.0,
	})

	// --- 4. 用户/密钥 ---
	u1 := createUser(t, env, "e2e-user@example.com", 10.0) // 1,000,000 毫分
	_, u1Key := userKey(t, env, u1, g1)
	// 去抖：u1 须在余额快照中才能通过计费预检（窗口 200ms + 重载）。
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

	// auto：cost 250（首中通配 ctx 变体，整单切换）
	chatReq("e2e-matrix-model", nil)
	bal -= 250
	pollBalance(t, env, u1, bal) // 余额收敛蕴含日志行已落库且被 billing 消费
	r := env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(250), r.Cost, "auto 档矩阵计费")
	require.Equal(t, "auto", r.Tier)
	require.False(t, r.AboveHit, "above 分段已退役 → 恒 false")
	require.False(t, r.Overdraft)

	// priority：cost 650
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "priority"})
	bal -= 650
	pollBalance(t, env, u1, bal)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(650), r.Cost, "priority 档计费")
	require.Equal(t, "priority", r.Tier)

	// flex：cost 480
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "flex"})
	bal -= 480
	pollBalance(t, env, u1, bal)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(480), r.Cost, "flex 档计费")
	require.Equal(t, "flex", r.Tier)

	// fast：基础档 × fast 变体 multiplier 2（×2）→ 1000
	chatReq("e2e-matrix-model", map[string]any{"service_tier": "fast"})
	bal -= 1000
	pollBalance(t, env, u1, bal)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(1000), r.Cost, "fast 倍率计费")
	require.Equal(t, "fast", r.Tier)

	// ============ 场景 2：FEFO 临时额度优先扣 + 余额不足 402 ============
	t.Log("场景 2：FEFO 临时额度优先扣；余额不足 402")
	u2 := createUser(t, env, "fefo@example.com", 0.01) // 1,000 毫分
	u2Token, u2Key := userKey(t, env, u2, g1)
	// 去抖：u2 须在余额快照中（下方首个请求的计费预检依赖）。
	waitSnapshot()
	// temp_balance 兑换码 500 毫分（API 面值 USD：0.005 = $0.005 = 500 毫分）
	codeResp, respBody := env.admin(http.MethodPost, "/redemption-codes", map[string]any{
		"type": "temp_balance", "value": 0.005, "resource_expires_at": "2030-01-01T00:00:00Z", "count": 1,
	})
	require.Equal(t, 200, codeResp, "gen code: %s", respBody)
	code := jsonGet(t, respBody, "codes", 0, "Code")
	rec, rb := env.req(http.MethodPost, env.aiURL("/api/user/redemptions"), "Bearer "+u2Token, map[string]any{"code": code})
	require.Equal(t, 200, rec, "redeem: %s", rb)

	// 请求 cost 500：临时额度优先扣（temp 500 → 0，余额不动）
	code2, resp2 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, code2, "fefo req: %s", resp2)
	// 余额不动（temp 优先扣）：以临时额度清零为收敛信号（billing 消费该行后扣 temp）
	pollUntil(t, "FEFO 临时额度扣至 0", func() (bool, string) {
		left, err := env.dbInt(`SELECT COALESCE(SUM(amount),0) FROM temp_balances WHERE user_id=$1 AND amount>0`, u2)
		if err != nil {
			return false, err.Error()
		}
		return left == 0, fmt.Sprintf("temp_left=%d", left)
	})
	require.Equal(t, int64(1000), env.balance(u2), "余额未被临时额度请求消耗")
	r = env.lastLogFor("e2e-model")
	require.Equal(t, int64(500), r.Cost)

	// 第二笔：余额 1000 → 500；第三笔：500 → 0；第四笔：余额 0 预检 402（余额 ≤0 拒绝）
	for _, want := range []int64{500, 0} {
		c, rb2 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
			"model": "e2e-model", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		require.Equal(t, 200, c, "drain: %s", rb2)
		pollBalance(t, env, u2, want)
	}
	// 余额 0 拒绝：预检 ≤0 → 402（临时额度已耗尽，无 FEFO 兜底）；
	// 去抖等定向余额快照 Set 落定，避免读到扣减前的旧值。
	waitSnapshot()
	c, rb3 := env.aiReq(http.MethodPost, "/v1/chat/completions", u2Key, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 402, c, "balance 0 must 402: %s", rb3)

	// ============ 场景 3：未设价 402 ============
	t.Log("场景 3：未设价模型 → 402（error_type=billing）")
	c, rb4 := env.aiReq(http.MethodPost, "/v1/chat/completions", u1Key, map[string]any{
		"model": "e2e-noprice-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 402, c, "no price must 402: %s", rb4)
	// 402 拒绝行（分表裁决）：错误审计面归 err_logs——error_type=billing
	// 全值 + status_code；usage_logs 零行（放行路径语义：失败行不入 usage_logs）。
	// errlog worker 独立管道落库：轮询拒绝行出现（替代固定 sleep）
	var dbEType string
	pollUntil(t, "err_logs 402 拒绝行落库", func() (bool, string) {
		err := env.pg.QueryRow(context.Background(), `
			SELECT error_type FROM err_logs
			WHERE model='e2e-noprice-model' ORDER BY id DESC LIMIT 1`).Scan(&dbEType)
		if err != nil {
			return false, err.Error()
		}
		return true, "error_type=" + dbEType
	})
	// err_logs/usage_logs 的 raw 分区只保留保留期内数据（e2e 配 log_retention_days=2，
	// 服务端 raw horizon 默认 7d）；窗口起点必须落在保留期内，不能写 2000 年。
	winFrom := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	winTo := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code3, resp3 := env.admin(http.MethodGet, "/err_logs?model=e2e-noprice-model&from="+winFrom+"&to="+winTo, nil)
	require.Equal(t, 200, code3, "err logs: %s", resp3)
	require.Contains(t, resp3, `"billing"`, "402 拒绝行 err_logs error_type=billing")
	// HTTP 面 ↔ DB 面交叉验证（同一条拒绝行经 errlog worker 落库 → HTTP 查询可见）
	require.Equal(t, "billing", dbEType, "err_logs DB 面与 HTTP 面一致")
	code3u, resp3u := env.admin(http.MethodGet, "/usage_logs?model=e2e-noprice-model&from="+winFrom+"&to="+winTo, nil)
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
	bal -= 650
	pollBalance(t, env, u1, bal)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, "priority", r.Tier, "strip 照常计费 priority 档")
	require.Equal(t, int64(650), r.Cost)
	// 恢复 passthrough
	c, rb9 := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_priority", "value": "passthrough",
	})
	require.Equal(t, 200, c, "restore passthrough: %s", rb9)

	// ============ 场景 4b：fast 档同策略（回归：caller 门控含 TierFast） ============
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
	bal -= 1000
	pollBalance(t, env, u1, bal)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, "fast", r.Tier, "strip 照常计费 fast 档")
	require.Equal(t, int64(1000), r.Cost, "fast ×2：500×2 = 1000")
	// 恢复 passthrough
	c, rbe := env.admin(http.MethodPut, "/settings", map[string]any{
		"key": "service_tier_policy_fast", "value": "passthrough",
	})
	require.Equal(t, 200, c, "restore fast passthrough: %s", rbe)

	// ============ 场景 5：价格倍率（组倍率 / 用户-组专属倍率覆盖 / 0 免费） ============
	t.Log("场景 5：组倍率 ×2 → 扣费 ×2；用户-组专属倍率覆盖组（按组挂载）；0 = 免费不扣费")
	u4 := createUser(t, env, "mult@example.com", 10.0) // grp2 倍率 2.0
	_, u4Key := userKey(t, env, u4, g2)
	// 去抖：u4 须在余额快照中（下方首个请求的计费预检依赖）。
	waitSnapshot()
	chat := func(key string, model string) {
		c, rb := env.aiReq(http.MethodPost, "/v1/chat/completions", key, map[string]any{
			"model": model, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		require.Equal(t, 200, c, "chat: %s", rb)
	}
	// 组倍率 2.0 → 500×2 = 1000
	chat(u4Key, "e2e-mult-model")
	pollBalance(t, env, u4, 1000000-1000)
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(1000), r.Cost, "组倍率 ×2")

	// 用户-组专属倍率覆盖组（按组挂载，经 assignments 的 multipliers）：
	// 0.5 → 500×0.5 = 250
	c, rb10 := env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments",
		map[string]any{"user_ids": []int64{u4}, "multipliers": map[string]any{strconv.FormatInt(u4, 10): 0.5}})
	require.Equal(t, 200, c, "set assignment mult: %s", rb10)
	waitSnapshot() // 去抖：新倍率须已进快照（请求按快照计费）
	chat(u4Key, "e2e-mult-model")
	pollBalance(t, env, u4, 1000000-1000-250)
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(250), r.Cost, "用户-组专属倍率覆盖组倍率")

	// 专属倍率 0 = 免费：cost 0 不扣费
	c, _ = env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g2, 10)+"/assignments",
		map[string]any{"user_ids": []int64{u4}, "multipliers": map[string]any{strconv.FormatInt(u4, 10): 0.0}})
	require.Equal(t, 200, c, "set free mult")
	waitSnapshot() // 去抖：新倍率须已进快照
	chat(u4Key, "e2e-mult-model")
	pollUserLastLogCost(t, env, u4, 0) // cost=0 余额不动：以最新行落库为收敛信号
	r = env.lastLogFor("e2e-mult-model")
	require.Equal(t, int64(0), r.Cost, "0 = 免费（cost 0）")
	require.Equal(t, int64(1000000-1000-250), env.balance(u4), "免费不扣费")

	// 组倍率 0 = 免费 + 余额 0 预检放行（免费用户不 402）
	u5 := createUser(t, env, "free@example.com", 0.0) // 余额 0
	_, u5Key := userKey(t, env, u5, g3)
	c, rb11 := env.admin(http.MethodPut, "/groups/"+strconv.FormatInt(g3, 10), map[string]any{"name": "e2e-grp3", "price_multiplier": 0.0})
	require.Equal(t, 200, c, "set group free: %s", rb11)
	waitSnapshot() // 去抖：u5 入余额快照 + g3 倍率 0 进倍率快照（同窗口合并一次重载）
	chat(u5Key, "e2e-mult-model")
	pollUserLastLogCost(t, env, u5, 0) // u5 首行落库即收敛（此前无行，ErrNoRows 重试）
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
	c, rb14 := env.admin(http.MethodGet, "/prices/entry?model=e2e-litellm-model", nil)
	require.Equal(t, 200, c, "price entry: %s", rb14)
	row := jsonGet(t, rb14).(map[string]any)
	for field, want := range map[string]any{
		"Mode":           "token",
		"InputPerM":      float64(2.5),
		"OutputPerM":     float64(10.0),
		"CacheReadPerM":  float64(0.25),
		"CacheWritePerM": float64(1.25),
	} {
		got, ok := row[field]
		require.True(t, ok, "litellm 行含 %s", field)
		require.Equal(t, want, got, "字段 %s 值", field)
	}
	require.Equal(t, "litellm", row["Source"], "sync 行 source=litellm")
	// 矩阵字段迁至变体：priority/flex 替换档 + ctx_min 分段 + fast 万分倍率
	c, rb14v := env.admin(http.MethodGet, "/prices/variants?model=e2e-litellm-model", nil)
	require.Equal(t, 200, c, "variants list: %s", rb14v)
	vrows := jsonGet(t, rb14v, "rows").([]any)
	require.Len(t, vrows, 4, "litellm 行变体：priority/flex/above/fast")
	for _, vr := range vrows {
		vm := vr.(map[string]any)
		switch vm["ServiceTier"] {
		case "priority":
			require.Equal(t, float64(5.0), vm["SetInputPerM"], "priority 变体 SetInputPerM")
			require.Equal(t, float64(20.0), vm["SetOutputPerM"], "priority 变体 SetOutputPerM")
		case "flex":
			require.Equal(t, float64(2.0), vm["SetInputPerM"], "flex 变体 SetInputPerM")
			require.Equal(t, float64(8.0), vm["SetOutputPerM"], "flex 变体 SetOutputPerM")
		case "fast":
			require.Equal(t, float64(6), vm["multiplier"], "fast 变体 ×6.0")
		default:
			if vm["CtxMin"] != nil {
				require.Equal(t, float64(256000), vm["CtxMin"], "above 分段阈值 256k tokens")
				require.Equal(t, float64(1.5), vm["SetInputPerM"], "above 变体 SetInputPerM")
				require.Equal(t, float64(7.5), vm["SetOutputPerM"], "above 变体 SetOutputPerM")
			}
		}
	}

	// manual 接管后 sync 不覆盖
	putPrice(t, env, "e2e-manual-model", map[string]any{
		"input_per_m": 1.23456, "output_per_m": 6.54321,
	})
	c, rb15 := env.admin(http.MethodPost, "/pricing/sync", nil)
	require.Equal(t, 200, c, "sync2: %s", rb15)
	require.Contains(t, rb15, `"updated":1`, "manual 行不计入 updated（litellm 行仍更新）")
	c, rb16 := env.admin(http.MethodGet, "/prices/entry?model=e2e-manual-model", nil)
	require.Equal(t, 200, c, "price manual: %s", rb16)
	row = jsonGet(t, rb16).(map[string]any)
	require.Equal(t, "manual", row["Source"], "manual 行不被 sync 覆盖")
	require.Equal(t, float64(1.23456), row["InputPerM"], "manual 价保持（USD/1M 回显）")

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

	// ============ 场景 8：新用户 402 窗口回归 ============
	// 建用户 → 立即请求（<0.5s）→ 200：新用户必须在去抖窗口 + 一次重载后
	// 即刻进入余额快照（防 ≤10s BalanceRefreshInterval 402 窗口）。
	t.Log("场景 8：建用户 → 立即请求（<0.5s）→ 200（去抖窗口内余额快照收敛）")
	uNew := createUser(t, env, "fresh-e2e@example.com", 10.0) // 1,000,000 毫分
	waitSnapshot()                                            // 去抖窗口（200ms）+ 重载；随后请求须落在重载之后
	_, uNewKey := userKey(t, env, uNew, g1)
	// t0 从 userKey 返回后起算（用户已就绪、密钥已取）——createUser/
	// login 的 API 往返不计入 <0.5s 预算，只测"用户就绪后首次请求"的去抖收敛链。
	t0 := time.Now()
	c, rbNew := env.aiReq(http.MethodPost, "/v1/chat/completions", uNewKey, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "新用户立即请求必须 200（余额快照未收敛 → 402）: %s", rbNew)
	require.Less(t, time.Since(t0), 500*time.Millisecond,
		"userKey 后 <0.5s（去抖窗口 200ms + 一次重载 + 请求余量；若 invalidate 未按矩阵去抖合并会超时或 402）")
	pollBalance(t, env, uNew, 1000000-500)
	r = env.lastLogFor("e2e-model")
	require.Equal(t, int64(500), r.Cost, "新用户计费正常（快照内余额扣减）")

	// ============ 场景 8.5：上游 4xx 错误文本落盘（部署故障修复） ============
	// 独立模板/账号/组：上游 key = fail400-key → fakeupstream 注入 400（透传，
	// 不转移）。分表设计：4xx 失败行不入 usage_logs——err_logs
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
	waitSnapshot() // 去抖：u400 须在余额快照中（计费预检依赖）
	c, rb400 := env.aiReq(http.MethodPost, "/v1/chat/completions", u400Key, map[string]any{
		"model": "e2e-model", "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 400, c, "4xx 透传: %s", rb400)
	require.Contains(t, rb400, "injected 400", "4xx 必须透传上游原始 body")
	// errlog worker 独立管道落盘：轮询 4xx 行出现（替代固定 sleep）
	var errMsg string
	pollUntil(t, "err_logs 4xx 行落盘", func() (bool, string) {
		err := env.pg.QueryRow(context.Background(), `
			SELECT COALESCE(error_message,'') FROM err_logs
			WHERE model='e2e-model' ORDER BY id DESC LIMIT 1`).Scan(&errMsg)
		if err != nil {
			return false, err.Error()
		}
		return true, "error_message=" + errMsg
	})
	require.Contains(t, errMsg, "injected 400", "4xx 错误文本必须落盘 err_logs.error_message")
	require.LessOrEqual(t, len(errMsg), 500, "error_message 域内截断 500")
	// 分表验证：4xx 失败行不入 usage_logs（仅成功/abort 放行路径行）
	var nRows int
	err = env.pg.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM usage_logs WHERE model='e2e-model' AND error_type <> 'none'`).Scan(&nRows)
	require.NoError(t, err)
	require.Zero(t, nRows, "4xx 失败行不入 usage_logs（error_type 值域收敛 none/abort——错误审计归 err_logs）")

	// ============ 场景 10：Key quota（billing on 按最终 Cost 后扣并耗尽；quota=0 零回写） ============
	t.Log("场景 10：Key quota 按最终 Cost 后扣至耗尽 429；quota=0 不产生 quota 回写")
	uQ := createUser(t, env, "quota-on@example.com", 10.0) // 1,000,000 毫分（余额远大于 quota，拦截点必在 quota）
	qToken, qKey := userKeyQuota(t, env, uQ, g1, 1000)     // e2e-model 每请求 Cost=500 → 两笔耗尽
	waitSnapshot()                                         // 去抖：key 入鉴权快照
	chat(qKey, "e2e-model")
	chat(qKey, "e2e-model")
	pollQuotaUsed(t, env, qKey, 1000) // DB 回写收敛（quota_flush_interval=5s，有界轮询）
	c, rbQ := env.aiReq(http.MethodPost, "/v1/chat/completions", qKey, map[string]any{
		"model": "e2e-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 429, c, "quota 耗尽必须 429: %s", rbQ)
	require.Contains(t, rbQ, "key quota exhausted", "429 文案 = quota 耗尽（非并发/余额）")
	require.Equal(t, int64(1000), env.quotaUsed(qKey), "429 拒绝不产生新 delta（quota_used 恒 1000）")
	pollBalance(t, env, uQ, 1000000-1000) // 用户余额照常结算（quota 与余额两线独立）

	// quota=0（不限）零回写：kZero 请求放行；kProbe（quota>0）随后正常回写——
	// kProbe 的 DB 收敛即"quota flush 窗口已过"的正向屏障，此时 kZero 仍 0
	// 才证明"无回写"而非"尚未回写"（负断言不盲等）。
	_, kZero := userKeyQuota(t, env, uQ, g1, 0)
	_, kProbe := userKeyQuota(t, env, uQ, g1, 100000)
	waitSnapshot()
	chat(kZero, "e2e-model") // 放行（quota=0 不拦截）
	chat(kProbe, "e2e-model")
	pollQuotaUsed(t, env, kProbe, 500) // 屏障：flush 已把 kProbe 的 500 落库
	require.Equal(t, int64(0), env.quotaUsed(kZero), "quota=0 不产生 quota 回写（同窗口 kProbe 已回写）")

	// 额度**设为 0**（= 不限）→ 同步清零 quota_used（不是"只停累积"）：已用 1000 归零，
	// 且 key 立刻恢复放行（此前 429）。清零是同步写（同一 UPDATE），无异步窗口。
	qID := env.keyID(qKey)
	keyURL := env.aiURL(fmt.Sprintf("/api/user/keys/%d", qID))
	c, rbR := env.req(http.MethodPut, keyURL, "Bearer "+qToken, map[string]any{"quota": 0})
	require.Equal(t, 200, c, "quota→0: %s", rbR)
	require.Equal(t, int64(0), env.quotaUsed(qKey), "额度设为 0 → quota_used 清零")
	waitSnapshot()
	chat(qKey, "e2e-model") // 清零 + 不限 → 恢复放行（此前同 key 429）
	// 无额度 key 恒 0：迟到的增量回写被 "quota" > 0 守卫跳过（不等 flush 窗口）。
	require.Equal(t, int64(0), env.quotaUsed(qKey), "不限额度下不再累积")

	// 重新设额 → 从零起算（不被历史消耗挡住）：quota=500 恰好再放行一笔
	c, rbR = env.req(http.MethodPut, keyURL, "Bearer "+qToken, map[string]any{"quota": 500})
	require.Equal(t, 200, c, "quota→500: %s", rbR)
	waitSnapshot()
	chat(qKey, "e2e-model")
	pollQuotaUsed(t, env, qKey, 500) // 回写恢复生效（守卫只对 quota=0 生效）

	// ============ 场景 9：SIGTERM 优雅停机（流式中断 → 日志 cost 不丢） ============
	t.Log("场景 9：优雅停机——流式中断计费完整 flush")
	balBefore := env.balance(u1)
	// anthropic 长流（chunks 1000 × 5ms ≈ 5s > Shutdown 2s 优雅窗口：强断发生
	// 时流仍在途）：message_start 已带 pt=10，停机强断后按已累积 token 计费：
	// auto 档首中通配 ctx 变体 → 整单 10×5 = 50 毫分
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequest(http.MethodPost, env.aiURL("/v1/messages"), strings.NewReader(
			`{"model":"e2e-matrix-model","stream":true,"max_tokens":64,"chunks":1000,`+
				`"messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+u1Key)
		req.Header.Set("Content-Type", "application/json")
		_, _ = http.DefaultClient.Do(req) // 连接被强断 → 错误忽略（网关侧已计费）
	}()
	time.Sleep(800 * time.Millisecond) // 等 message_start 已消费、流仍在途

	gw.Stop(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("流式请求 goroutine 未退出")
	}
	// 流式中断日志：cost 完整（不丢计费）+ 余额扣减（排空落库）。停机后排空是
	// 一次性终态：轮询至余额收敛——数据缺失时 10s 超时带最后观测值失败，而非盲等。
	pollBalance(t, env, u1, balBefore-50)
	r = env.lastLogFor("e2e-matrix-model")
	require.Equal(t, int64(50), r.Cost, "优雅停机后流式中断日志 cost 不丢")
	require.Equal(t, "auto", r.Tier)
	require.False(t, r.AboveHit)
	require.Equal(t, balBefore-50, env.balance(u1), "优雅停机排空扣费完整")

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

	// ============ 场景 11：billing off——Key quota 链路全短路（独立配置/独立端口） ============
	// 主实例已停机；辅助实例同库同 Redis，仅 billing.enabled=false。quota=1 毫分
	// 的 key 两笔请求均 200（gate 不建条目、不检查、不扣减、无回写）。
	t.Log("场景 11：billing off——quota 不拒绝、不回写（独立进程验证）")
	gw2 := run.gateway(t, gatewaySpec{
		Name:   "billing-aux-billing-off",
		Config: func(in gatewayInputs) string { return billingAuxCfgTOML(in, false, true) },
	})
	env2 := run.env(gw2)
	uOff := createUser(t, env2, "quota-off@example.com", 0.0) // 余额 0：billing off 连余额预检一并短路
	_, offKey := userKeyQuota(t, env2, uOff, g1, 1)           // quota=1 毫分：门禁若生效第二笔必 429
	waitSnapshot()
	chatOn(t, env2, offKey, "e2e-model")
	chatOn(t, env2, offKey, "e2e-model") // 两笔 200（不因 quota=1 拒绝；Cost 亦不产生）
	// usage 明细正常落库（UsageCapture 语义不被破坏）→ 证明 Recorder 在跑
	pollUntil(t, "billing-off usage 明细落库", func() (bool, string) {
		n, err := env2.dbInt(`SELECT count(*) FROM usage_logs WHERE user_id=$1`, uOff)
		if err != nil {
			return false, err.Error()
		}
		return n == 2, fmt.Sprintf("usage_logs rows=%d want=2", n)
	})
	// 负断言屏障：quota_flush_interval=1s，明细已落库即已过 ≥1 个 flush 窗口，
	// 再等 2 个窗口（有界 settle）后 quota_used 仍 0 = 恒零回写。
	time.Sleep(2 * time.Second)
	require.Equal(t, int64(0), env2.quotaUsed(offKey), "billing off 不产生 quota 回写")
	require.Equal(t, int64(0), env2.balance(uOff), "billing off 不扣余额")
	gw2.Stop(t)

	// ============ 场景 12：billing on + UsageCapture=false——quota 独立回写 ============
	// 普通 usage 明细跳过（单写点 routeLog 不执行），Key quota 仍经 finish 的
	// DeductQuota→AddQuota 独立批量回写（两线解耦的端到端形态）。
	t.Log("场景 12：billing on + UsageCapture=false——quota 独立回写、明细跳过")
	gw3 := run.gateway(t, gatewaySpec{
		Name:   "billing-aux-usage-capture-off",
		Config: func(in gatewayInputs) string { return billingAuxCfgTOML(in, true, false) },
	})
	env3 := run.env(gw3)
	uCap := createUser(t, env3, "quota-cap@example.com", 10.0)
	_, capKey := userKeyQuota(t, env3, uCap, g1, 100000)
	waitSnapshot()
	chatOn(t, env3, capKey, "e2e-model")
	pollQuotaUsed(t, env3, capKey, 500) // quota 独立回写收敛（flush 1s 节奏）
	time.Sleep(1 * time.Second)         // 明细 flush（300ms）若有行早已落库——settle 后仍 0 = 恒跳过
	nCap, err := env3.dbInt(`SELECT count(*) FROM usage_logs WHERE user_id=$1`, uCap)
	require.NoError(t, err)
	require.Zero(t, nCap, "UsageCapture=false 普通 usage 明细不落库")
	gw3.Stop(t)
}

// ---- helpers ----
// putVariants manual 变体矩阵（断言 200）：service_tier 替换档 / ctx_min 分段 /
// multiplier 倍数小数（0..10，0=免费，存储换算万分），整体替换语义。
func putVariants(t *testing.T, env *e2eEnv, model string, variants []map[string]any) {
	t.Helper()
	c, rb := env.admin(http.MethodPut, "/prices/variants?model="+url.QueryEscape(model),
		map[string]any{"variants": variants})
	require.Equal(t, 200, c, "put variants %s: %s", model, rb)
}

// userKeyQuota 组内建带额度 key（quota = 累计最终计费金额上限，毫分；0 = 不限），
// 返回 (token, key 明文)。
func userKeyQuota(t *testing.T, env *e2eEnv, userID, groupID, quota int64) (string, string) {
	t.Helper()
	email, err := env.dbVal(`SELECT email FROM users WHERE id=$1`, userID)
	require.NoError(t, err)
	c, rb := env.req(http.MethodPost, env.aiURL("/api/user/auth/login"), "", map[string]any{
		"email": email, "password": "s3cret-pass",
	})
	require.Equal(t, 200, c, "login: %s", rb)
	token := jsonGet(t, rb, "token").(string)
	c, rb = env.req(http.MethodPost, env.aiURL("/api/user/keys"), "Bearer "+token, map[string]any{
		"name": "kq-" + email, "group_id": groupID, "quota": quota,
	})
	require.Equal(t, 200, c, "create quota key: %s", rb)
	return token, jsonGet(t, rb, "key").(string)
}

// chatOn 对指定 env 实例发 e2e-model 流式 chat（断言 200——辅助实例用；主实例
// 停机后闭包 chat 的 env 已失效）。
func chatOn(t *testing.T, env *e2eEnv, key, model string) {
	t.Helper()
	c, rb := env.aiReq(http.MethodPost, "/v1/chat/completions", key, map[string]any{
		"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	require.Equal(t, 200, c, "chat %s: %s", model, rb)
}

// keyID key 明文的 DB 主键（更新端点路径用）。
func (e *e2eEnv) keyID(keyRaw string) int64 {
	e.t.Helper()
	v, err := e.dbInt(`SELECT id FROM keys WHERE key_raw=$1`, keyRaw)
	require.NoError(e.t, err)
	return v
}

// quotaUsed key 的 DB quota_used（毫分）。
func (e *e2eEnv) quotaUsed(keyRaw string) int64 {
	e.t.Helper()
	v, err := e.dbInt(`SELECT quota_used FROM keys WHERE key_raw=$1`, keyRaw)
	require.NoError(e.t, err)
	return v
}

// pollQuotaUsed 有界轮询 key 的 quota_used 收敛到期望值（quota flush 异步批量
// 回写，禁止盲 sleep 断言正向收敛）。
func pollQuotaUsed(t *testing.T, env *e2eEnv, keyRaw string, want int64) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("key quota_used→%d", want), func() (bool, string) {
		var got int64
		err := env.pg.QueryRow(context.Background(), `SELECT quota_used FROM keys WHERE key_raw=$1`, keyRaw).Scan(&got)
		if err != nil {
			return false, err.Error()
		}
		return got == want, fmt.Sprintf("quota_used got=%d want=%d", got, want)
	})
}
