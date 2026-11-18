// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	// 注册 database/sql 的 pgx 驱动（本文件的探针库句柄走 database/sql）。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/db/migrate/catalog"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
	"github.com/is7qin/c3api/pkg/logx"
)

// 启动门四态（spec §2.5，自证 T5）：真库（pgtest 私有克隆）+ 真 HTTP。
//
//  ① db_head < RequiredVersion      ⇒ 进程存活、/healthz 200、/readyz 503、业务 503
//  ② db_contract > binary_contract  ⇒ 同上
//  ③ db_head > binary_head（contract 相同）⇒ /readyz 200、业务照常（k8s 回退靠它）
//  ④ 相等                           ⇒ 200
//
// ①② 直接驱动生产函数 serveNotReady（不可服务时的真实运行面）；③④ 用
// server.NewServer + 同一个 gate 读点（就绪路径的 HTTP 面）。两条路径共用同一份
// gate 判定，这正是「能不能服务」只有一个真相的意思。
//
// 等待一律用轮询到 deadline（禁 time.Sleep）：服务器起来有延迟，但断言的是
// 「最终可答」，不是「立刻可答」。

func TestStartupGateFourStatesPG(t *testing.T) {
	t.Run("db behind required version is not ready but stays alive", func(t *testing.T) {
		db := openProbeDB(t) // 空库：账本都没有 ⇒ db_head=0 < RequiredVersion
		gate := gateFromDB(t, db)
		require.False(t, gate.ready())

		baseURL, stop := startNotReadyServer(t, gate)
		defer stop()
		assertNotReady(t, baseURL, 0, 0)
	})

	t.Run("contract ahead is not ready", func(t *testing.T) {
		db := openProbeDB(t)
		applyBaselineMigration(t, db)
		insertLedger(t, db, 9, "future_contract", migrate.PhaseContract)

		gate := gateFromDB(t, db)
		require.False(t, gate.ready())

		baseURL, stop := startNotReadyServer(t, gate)
		defer stop()
		assertNotReady(t, baseURL, 9, 9)
	})

	t.Run("expand ahead serves with a warning", func(t *testing.T) {
		db := openProbeDB(t)
		applyBaselineMigration(t, db)
		insertLedger(t, db, 9, "future_expand", migrate.PhaseExpand)

		gate := gateFromDB(t, db)
		require.True(t, gate.ready(), "库比二进制新不是停服理由（expand-only 保证旧代码能跑）")
		require.Len(t, gate.decision.Warnings, 1)

		assertReady(t, gate, 9)
	})

	t.Run("equal watermarks serve", func(t *testing.T) {
		db := openProbeDB(t)
		applyBaselineMigration(t, db)

		gate := gateFromDB(t, db)
		require.True(t, gate.ready())
		require.Empty(t, gate.decision.Warnings)

		assertReady(t, gate, migrate.RequiredVersion)
	})
}

// openProbeDB 打开一个私有空库句柄（迁移由用例自己跑，保证四态的起点可控）。
func openProbeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openProbeDBWithDSN(t)
	return db
}

// openProbeDBWithDSN 同 openProbeDB，另给出 DSN（需要同库的第二个句柄/连接池时用）。
func openProbeDBWithDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := pgtest.CloneEmpty(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(context.Background()))
	return db, dsn
}

// gateFromDB 用生产同一段逻辑算启动门：Verify 的水位 + Decide 的判定。
func gateFromDB(t *testing.T, db *sql.DB) *schemaGate {
	t.Helper()
	verdict, err := migrate.Verify(context.Background(), db)
	require.NoError(t, err)
	return newSchemaGate(verdict)
}

func applyBaselineMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := migrate.Up(context.Background(), db, migrate.UpOptions{})
	require.NoError(t, err)
}

