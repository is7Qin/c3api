// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// TestE2EHarness 共享 harness 自测（spec §5 P0 第 4 项）：
//
//	SharedLifecycle   同 run 双 gateway 共享 DSN/Redis 且动态端口互异；Stop/Kill/
//	                  Restart 复用地址且业务恢复；ExpectExit；registry 内容断言；
//	                  shared 模式 StopService 必须返回错误；AV-B 每 run 唯一资源。
//	StaleReclaim      AV-A（见 harness_test.go：活 PID/死 PID/活跃连接三种回收判定）。
//	DedicatedServices 专用 compose 栈（app 网关跑在专用 PG/Redis 上）+ 服务 stop/start/
//	                  health/logs/端口不变。
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// selftestConfigTOML 自测网关配置（billing 关闭：本自测只验生命周期，不验计费；
// scheduler 1s 加速快照收敛；动态 addr/DSN/Redis 逻辑库来自 harness）。
func selftestConfigTOML(in gatewayInputs) string {
	return fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
redis = { addr = "%s", db = %d }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = false }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "1s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "1s" }
billing = { enabled = false, flush_interval = "300ms", balance_refresh_interval = "1s" }
`, in.Addr, jwtSecret, in.DSN, in.RedisAddr, in.RedisDB)
}

// selftestChat 发一条非流式 chat；**允许传输错误**（停机断言需要观测 connection
// refused，而 e2eEnv.aiReq 用 require.NoError 会直接失败）。
func selftestChat(env *e2eEnv, key, model string) (int, string, error) {
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, env.aiURL("/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(rb), nil
}

// selftestWaitChat 有界轮询直到 chat 200（路由/鉴权快照与重启恢复的唯一收敛信号）。
func selftestWaitChat(t *testing.T, env *e2eEnv, key, model, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var last string
	for {
		code, body, err := selftestChat(env, key, model)
		if err == nil && code == http.StatusOK {
			return
		}
		last = fmt.Sprintf("code=%d err=%v body=%.200s", code, err, body)
		if !time.Now().Before(deadline) {
			t.Fatalf("%s：30s 内 chat 未 200（最后一次 %s）", what, last)
		}
		<-tick.C
	}
}

// selftestBusiness 用管理面建模板/组/账号 + 用户 key，并等首条 chat 200。
// 返回 env（绑定主 gateway 的兼容视图）与 key。
func selftestBusiness(t *testing.T, run *e2eRun, gw *gateway, upstreamBaseURL, model string) (*e2eEnv, string) {
	t.Helper()
	env := run.env(gw)
	tpl := env.create("/templates", map[string]any{
		"name":              "selftest-tpl-" + model,
		"base_url":          upstreamBaseURL,
		"supported_formats": []string{"openai-chat"},
		"models":            []string{model},
	})
	grp := env.create("/groups", map[string]any{"name": "selftest-grp-" + model})
	_ = env.create("/accounts", map[string]any{
		"name":         "selftest-acc-" + model,
		"template_id":  tpl,
		"upstream_key": "selftest-up-key",
		"group_ids":    []int64{grp},
	})
	userID := createUser(t, env, "selftest-"+model+"@example.com", 100.0)
	_, key := userKey(t, env, userID, grp)
	selftestWaitChat(t, env, key, model, "首条 chat 收敛（路由/鉴权快照）")
	return env, key
}

// TestE2EHarness P0 harness 自测主入口（e2e tag；串行，不 t.Parallel）。
func TestE2EHarness(t *testing.T) {
	t.Run("SharedLifecycle", testHarnessSharedLifecycle)
	t.Run("StaleReclaim", testHarnessStaleReclaim)
	t.Run("DedicatedServices", testHarnessDedicatedServices)
}

// testHarnessSharedLifecycle：同 run 双 gateway + Stop/Kill/Restart + ExpectExit +
// registry 断言 + shared 所有权 + AV-B 唯一资源。
func testHarnessSharedLifecycle(t *testing.T) {
	run := newE2ERun(t, e2eRunOptions{})
	require.NotEmpty(t, run.services.PGAdminDSN(), "shared 模式 base PG DSN")
	require.NotEmpty(t, run.services.RedisAddr())

	// shared 模式无权 mutate 共享 base：Stop/Start 必须返回错误（spec §3.3）。
	require.Error(t, run.services.StopService(t, "db"), "shared 模式 StopService 必须返回错误")
	require.Error(t, run.services.StartService(t, "redis"), "shared 模式 StartService 必须返回错误")

	up := run.fakeUpstream(t, upstreamSpec{Name: "selftest-upstream", Flags: []string{"-chunks", "2", "-latency", "1ms"}})
	require.NotEmpty(t, up.Addr, "fakeupstream 地址由 harness 分配")

	// 生产语义顺序：先起主实例并完成 bootstrap（首个注册者 = platform_admin），
	// 之后加入第二个实例（其启动快照已能看到 admin 用户）。
	gw1 := run.gateway(t, gatewaySpec{Name: "selftest-gw1", Config: selftestConfigTOML})
	env1 := run.env(gw1)
	require.NotEmpty(t, env1.adminJWT, "bootstrapAdmin 取得 platform_admin JWT")
	gw2 := run.gateway(t, gatewaySpec{Name: "selftest-gw2", Config: selftestConfigTOML})
	require.NotEqual(t, gw1.Addr, gw2.Addr, "同 run 双 gateway 必须用不同动态端口")
	require.Equal(t, gw1.inputs.DSN, gw2.inputs.DSN, "双 gateway 共享同一 run DSN")
	require.Equal(t, gw1.inputs.RedisAddr, gw2.inputs.RedisAddr, "双 gateway 共享同一 Redis 地址")
	require.Equal(t, gw1.inputs.RedisDB, gw2.inputs.RedisDB, "双 gateway 共享同一 Redis 逻辑库")
	require.Equal(t, run.dsn, gw1.inputs.DSN, "gateway config 输入的 DSN = harness 每 run 唯一 DSN")
	require.Contains(t, gw1.inputs.DSN, run.dbName, "DSN 库名 = 本 run 唯一库")

	// registry 内容断言（状态机 + 资源 + 子进程登记）。
	rec := run.registryRecord(t)
	require.Equal(t, run.uuid, rec.UUID)
	require.Equal(t, os.Getpid(), rec.PID)
	require.Equal(t, stateReady, rec.State, "全部 gateway 就绪 => registry state=ready")
	require.Equal(t, run.dbName, rec.PlannedPGDB)
	require.Equal(t, run.dbName, rec.PGDB)
	require.Equal(t, run.redisDB, rec.AllocatedRedisDB)
	require.Equal(t, run.uuid, rec.LockUUID, "Redis 锁 owner = run uuid")
	require.False(t, rec.ManualOverride, "未设置 E2E_REDIS_DB 时 manual_override=false")
	require.NotEmpty(t, rec.JobName, "Job Object 名（Unix 为占位记录）")
	require.NotEmpty(t, rec.TmpDir)
	require.NotEmpty(t, rec.LogDir)
	require.GreaterOrEqual(t, len(rec.ChildPIDs), 3, "fakeupstream + 双 gateway 全部登记 child_pids")
	for _, child := range rec.ChildPIDs {
		require.NotZero(t, child.PID, "child_pids 必须带 PID")
	}

	env2 := run.env(gw2)
	require.Equal(t, env1.addr, gw1.Addr)
	require.NotEmpty(t, env2.adminJWT, "第二个 gateway bootstrap 幂等（已 bootstrap => 登录）")

	model := "harness-model"
	env1, key := selftestBusiness(t, run, gw1, "http://"+up.Addr, model)
	// gw2 与 gw1 同库同 Redis：同一 key 也能从 gw2 完整业务（共享 DSN/Redis 证据）。
	selftestWaitChat(t, env2, key, model, "gw2 使用同一 key 业务可用（共享 DSN/Redis）")

	// Stop：优雅停机（退出码 0）=> 业务不可用；Restart 复用原地址且业务恢复。
	addr1 := gw1.Addr
	gw1.Stop(t)
	require.True(t, gw1.Exited())
	require.Equal(t, 0, gw1.ExitCode(), "优雅退出码 0")
	if code, _, err := selftestChat(env1, key, model); err == nil && code == http.StatusOK {
		t.Fatalf("gateway 停机后仍返回 200（期望连接失败/非 200）：code=%d", code)
	}
	gw1.Restart(t)
	require.Equal(t, addr1, gw1.Addr, "Restart 必须复用原地址（不静默换端口）")
	require.False(t, gw1.Exited())
	selftestWaitChat(t, env1, key, model, "Restart 后业务恢复")

	// Kill：强杀 => Restart 复用原地址且业务恢复（同 run 另一 gateway 全程存活）。
	gw1.Kill(t)
	require.True(t, gw1.Exited())
	gw1.Restart(t)
	require.Equal(t, addr1, gw1.Addr)
	selftestWaitChat(t, env1, key, model, "Kill 后 Restart 业务恢复")
	selftestWaitChat(t, env2, key, model, "另一 gateway 全程不受影响（独立进程）")

	// registry child_pids 随进程启停更新：重启后登记的是新 PID（不多留旧 PID）。
	rec = run.registryRecord(t)
	livePIDs := map[int]bool{}
	for _, child := range rec.ChildPIDs {
		require.False(t, livePIDs[child.PID], "child_pids 不得重复登记同一 PID")
		livePIDs[child.PID] = true
	}
	require.True(t, livePIDs[gw1.child.rec.PID], "重启后的 gateway PID 已登记")
	require.True(t, livePIDs[gw2.child.rec.PID], "另一 gateway PID 仍在登记")

	// ExpectExit：迁移后启动的进程按预期非零退出（退出码 + 日志包含）。
	exitGW := run.gateway(t, gatewaySpec{
		Name: "selftest-bad-config",
		Config: func(in gatewayInputs) string {
			return "proxy = { max_infligh = 1 }"
		},
		ExpectExit: &exitExpectation{Code: 1, Contains: "max_infligh", Timeout: 60 * time.Second},
	})
	require.True(t, exitGW.Exited())
	require.Equal(t, 1, exitGW.ExitCode())

	// AV-B（每 run 唯一资源）：同一测试进程新建第二个 run，PG 库/Redis 逻辑库互异。
	run2 := newE2ERun(t, e2eRunOptions{})
	snap1 := run.resources()
	snap2 := run2.resources()
	require.NotEqual(t, snap1.PGDB, snap2.PGDB, "每 run 唯一 PG 库")
	require.NotEqual(t, snap1.RedisDB, snap2.RedisDB, "每 run 唯一 Redis 逻辑库")
	lock1, err := readRedisLock(snap1.RedisDB)
	require.NoError(t, err)
	require.NotNil(t, lock1)
	lock2, err := readRedisLock(snap2.RedisDB)
	require.NoError(t, err)
	require.NotNil(t, lock2)
	require.NotEqual(t, lock1.UUID, lock2.UUID, "Redis 锁 owner 互异")
}

// testHarnessDedicatedServices：自包含专用 compose 栈上的完整业务 + 服务 stop/start/
// health/logs + 端口不变（不触碰共享 base）。
func testHarnessDedicatedServices(t *testing.T) {
	if err := composePreflight(); err != nil {
		t.Skipf("专用栈需要容器编排 CLI（compose v2+）：%v", err)
	}
	run := newE2ERun(t, e2eRunOptions{DedicatedServices: true})
	require.Contains(t, run.services.PGAdminDSN(), "127.0.0.1:", "专用栈 PG 端口由 compose 随机发现")
	require.NotEmpty(t, run.services.RedisAddr())

	// 进程内上游（专用栈子测试不走真实 fakeupstream 也能覆盖业务面；:0 httptest）。
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"dedicated-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upSrv.Close)

	gw := run.gateway(t, gatewaySpec{Name: "selftest-dedicated", Config: selftestConfigTOML})
	rec := run.registryRecord(t)
	require.NotEmpty(t, rec.ComposeProject, "registry 记录专用栈 project")
	require.Equal(t, run.resources().ComposeProject, rec.ComposeProject)
	require.NotEmpty(t, rec.ComposeFile, "registry 记录 compose 文件")
	require.Equal(t, rec.ComposeProject, rec.ContainerLabels[composeProjectLabel], "registry 记录 container label")

	model := "dedicated-model"
	env, key := selftestBusiness(t, run, gw, upSrv.URL, model)

	redisAddrBefore := run.services.RedisAddr()
	pgAddrBefore := run.services.PGAdminDSN()
	// 停专用 redis => 启动 => 健康 => 端口不变 + 业务恢复。
	require.NoError(t, run.services.StopService(t, serviceRedis), "专用栈 stop redis")
	require.NoError(t, run.services.StartService(t, serviceRedis), "专用栈 start redis")
	require.Equal(t, redisAddrBefore, run.services.RedisAddr(), "stop/start 必须保留端口映射")
	run.services.WaitHealthy(t, serviceRedis)
	selftestWaitChat(t, env, key, model, "专用 redis 恢复后业务恢复")

	// 停专用 db => 启动 => 健康 => 端口不变。
	require.NoError(t, run.services.StopService(t, serviceDB), "专用栈 stop db")
	require.NoError(t, run.services.StartService(t, serviceDB), "专用栈 start db")
	require.Equal(t, pgAddrBefore, run.services.PGAdminDSN(), "stop/start 必须保留端口映射")
	run.services.WaitHealthy(t, serviceDB)

	logs := run.services.Logs(t)
	require.NotEmpty(t, logs, "专用栈 logs 可读（失败诊断/artifact 数据源）")
	require.Contains(t, logs, "redis", "logs 覆盖专用 redis 服务")
	require.Contains(t, logs, "postgres", "logs 覆盖专用 db 服务")
}
