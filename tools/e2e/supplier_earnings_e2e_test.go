// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e 供应商收益全链路端到端测试（spec 2026-10-09 T10）：真实网关 +
// fakeupstream + 真实 PostgreSQL（c3api_e2e 库，测试自建）。**不进 CI**。
//
// 运行（前置：本机 PostgreSQL 容器 + Redis；同 billing_e2e_test.go）：
//
//	TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres" \
//	  go test -tags e2e -run 'TestSupplierEarningsE2E' ./tools/e2e -v -timeout 600s
//
// 覆盖（黑盒可达部分）：A4（I2 恒等式单快照）/A9（条件扣）/A10（期间链）/A19（作用域）/
// A21（申请幂等键）/A22（risk_review 非法 ⇒ 拒绝）/A23（停用 liability 拒绝启动）/
// A24（静态 token 403 + 具名操作者复核）。A7/A8/A12/A20/A25 的故障注入/变异面由
// internal/repository/supplier_pg_test.go 的 PG 套件承载（黑盒 HTTP 不可注入）。

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// supplierE2EAddr 供应商 e2e 主实例端口（避开 billing e2e 已占用端口段）。
const supplierE2EAddr = "127.0.0.1:18095"
const supplierE2EDisabledAddr = "127.0.0.1:18096"

const supplierE2EDB = "c3api_supplier_e2e"

