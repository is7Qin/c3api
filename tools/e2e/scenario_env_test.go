// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// 本文件是 P0 拥有的「场景 HTTP 句柄 + 管理面 helper」共享层（oracle P0 review I2）：
// 这些符号原先定义在尚未迁移的 billing_e2e_test.go 里，导致 harness（harness_test.go 的
// e2eRun.env）与 P0 已迁移场景（request_patch/supplier/harness selftest）反向依赖 legacy
// 文件——P1 迁移 billing 时若按 spec §3.1 删旧 e2eEnv/helper，会被迫回头改 P0 代码。
// 因此把跨文件共用的最小面（e2eEnv + req/admin/aiURL 等 helper + 轮询/json 助手）上移
// 到本文件；legacy billing/routing/routing_multi 继续引用同一符号，迁移时只删自己文件内
// 的 boot/断言 helper，不再触碰 P0 代码。
//
// 边界：本文件只有场景侧 HTTP/SQL 断言 helper（无进程/端口/库建删/env 设置/监听），
// boundary gate 对非白名单文件直接扫描通过；进程与资源生命周期归 harness 独占。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// jwtSecret e2e 网关配置的固定 JWT secret（场景只把它渲染进配置，不参与鉴权实现）。
const jwtSecret = "e2e-jwt-secret-change-me"

// adminURL / aiURL 端点基址（按 env.addr——主实例或 harness 动态分配的网关地址）。
func (e *e2eEnv) adminURL(p string) string { return "http://" + e.addr + "/api/admin" + p }
func (e *e2eEnv) aiURL(p string) string    { return "http://" + e.addr + p }

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

// e2eEnv 场景侧网关句柄（管理面 helper 的兼容视图；资源/进程生命周期归 harness）：
//   - pg 本 run 唯一库 pool（SQL 断言）；
//   - tmp 本 run temp 目录（场景落配置/夹具）；
//   - addr 目标 gateway 地址（同 run 多 gateway 时场景各自持有一个句柄）；
//   - adminJWT bootstrap 后的 platform_admin JWT（env.admin 鉴权用）。
type e2eEnv struct {
	t        *testing.T
	pg       *pgxpool.Pool
	tmp      string
	addr     string
	adminJWT string
}

// admin 管理面请求：body nil = 无请求体；返回状态码 + 响应体。
func (e *e2eEnv) admin(method, path string, body any) (int, string) {
	e.t.Helper()
	return e.req(method, e.adminURL(path), "Bearer "+e.adminJWT, body)
}

// req 用户面请求（JWT）；key 为 AI 请求（/v1）鉴权 key 时走 ai。
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
	return e.req(method, e.aiURL(path), "Bearer "+key, body)
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

// ---- 轮询收敛助手（spec-f2-ledger-cursor）----
// 扣费两跳异步：usage flusher 落库（flush_interval=300ms）→ billing 扫游标
// 扣费（flush_interval=300ms），最坏 ≈750ms；固定 sleep 余量不足，负载下必
// flake。断言一律有界轮询：条件成立即返回，超时 FailNow 并附最后观测值。

const (
	pollTick    = 100 * time.Millisecond
	pollTimeout = 10 * time.Second
)

// pollUntil 有界轮询直到 observe 报告收敛；超时 FailNow（what 标注等待目标，
// last 为最后一次观测描述）。observe 须容忍行未落库（pgx.ErrNoRows 等）返回
// false 重试。
func pollUntil(t *testing.T, what string, observe func() (done bool, last string)) {
	t.Helper()
	tick := time.NewTicker(pollTick)
	defer tick.Stop()
	deadline := time.Now().Add(pollTimeout)
	for {
		done, last := observe()
		if done {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("轮询超时(%s) %s：%s", pollTimeout, what, last)
		}
		<-tick.C
	}
}

// waitSnapshot 等待去抖窗口 + 一次重载完成（管理面变更生效延迟 ≤200ms
// 窗口 + 一次重载时长——变更后的断言性请求须落在重载之后，等效旧实现同步
// invalidate 的"变更即生效"，只是生效点推迟到窗口到点）。
func waitSnapshot() { time.Sleep(300 * time.Millisecond) }

// putPrice manual 设价（断言 200）。统一价格表契约：PUT /prices/entry?model=
// 查询参数形态（模型 ID 含 "/"，禁路径参数）；token 档必填 mode。
func putPrice(t *testing.T, env *e2eEnv, model string, body map[string]any) {
	t.Helper()
	full := map[string]any{"mode": "token"}
	for k, v := range body {
		full[k] = v
	}
	c, rb := env.admin(http.MethodPut, "/prices/entry?model="+url.QueryEscape(model), full)
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
	c, rb := env.req(http.MethodPost, env.aiURL("/api/user/auth/login"), "", map[string]any{
		"email": email, "password": "s3cret-pass",
	})
	require.Equal(t, 200, c, "login: %s", rb)
	token := jsonGet(t, rb, "token").(string)
	c, rb = env.req(http.MethodPost, env.aiURL("/api/user/keys"), "Bearer "+token, map[string]any{
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