// insertLedger 往账本里塞一行「未来版本」（模拟更晚的二进制留下的事实）。
func insertLedger(t *testing.T, db *sql.DB, version int64, name, phase string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO public.`+catalog.ControlTable+` (version, name, checksum, phase, origin, applied_at, execution_ms)
		 VALUES ($1, $2, $3, $4, $5, now(), 0)`,
		version, name, []byte("not-in-this-binary"), phase, migrate.OriginExecuted)
	require.NoError(t, err)
}

// startNotReadyServer 驱动生产函数 serveNotReady（schema 不可服务时的真实运行面），
// 返回基址与停机函数。
func startNotReadyServer(t *testing.T, gate *schemaGate) (baseURL string, stop func()) {
	t.Helper()
	addr := freeLoopbackAddr(t)
	log, err := logx.New("error", "")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	in := &infraFace{
		cfg: &config.Config{
			Server: config.ServerConfig{Addr: addr, ReadHeaderTimeout: time.Second, ShutdownDrain: 10 * time.Millisecond},
			Proxy:  config.ProxyConfig{UpstreamTimeout: time.Second},
		},
		log:    log,
		schema: gate,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveNotReady(ctx, in)
	}()
	base := "http://" + addr
	waitForHTTPStatus(t, base+"/healthz", http.StatusOK)
	return base, func() {
		cancel()
		<-done
	}
}

// assertNotReady 断言不可服务态的完整契约：/healthz 恒 200（三键不变），/readyz
// 503 + `{schema:{…}}`，业务/写面一律 503（含 /api/user 与支付回调：库写不了时
// 收款回调也不该先"成功"再丢单）。
func assertNotReady(t *testing.T, baseURL string, wantDB, wantContract int64) {
	t.Helper()
	health, body := getJSON(t, baseURL+"/healthz")
	require.Equal(t, http.StatusOK, health)
	require.ElementsMatch(t, []string{"inflight", "goroutines", "heap"}, bodyKeys(body),
		"/healthz 的键集是既有契约，不因 schema 状态变化")

	ready, readyBody := getJSON(t, baseURL+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, ready)
	schema := schemaObject(t, readyBody)
	require.Equal(t, float64(wantDB), schema["db"])
	require.Equal(t, float64(wantContract), schema["db_contract"])
	require.Equal(t, float64(migrate.RequiredVersion), schema["binary"])
	require.Equal(t, migrate.ModeReadonly, schema["mode"])

	for _, path := range []string{"/api/admin/ping", "/api/user/keys", "/api/payment/notify", "/v1/chat/completions"} {
		status, _ := getJSON(t, baseURL+path)
		require.Equal(t, http.StatusServiceUnavailable, status, "业务/写面必须 503: %s", path)
	}
}

// assertReady 断言可服务态：/readyz 200，业务面不再被闸门拦住。
func assertReady(t *testing.T, gate *schemaGate, wantDB int64) {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := server.NewServer(server.Options{
		Ready:        gate.status,
		MaxInflight:  64,
		AdminHandler: ok,
		UserHandler:  ok,
		AIHandler:    ok,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	health, _ := getJSON(t, ts.URL+"/healthz")
	require.Equal(t, http.StatusOK, health)
	ready, readyBody := getJSON(t, ts.URL+"/readyz")
	require.Equal(t, http.StatusOK, ready)
	schema := schemaObject(t, readyBody)
	require.Equal(t, float64(wantDB), schema["db"])
	require.Equal(t, migrate.ModeServe, schema["mode"])

	// 就绪态下业务面**不因 schema 被拒**：/v1 由桩处理成 200，/api/admin 可能因
	// 缺鉴权 401——两者都不是闸门的 503。
	aiStatus, _ := getJSON(t, ts.URL+"/v1/chat/completions")
	require.Equal(t, http.StatusOK, aiStatus)
	adminStatus, _ := getJSON(t, ts.URL+"/api/admin/ping")
	require.NotEqual(t, http.StatusServiceUnavailable, adminStatus)
}

// schemaObject 取 /readyz 的 `schema` 子对象（形状即契约）。
func schemaObject(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["schema"]
	require.True(t, ok, "/readyz 必须带 schema 对象: %v", body)
	schema, ok := raw.(map[string]any)
	require.True(t, ok, "schema 必须是对象: %T", raw)
	return schema
}

// getJSON 发一次 GET 并解出 JSON 体（非 JSON 体解不出也只返回状态码）。
func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	status, body, err := tryGetJSON(url)
	require.NoError(t, err)
	return status, body
}

// tryGetJSON 是轮询用的宽容版本：连接还没起来不是断言失败，是"再等一轮"。
func tryGetJSON(url string) (int, map[string]any, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, nil
}

func bodyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// freeLoopbackAddr 取一个当前空闲的回环端口（先占后放；serveNotReady 自己 Listen）。
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// waitForHTTPStatus 轮询到 deadline（禁 time.Sleep：用 time.After 作为轮询节拍）。
func waitForHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := 0
	for {
		status, _, err := tryGetJSON(url)
		if err == nil {
			last = status
			if status == want {
				return
			}
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "http endpoint never reached the expected status",
				"%s: want %d, last %d", url, want, last)
		}
		<-time.After(10 * time.Millisecond)
	}
}