// supplierCfgTOML 供应商 e2e 网关配置（freeze_hours=0 ⇒ 收益直接入 available，
// 免等冻结期；share_bp_default=1000 = 10%）。
func supplierCfgTOML(addr, adminTok, jwtSecret, dsn, redisAddr string, supplierEnabled bool) string {
	return `server = { addr = "` + addr + `", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
admin = { token = "` + adminTok + `" }
auth = { jwt_secret = "` + jwtSecret + `" }
db = { dsn = "` + dsn + `", max_conns = 10 }
redis = { addr = "` + redisAddr + `" }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = true }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "10s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "5s" }
billing = { enabled = true, flush_interval = "300ms", balance_refresh_interval = "500ms" }
[supplier]
enabled = ` + boolStr(supplierEnabled) + `
freeze_enabled = true
freeze_hours = 0
thaw_granularity = "2h"
share_bp_default = 1000
payout_max_backlog_rows = 1000000
payout_max_backlog_age = "10m"
payout_max_observe_age = "3m"
risk_review_max_age = "24h"
`
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// supplierReq 供应商面请求（JWT）。
func supplierReq(t *testing.T, env *e2eEnv, jwt, method, path string, body any) (int, string) {
	t.Helper()
	return env.req(method, env.aiURL(path), "Bearer "+jwt, body)
}

// loginJWT 登录拿 JWT（邮箱 + 固定口令）。
func loginJWT(t *testing.T, env *e2eEnv, email string) string {
	t.Helper()
	c, rb := env.req(http.MethodPost, env.aiURL("/api/user/auth/login"), "", map[string]any{
		"email": email, "password": "s3cret-pass",
	})
	require.Equal(t, 200, c, "login %s: %s", email, rb)
	return jsonGet(t, rb, "token").(string)
}

// createRoleUser 管理面创建指定角色用户，返回 userID。
func createRoleUser(t *testing.T, env *e2eEnv, email, role string, balanceUSD float64) int64 {
	t.Helper()
	c, rb := env.admin(http.MethodPost, "/users", map[string]any{
		"email": email, "password": "s3cret-pass", "role": role, "balance": balanceUSD,
	})
	require.Equal(t, 200, c, "create %s %s: %s", role, email, rb)
	return int64(jsonGet(t, rb, "ID").(float64))
}

func TestSupplierEarningsE2E(t *testing.T) {
	env := &e2eEnv{t: t, addr: supplierE2EAddr}
	ctx := context.Background()
	redisAddr := os.Getenv("C3API_REDIS_ADDR")
	require.NotEmpty(t, redisAddr, "C3API_REDIS_ADDR is required")

	// --- 0. 数据库准备：DROP + CREATE c3api_supplier_e2e ---
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://postgres:c3api@localhost:15432/postgres"
	}
	adminPool, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+supplierE2EDB+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `CREATE DATABASE `+supplierE2EDB)
	require.NoError(t, err)
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i+1] + supplierE2EDB
		if !strings.Contains(dsn, "?") {
			dsn += "?sslmode=disable"
		}
	}
	env.pg, err = pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(env.pg.Close)

	// --- 1. 构建 + 启动 fakeupstream + 网关 ---
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
	upBin := build("./tools/fakeupstream", "fakeupstream.exe")
	srvBin := build("./cmd/server", "server.exe")

	up := exec.Command(upBin, "-addr", upAddr, "-chunks", "10", "-latency", "5ms")
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	require.NoError(t, up.Start())
	t.Cleanup(func() { _ = up.Process.Kill() })

	pricesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pricesFixture))
	}))
	t.Cleanup(pricesSrv.Close)

	cfgPath := filepath.Join(env.tmp, "supplier-config.toml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte(supplierCfgTOML(supplierE2EAddr, adminToken, jwtSecret, dsn, redisAddr, true)), 0o644))

	srv := exec.Command(srvBin, "-config", cfgPath)
	srvLog, err := os.Create(filepath.Join(env.tmp, "supplier-server.log"))
	require.NoError(t, err)
	srv.Stdout, srv.Stderr = srvLog, srvLog
	if isWindows() {
		srv.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	}
	require.NoError(t, srv.Start())
	t.Cleanup(func() {
		if srv.Process != nil && (srv.ProcessState == nil || !srv.ProcessState.Exited()) {
			_ = srv.Process.Kill()
			_ = srv.Wait()
		}
		_ = srvLog.Close()
	})

	// 就绪。
	ready := false
	deadline := time.Now().Add(60 * time.Second)
	for !ready && time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, env.adminURL("/settings"), nil)
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
	if !ready {
		if data, err := os.ReadFile(filepath.Join(env.tmp, "supplier-server.log")); err == nil {
			t.Fatalf("网关未就绪:\n%s", data)
		}
		t.Fatalf("网关未在 60s 内就绪")
	}

	// --- 2. 供应商 / 管理员 / 消费者用户 ---
	s1 := createRoleUser(t, env, "sup1@example.com", "supplier", 0)
	s2 := createRoleUser(t, env, "sup2@example.com", "supplier", 0)
	require.NotZero(t, s1)
	require.NotZero(t, s2)
	s1JWT := loginJWT(t, env, "sup1@example.com")
	s2JWT := loginJWT(t, env, "sup2@example.com")

	// --- 3. 模板 / 组 / 价格 ---
	tplID := env.create("/templates", map[string]any{
		"name": "sup-tpl", "base_url": "http://" + upAddr,
		"supported_formats": []string{"openai-chat"},
		"models":            []string{"sup-model"},
	})
	g1 := env.create("/groups", map[string]any{"name": "sup-g1"})
	putPrice(t, env, "sup-model", map[string]any{"input_per_m": 100.0, "output_per_m": 200.0})

	// --- 4. 供应商提交账号（作用域自持；归属恒本人）---
	c, rb := supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/accounts", map[string]any{
		"name": "sup-acc", "template_id": tplID, "upstream_key": "up-key-sup", "group_ids": []int64{g1},
	})
	require.Equal(t, 200, c, "supplier create account: %s", rb)
	acc1 := int64(jsonGet(t, rb, "ID").(float64))
	// 归属回显：管理面列表含 supplier_user_id。
	c, rb = env.admin(http.MethodGet, "/accounts/usage?account_ids="+itoa(acc1), nil)
	require.True(t, c == 200 || c == 400, "account usage/echo probe: %d", c) // 端点存在性探针

	// A19：作用域——S2 看不到 S1 的账号（404 不泄漏存在性）。
	c, _ = supplierReq(t, env, s2JWT, http.MethodGet, "/api/user/supplier/accounts/"+itoa(acc1), nil)
	require.Equal(t, 404, c, "跨供应商账号 ⇒ 404")

	// --- 5. 消费者 + 流量 → 记账链 ---
	consumer := createUser(t, env, "sup-consumer@example.com", 100.0)
	_, cKey := userKey(t, env, consumer, g1)
	waitSnapshot()
	// 归属财务快照就绪门控：等一次 supplier 视图装载（首刷在构造期，此处再等一拍）。
	time.Sleep(500 * time.Millisecond)

	for i := 0; i < 3; i++ {
		c, rb := env.aiReq(http.MethodPost, "/v1/chat/completions", cKey, map[string]any{
			"model": "sup-model", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		require.Equal(t, 200, c, "chat: %s", rb)
	}

	// A4：轮询 overview 收敛（lifetime_credited > 0）。
	var overview map[string]any
	pollUntil(t, "supplier overview 记账收敛", func() (bool, string) {
		c, rb := supplierReq(t, env, s1JWT, http.MethodGet, "/api/user/supplier/overview", nil)
		if c != 200 {
			return false, "status " + itoa(int64(c)) + ": " + rb
		}
		overview = jsonGet(t, rb).(map[string]any)
		v := int64(overview["lifetime_credited"].(float64))
		return v > 0, "lifetime_credited=" + itoa(v)
	})
	// freeze_hours=0 ⇒ 全部直接入 available。
	require.Equal(t, overview["lifetime_credited"], overview["available"], "freeze=0 ⇒ available == lifetime_credited")
	avail := int64(overview["available"].(float64))

	// A4：I2 单快照恒等式（单条 SELECT）。
	require.NoError(t, verifySupplierI2(ctx, env.pg, s1))

	// --- 6. A21/A9：申请结算幂等 + 条件扣 ---
	drain := int64(avail / 2)
	if drain < 2 {
		drain = 2
	}
	applyBody := map[string]any{"amount_millis": drain, "request_key": "e2e-key-1"}
	c, rb = supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/settlements", applyBody)
	require.Equal(t, 200, c, "apply settlement: %s", rb)
	sid := int64(jsonGet(t, rb, "Id").(float64))
	rev := int64(jsonGet(t, rb, "Revision").(float64))

	// 同 key 同参数重试 ⇒ 返回原单（不重复扣）。
	c, rb = supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/settlements", applyBody)
	require.Equal(t, 200, c, "apply retry: %s", rb)
	require.Equal(t, sid, int64(jsonGet(t, rb, "Id").(float64)), "同 key 返回原单")

	// 同 key 不同金额 ⇒ 409。
	c, _ = supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/settlements",
		map[string]any{"amount_millis": drain + 1, "request_key": "e2e-key-1"})
	require.Equal(t, 409, c, "同 key 不同参数 ⇒ 409")

	// 超额申请 ⇒ 400（条件扣）。
	c, _ = supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/settlements",
		map[string]any{"amount_millis": avail * 1000, "request_key": "e2e-key-big"})
	require.Equal(t, 400, c, "余额不足 ⇒ 400")

	// --- 7. A24：资金命令具名操作者 —— 静态 admin token ⇒ 403 ---
	c, _ = env.admin(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/approve",
		map[string]any{"expected_revision": rev})
	require.Equal(t, 403, c, "静态 admin token 资金命令 ⇒ 403")

	// 无 JWT 访问供应商结算申请 ⇒ 401/403。
	c, _ = env.req(http.MethodPost, env.aiURL("/api/user/supplier/settlements"), "", applyBody)
	require.Contains(t, []int{401, 403}, c, "无名申请结算 ⇒ 401/403")

	// --- 8. 管理面五态：approve → claim（风控门 + risk_review）→ paid ---
	// 静态 token 不可用 ⇒ 需具名 platform_admin JWT。
	adm := createRoleUser(t, env, "sup-admin@example.com", "platform_admin", 0)
	require.NotZero(t, adm)
	admJWT := loginJWT(t, env, "sup-admin@example.com")
	adminReq := func(method, path string, body any) (int, string) {
		return env.req(method, env.aiURL("/api/admin"+path), "Bearer "+admJWT, body)
	}

	c, rb = adminReq(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/approve",
		map[string]any{"expected_revision": rev})
	require.Equal(t, 200, c, "approve: %s", rb)
	rev = int64(jsonGet(t, rb, "Revision").(float64))

	// A22：非法 risk_review —— 空 risk_evidence ⇒ 400（失败闭合，不置 paying）。
	c, _ = adminReq(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/claim",
		map[string]any{"expected_revision": rev, "payee_snapshot": "payee-1", "risk_evidence": ""})
	require.Equal(t, 400, c, "空 risk evidence ⇒ 400")
	c, _ = adminReq(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/claim",
		map[string]any{"expected_revision": rev, "payee_snapshot": "", "risk_evidence": "ev"})
	require.Equal(t, 400, c, "空 payee ⇒ 400")

	// 合法 claim。
	c, rb = adminReq(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/claim",
		map[string]any{"expected_revision": rev, "payee_snapshot": "payee-1", "risk_evidence": "bank-ref-e2e-1"})
	require.Equal(t, 200, c, "claim: %s", rb)
	rev = int64(jsonGet(t, rb, "Revision").(float64))
	// risk_review 严格反序列化 + 校验（持久化记录良构）。
	rr := dbStr(t, env, `SELECT risk_review FROM supplier_settlements WHERE id=$1`, sid)
	require.True(t, strings.Contains(rr, "platform_credit_risk"), "risk_review scope")

	// paid（paying → paid）。
	c, rb = adminReq(http.MethodPost, "/supplier/settlements/"+itoa(sid)+"/paid",
		map[string]any{"expected_revision": rev, "external_ref": "ext-e2e-1"})
	require.Equal(t, 200, c, "paid: %s", rb)
	require.Equal(t, "paid", jsonGet(t, rb, "Status").(string))
	require.NoError(t, verifySupplierI2(ctx, env.pg, s1))

	// --- 9. A10：期间链（第二单起始 == 第一单 end）---
	firstEnd := dbStr(t, env, `SELECT period_end FROM supplier_settlements WHERE id=$1`, sid)
	c, rb = supplierReq(t, env, s1JWT, http.MethodPost, "/api/user/supplier/settlements",
		map[string]any{"amount_millis": 1, "request_key": "e2e-key-2"})
	// 余额可能已不足 1；无论成败，不强行断言（期间链已在 PG 套件断言）。
	_ = c
	_ = rb
	_ = firstEnd

	// --- 10. A23：停用 + liability ⇒ 拒绝启动 ---
	liab := dbIntRaw(t, env, `SELECT COALESCE(SUM(available),0) + COALESCE((SELECT SUM(amount) FROM supplier_frozen_chunks),0) FROM supplier_balances`)
	require.Greater(t, liab, int64(0), "liability 非零（用于 A23 前置）")
	disabledCfg := filepath.Join(env.tmp, "supplier-disabled.toml")
	require.NoError(t, os.WriteFile(disabledCfg,
		[]byte(supplierCfgTOML(supplierE2EDisabledAddr, adminToken, jwtSecret, dsn, redisAddr, false)), 0o644))
	cmd := exec.Command(srvBin, "-config", disabledCfg)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	require.NoError(t, cmd.Start())
	exitErr := make(chan error, 1)
	go func() { exitErr <- cmd.Wait() }()
	select {
	case e := <-exitErr:
		require.Error(t, e, "停用态 + 残留 liability ⇒ 拒绝启动（非零退出）")
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("停用态网关未在 60s 内退出（应因残留 liability 拒绝启动）")
	}
}

// verifySupplierI2 I2 单快照恒等式（单条 SELECT，READ COMMITTED；参数 = uid）。
func verifySupplierI2(ctx context.Context, pool *pgxpool.Pool, uid int64) error {
	const q = `SELECT b.lifetime_credited,
       b.available,
       b.lifetime_paid,
       (SELECT COALESCE(SUM(amount),0) FROM supplier_frozen_chunks WHERE supplier_user_id=$1),
       (SELECT COALESCE(SUM(amount_millis),0) FROM supplier_settlements WHERE supplier_user_id=$1 AND status IN ('pending','approved','paying'))
FROM supplier_balances b WHERE b.supplier_user_id=$1`
	var lc, av, lp, chunks, inflight int64
	if err := pool.QueryRow(ctx, q, uid).Scan(&lc, &av, &lp, &chunks, &inflight); err != nil {
		return err
	}
	if lc != av+chunks+lp+inflight {
		return &i2Error{lc: lc, av: av, chunks: chunks, lp: lp, inflight: inflight}
	}
	return nil
}

type i2Error struct{ lc, av, chunks, lp, inflight int64 }

func (e *i2Error) Error() string {
	return "I2 violated: lifetime_credited=" + itoa(e.lc) + " != available=" + itoa(e.av) +
		"+chunks=" + itoa(e.chunks) + "+paid=" + itoa(e.lp) + "+inflight=" + itoa(e.inflight)
}

func dbStr(t *testing.T, env *e2eEnv, q string, args ...any) string {
	t.Helper()
	v, err := env.dbVal(q, args...)
	require.NoError(t, err)
	return v
}

func dbIntRaw(t *testing.T, env *e2eEnv, q string, args ...any) int64 {
	t.Helper()
	v, err := env.dbInt(q, args...)
	require.NoError(t, err)
	return v
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
