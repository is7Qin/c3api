// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// Package e2e request_patch 端到端（P6）：真实网关二进制 + 真实 fakeupstream +
// 真实 PostgreSQL/Redis，覆盖 spec custom-request-patch §11.1/§11.2/§11.3/§11.7/
// §11.9/§11.16 的人工 e2e 行：
//
//	A. HTTP chat custom：配置面往返（占位符原样回显、非法配置 400）→ {api_key}
//	   展开 / Authorization 覆盖 / body set+delete（真实 fakeupstream 审计面）；
//	   全矩阵捕获上游（Host / UA / Accept / 两层头 Set+Del / 客户端头透传 /
//	   upstream_paths / body 嵌套 set、delete、set 胜）；配置变更后新配置生效
//	   （行为 + plan generation）；非 custom 模板对照；上游回显 key 时 err_logs
//	   与 server.log 脱敏而客户端 4xx 原文不脱敏（spec §8 明示裁决）。
//	B. Responses WS custom（尽力）：握手头 / Host / {api_key} / 格式层删网关默认
//	   beta 头；首帧 patch、后续仅 response.create 帧 patch、response.cancel 字节不变。
//	C. probe（account-model-test，P5 数据面）：真实管理面探测端点 → 捕获上游断言
//	   探测请求携带 patch 握手头（Authorization/{api_key}/Host/UA）；7 键映射按出站
//	   格式取层（chat / responses / responses-ws 各用自己键，responses-ws 不消费
//	   responses 层）；body set/delete 生效；images edits multipart 只头（体结构性
//	   跳过）。另含「真实脏库 → probe 残余失败 WARN」观测：SQL 直写 template_exts.
//	   request_patch 为运行期必然 sjson 残余失败的合法 JSON → 探测结论不变 + 固定
//	   文案限频 WARN 恰好 1 条（含 template_id/format，不含 path/value/body/key）。
//
// 运行（前置：PG :15432；Redis 缺省 127.0.0.1:16379，可经 C3API_REDIS_ADDR 覆盖）：
//
//	$env:TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres"
//	go test -tags e2e -run 'TestRequestPatchE2E' ./tools/e2e -v -timeout 600s
//
// 与 billing/routing 文件同包：复用 e2eEnv/bootstrapAdminJWT/admin/aiURL/pollUntil/
// createUser/userKey/jsonGet/isWindows/createNewProcessGroup 与 rtPortFree。
// 本文件新增符号一律 rp 前缀，禁与其他 e2e 文件重名。
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/pkg/redisx"
)

const (
	rpServerAddr = "127.0.0.1:18094" // billing 18090-18092 / routing 18093，本文件独立端口
	rpUpAddr     = "127.0.0.1:19112" // 真实 fakeupstream（routing 用 19111）
	rpDBName     = "c3api_request_patch_e2e"
	rpPageMax    = 200

	// 客户端请求模型（路由/定价按客户端模型；patch 在选号之后才作用于出站报文）。
	rpModelChat    = "rp-chat-model"
	rpModelWS      = "rp-ws-model"
	rpModelLeak    = "rp-leak-model"
	rpModelPatched = "rp-patched-model"
	rpModelPatchV2 = "rp-patched-model-v2"
	rpModelWSPatch = "rp-ws-patched-model"

	// rpClientCache 客户端带给上游的 prompt_cache_key（body delete 的正向证据）。
	rpClientCache = "rp-client-cache"

	// rpLeakAccountKey 脱敏场景的账号上游 key：上游把收到的 Authorization 回显进
	// 4xx 文本，日志面必须脱敏、客户端原文面不脱敏。
	rpLeakAccountKey = "rp-key-leak-e2e"

	// rpWSCancelFrame response.cancel 原始帧（只能逐字节原样到达上游）。
	rpWSCancelFrame = `{"type":"response.cancel","response_id":"resp_rp_cancel"}`

	// probe（account-model-test，P5）场景模型。probe 出站目标/凭据全部来自已存储
	// 配置，请求体只带 model/format；patch 在 probe 的既有载荷合成之后应用。
	rpModelProbeChat   = "rp-probe-chat-model"
	rpModelProbeResp   = "rp-probe-resp-model"
	rpModelProbeWS     = "rp-probe-ws-model"
	rpModelProbeImg    = "rp-probe-img-model"
	rpModelDirty       = "rp-dirty-model"
	rpProbeChatPatched = "rp-probe-chat-patched"
	rpProbeRespPatched = "rp-probe-resp-patched"

	// rpDirtyAccountKey 脏库 WARN 场景的账号上游 key（日志/上游体都不得泄漏它）。
	rpDirtyAccountKey = "rp-key-dirty-e2e"
	// rpDirtySecretMarker 脏 patch 里的 set 值哨兵：残余失败必须裸透传，绝不出现在
	// 上游 body / 日志里（value 泄漏负断言）。
	rpDirtySecretMarker = "must-not-leak-dirty-value"
	// rpDirtyPathMarker 脏 patch 里的 set path 哨兵（超 sjson 数组 padding 上限 ⇒
	// 运行期必然残余失败；path 不得出现在日志里）。
	rpDirtyPathMarker = "x.999999999"
	// rpProbeWarnMessage 残余失败固定文案（日志面契约；逐字断言，改文案=改契约）。
	rpProbeWarnMessage = "request patch body apply failed; forwarding unmodified body"
)

// ---- 进程内捕获上游 ----
//
// 真实 fakeupstream 的审计面只记 path/key/model/prompt_cache_key（见
// tools/fakeupstream/audit.go），观察不到 Host/UA/Accept/自定义头与 body 自定义
// 字段；request_patch 的核心断言恰恰是「上游收到的头/体逐项等于既有变换 + patch」。
// 因此增加一个**进程内真实 HTTP/WS 服务**（httptest，监听真实回环端口；网关是
// 独立进程、经 TCP 访问），逐字节记录收到的请求。两面各覆盖一半：真实 fakeupstream
// 证明与既有 e2e 假上游兼容；捕获上游证明全矩阵。

type rpHTTPReq struct {
	Seq      int64
	Path     string
	RawQuery string
	Host     string
	Header   http.Header
	Body     []byte
}

type rpWSConn struct {
	Seq    int64
	Path   string
	Host   string
	Header http.Header
	Frames [][]byte
}

type rpCapture struct {
	srv    *httptest.Server
	respID string
	imgB64 string // probe images edits 用的合法 PNG（data URL 与回包 b64_json 同一份）

	mu       sync.Mutex
	seq      int64
	httpReqs []rpHTTPReq
	wsConns  []rpWSConn
}

// rpSmallPNG 生成 1x1 PNG（probe images edits 的 image_url 与上游 b64_json 响应共用，
// 运行期生成保证是可解码的真实 PNG，免外部夹具）。
func rpSmallPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xFF, A: 0xFF})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("rpSmallPNG: " + err.Error()) // 1x1 编码不可能失败（编程错误）
	}
	return buf.Bytes()
}

// rpWriteBody 写 JSON 响应体。
func rpWriteBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func rpStartCapture(t *testing.T) *rpCapture {
	t.Helper()
	c := &rpCapture{
		respID: fmt.Sprintf("resp_rp_%d", time.Now().UnixNano()),
		imgB64: base64.StdEncoding.EncodeToString(rpSmallPNG()),
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *rpCapture) serve(w http.ResponseWriter, r *http.Request) {
	if rpIsWSUpgrade(r) {
		c.serveWS(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		body = nil
	}
	c.mu.Lock()
	c.seq++
	rec := rpHTTPReq{
		Seq: c.seq, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Host: r.Host,
		Header: r.Header.Clone(), Body: append([]byte(nil), body...),
	}
	c.httpReqs = append(c.httpReqs, rec)
	c.mu.Unlock()

	// X-RP-Echo: auth ⇒ 上游把收到的凭据回显进 4xx 文本（脱敏 e2e 的唯一泄漏源）。
	if r.Header.Get("X-RP-Echo") == "auth" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":"upstream rejected auth=%s"}}`, r.Header.Get("Authorization"))
		return
	}
	// probe（account-model-test）各格式的成功响应：路径由 custom 模板 upstream_paths
	// 决定（与 request_patch 格式层同一把格式键）。chat/responses 回真实成功判定形状，
	// images edits 回可解析的 b64 图片 ⇒ probe 结论 success=true，证明整条探测链路
	// （service→prober→aiclient 出站）真实可用，而不只是"上游收到了请求"。
	switch r.URL.Path {
	case "/v1/rp-probe-chat":
		rpWriteBody(w, `{"choices":[{"message":{"content":"probe-ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		return
	case "/v1/rp-probe-resp", "/v1/rp-probe-ws":
		rpWriteBody(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"probe-ok"}]}]}`)
		return
	case "/v1/rp-probe-img-edits":
		rpWriteBody(w, fmt.Sprintf(`{"data":[{"b64_json":"%s"}]}`, c.imgB64))
		return
	}
	if gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "text/event-stream")
		if fl, ok := w.(http.Flusher); ok {
			fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "upstream-model",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "x"},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
	})
}

// serveWS 上游 Responses WS：记录握手与客户端帧，读满 3 帧（首帧 + 第二个
// response.create + response.cancel）后回 response.completed 并正常关闭。
// 「读满 3 帧才回帧」= 客户端帧确实全部到达上游的确定性屏障（不用 sleep）。
func (c *rpCapture) serveWS(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.seq++
	idx := len(c.wsConns)
	c.wsConns = append(c.wsConns, rpWSConn{
		Seq: c.seq, Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone(),
	})
	c.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for range 3 {
		rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, msg, rerr := conn.Read(rctx)
		cancel()
		if rerr != nil {
			return
		}
		c.mu.Lock()
		c.wsConns[idx].Frames = append(c.wsConns[idx].Frames, append([]byte(nil), msg...))
		c.mu.Unlock()
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"%s","status":"completed","model":"%s","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`, c.respID, rpModelWS)
	_ = conn.Write(wctx, websocket.MessageText, []byte(completed))
	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (c *rpCapture) httpSnapshot() []rpHTTPReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rpHTTPReq(nil), c.httpReqs...)
}

// lastHTTP 最近一次捕获的 HTTP 上游请求（无记录 = FailNow）。
func (c *rpCapture) lastHTTP(t *testing.T) rpHTTPReq {
	t.Helper()
	reqs := c.httpSnapshot()
	require.NotEmpty(t, reqs, "捕获上游未收到任何 HTTP 请求")
	return reqs[len(reqs)-1]
}

// lastWS 最近一次捕获的 WS 上游连接（无记录 = FailNow）。
func (c *rpCapture) lastWS(t *testing.T) rpWSConn {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.wsConns, "捕获上游未收到任何 WS 握手")
	return c.wsConns[len(c.wsConns)-1]
}

// rpIsWSUpgrade 判定 WS 升级（Connection 含 Upgrade token 且 Upgrade 含 websocket
// token，大小写不敏感；与 RFC 6455 / proxy.isWebSocketUpgrade 同判据）。
func rpIsWSUpgrade(r *http.Request) bool {
	has := func(key, token string) bool {
		for _, v := range r.Header.Values(key) {
			for part := range strings.SplitSeq(v, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
		return false
	}
	return has("Connection", "Upgrade") && has("Upgrade", "websocket")
}

// ---- 真实 fakeupstream 审计面（/_audit 子集） ----

type rpUpAuditEntry struct {
	Seq      int64  `json:"seq"`
	Path     string `json:"path"`
	Key      string `json:"key"`
	Model    string `json:"model"`
	CacheKey string `json:"prompt_cache_key"`
	Status   int    `json:"status"`
}

func rpUpAudit(t *testing.T, addr string) []rpUpAuditEntry {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/_audit")
	require.NoError(t, err, "fakeupstream /_audit")
	defer resp.Body.Close()
	var out []rpUpAuditEntry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "fakeupstream /_audit 解码")
	return out
}

// rpUpAuditLast 最近一条指定 path 的审计记录。
func rpUpAuditLast(t *testing.T, addr, path string) rpUpAuditEntry {
	t.Helper()
	entries := rpUpAudit(t, addr)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Path == path {
			return entries[i]
		}
	}
	t.Fatalf("fakeupstream 无 path=%s 的审计记录（entries=%+v）", path, entries)
	return rpUpAuditEntry{}
}

// ---- 环境启动 ----

type rpHarness struct {
	env     *e2eEnv
	capture *rpCapture
	upAddr  string
	logPath string
}

// rpBoot 起独立 PG 库 + 真实 fakeupstream + 进程内捕获上游 + 真实网关二进制。
// 配置要点：
//   - scheduler.sync_interval=1s 加速快照收敛（断言一律经 /routing/plan 就绪门，
//     不靠 sleep）；
//   - billing.enabled=false：本文件验的是出站报文面，计费面已有 billing e2e；
//     关掉以免价格/余额快照把请求拦在 402，失败诊断只剩 patch 面；
//   - log.level=warn：成功路径应零 patch 日志（WARN 是异常信号）。
func rpBoot(t *testing.T) *rpHarness {
	t.Helper()
	rtPortFree(t, rpServerAddr) // 端口占用预检：僵尸进程直接 FailNow
	rtPortFree(t, rpUpAddr)

	ctx := context.Background()
	redisAddr := os.Getenv("C3API_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:16379" // deploy/test-compose 默认（brief 的命令只设 TEST_DATABASE_URL）
	}
	// Redis 隔离（与 PG DROP+CREATE 对等，routing e2e 同款）：清掉上轮残留的
	// 健康冷却/续接绑定等易失态，避免确定性重放的账号 ID 读到旧记录。
	rc, err := redisx.Open(redisx.Options{Addr: redisAddr})
	require.NoError(t, err, "redis open for e2e isolation flush")
	require.NoError(t, rc.FlushDB(ctx).Err(), "e2e redis isolation flush")
	_ = rc.Close()

	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://postgres:c3api@localhost:15432/postgres"
	}
	adminPool, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+rpDBName+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, `CREATE DATABASE `+rpDBName)
	require.NoError(t, err)
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i+1] + rpDBName
		if !strings.Contains(dsn, "?") {
			dsn += "?sslmode=disable"
		}
	}
	pg, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)

	env := &e2eEnv{t: t, pg: pg, addr: rpServerAddr}
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
	upBin := build("./tools/fakeupstream", "rp-fakeupstream.exe")
	srvBin := build("./cmd/server", "rp-server.exe")

	up := exec.Command(upBin, "-addr", rpUpAddr, "-chunks", "3", "-latency", "1ms")
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	require.NoError(t, up.Start())
	t.Cleanup(func() {
		if up.Process != nil {
			_ = up.Process.Kill()
		}
	})

	capture := rpStartCapture(t)

	cfg := fmt.Sprintf(`server = { addr = "%s", read_header_timeout = "10s", max_header_bytes = 1048576 }
log = { level = "warn", output = "stdout" }
auth = { jwt_secret = "%s" }
db = { dsn = "%s", max_conns = 10 }
redis = { addr = "%s" }
proxy = { max_body_size = 4194304, max_inflight = 50000, upstream_timeout = "120s", upstream_stream_timeout = "30m", failover_attempts = 2, usage_capture = true }
upstream = { max_idle_conns = 64, max_idle_conns_per_host = 16, idle_conn_timeout = "90s", dial_timeout = "10s", force_http2 = false }
scheduler = { default_max_concurrency = 8, sync_interval = "1s" }
usage = { batch_size = 500, flush_interval = "300ms", log_retention_days = 2, quota_flush_interval = "1s" }
billing = { enabled = false, flush_interval = "300ms", balance_refresh_interval = "1s" }
`, rpServerAddr, jwtSecret, dsn, redisAddr)
	cfgPath := filepath.Join(env.tmp, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	// 迁移：app 进程永不建表（migrate CLI spec §2.10/§2.14），schema 只归显式
	// `-migrate up`。既有 e2e harness（billing/routing）没跑这一步——迁移 refactor
	// 后新鲜库会被 schema gate 摘成只读（本文件用真实库时实测）。在 boot 内显式
	// up，避免测试依赖"库恰好已被迁过"这种环境态。
	migCmd := exec.Command(srvBin, "-config", cfgPath, "-migrate", "up")
	migOut, migErr := migCmd.CombinedOutput()
	require.NoError(t, migErr, "schema migrate up: %s", migOut)

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
			t.Logf("--- rp server.log (test failed) ---\n%s", data)
		}
	})

	env.adminJWT = env.bootstrapAdminJWT() // /healthz 就绪 → bootstrap platform_admin = 就绪
	return &rpHarness{env: env, capture: capture, upAddr: rpUpAddr, logPath: filepath.Join(env.tmp, "server.log")}
}

// ---- 通用助手 ----

// rpAIReq /v1 AI 请求（Bearer key 鉴权；extra 为额外客户端头，验证透传/删除）。
func rpAIReq(t *testing.T, env *e2eEnv, key, path string, body map[string]any, extra http.Header) (int, string) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, env.aiURL(path), bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// rpCreateCustomTemplate 建 custom 模板（base_url 指向真实上游）。
func rpCreateCustomTemplate(t *testing.T, env *e2eEnv, name, baseURL string, formats, models []string) int64 {
	t.Helper()
	return env.create("/templates", map[string]any{
		"name": name, "base_url": baseURL, "credential_type": "custom",
		"supported_formats": formats, "models": models,
	})
}

// rpCreateAccount 建静态 key 账号并挂到组。
func rpCreateAccount(t *testing.T, env *e2eEnv, name string, tplID int64, upstreamKey string, groupID int64) int64 {
	t.Helper()
	return env.create("/accounts", map[string]any{
		"name": name, "template_id": tplID, "upstream_key": upstreamKey,
		"group_ids": []int64{groupID},
	})
}

// rpExtBody 模板 ext 写面体（request_patch / upstream_paths 全列替换）。
func rpExtBody(credentialType string, paths map[string]string, patch map[string]any) map[string]any {
	body := map[string]any{"credential_type": credentialType}
	if paths != nil {
		body["upstream_paths"] = paths
	}
	if patch != nil {
		body["request_patch"] = patch
	}
	return body
}

// rpPutExt 写模板 ext；返回状态码 + 响应体。
func rpPutExt(t *testing.T, env *e2eEnv, tplID int64, body map[string]any) (int, string) {
	t.Helper()
	return env.admin(http.MethodPut, fmt.Sprintf("/templates/%d/ext", tplID), body)
}

// rpGetExt 读模板 ext（配置面回显）。
func rpGetExt(t *testing.T, env *e2eEnv, tplID int64) string {
	t.Helper()
	c, rb := env.admin(http.MethodGet, fmt.Sprintf("/templates/%d/ext", tplID), nil)
	require.Equal(t, 200, c, "get template ext: %s", rb)
	return rb
}

// rpPlan 当前发布计划（generation + 路由页；本测试路由数远小于单页上限）。
func rpPlan(t *testing.T, env *e2eEnv) (int64, []any) {
	t.Helper()
	c, rb := env.admin(http.MethodGet, fmt.Sprintf("/routing/plan?limit=%d", rpPageMax), nil)
	require.Equal(t, 200, c, "get routing plan: %s", rb)
	root, ok := jsonGet(t, rb, "").(map[string]any)
	require.True(t, ok, "plan 顶层必须是对象: %s", rb)
	gen, _ := root["generation"].(float64)
	routes, _ := root["routes"].([]any)
	return int64(gen), routes
}

// rpWaitRoute 有界轮询直到 (group, format, model) 路由出现且候选非空（快照/编译
// 异步收敛；禁裸 sleep）。
func rpWaitRoute(t *testing.T, env *e2eEnv, groupID int64, format, model, what string) map[string]any {
	t.Helper()
	var found map[string]any
	pollUntil(t, what, func() (bool, string) {
		gen, routes := rpPlan(t, env)
		found = nil
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			ref, _ := route["ref"].(map[string]any)
			if ref == nil {
				continue
			}
			gid, _ := ref["group_id"].(float64)
			if int64(gid) != groupID || ref["format"] != format || ref["model"] != model {
				continue
			}
			found = route
			if cands, _ := route["candidates"].([]any); len(cands) > 0 {
				return true, ""
			}
			return false, fmt.Sprintf("generation=%d 路由存在但候选为空", gen)
		}
		return false, fmt.Sprintf("generation=%d 路由 (g=%d %s %s) 未出现", gen, groupID, format, model)
	})
	return found
}

// rpChatBody 非流式 chat 请求体（带客户端标记字段：body patch/透传断言用）。
func rpChatBody(model string) map[string]any {
	return map[string]any{
		"model":               model,
		"stream":              false,
		"messages":            []any{map[string]any{"role": "user", "content": "hi"}},
		"prompt_cache_key":    rpClientCache,
		"presence_penalty":    0.5,
		"temperature":         0.9,
		"custom_client_field": "keep",
	}
}

// rpChatUntil 有界轮询发非流式 chat，直到 accept 报告收敛；返回最后一次 (code, body)。
//
// 用途：管理面写请求（ext/账号/key）到调度快照/鉴权快照的生效点是异步的（invalidate
// 去抖 + 编译道），而"路由已存在"这种结构就绪门**不覆盖配置内容变更**（A2 实测：
// 路由早已存在，ext 写入后立刻发请求仍走旧快照）。这里以"上游可观测行为"为唯一
// 收敛信号，禁 sleep、也不把 plan generation 的时序巧合当屏障。
func rpChatUntil(t *testing.T, env *e2eEnv, key, model string, extra http.Header, what string,
	accept func(code int, body string) bool) (int, string) {
	t.Helper()
	var lastCode int
	var lastBody string
	pollUntil(t, what, func() (bool, string) {
		lastCode, lastBody = rpAIReq(t, env, key, "/v1/chat/completions", rpChatBody(model), extra)
		return accept(lastCode, lastBody), fmt.Sprintf("code=%d body=%.300s", lastCode, lastBody)
	})
	return lastCode, lastBody
}

// rpWaitChatOK 有界轮询直到 chat 200（鉴权/路由快照收敛屏障），返回响应体。
func rpWaitChatOK(t *testing.T, env *e2eEnv, key, model, what string) string {
	t.Helper()
	_, body := rpChatUntil(t, env, key, model, nil, what, func(code int, _ string) bool { return code == 200 })
	return body
}

// rpLogFile 服务日志全文（成功路径应无 patch 内容；脱敏负向断言的数据面）。
func rpLogFile(t *testing.T, h *rpHarness) string {
	t.Helper()
	data, err := os.ReadFile(h.logPath)
	require.NoError(t, err, "读 server.log")
	return string(data)
}

// rpProbe 管理面单账号单模型探测（account-model-test）：POST /accounts/{id}/test。
// 200 恒为探测结论（success=false 也是 200）；只读、不落库、不计费。
func rpProbe(t *testing.T, env *e2eEnv, accountID int64, body map[string]any) (int, string) {
	t.Helper()
	return env.admin(http.MethodPost, fmt.Sprintf("/accounts/%d/test", accountID), body)
}

// rpRequireProbeSuccess 断言探测结论为「上游 200 且可用输出」。
func rpRequireProbeSuccess(t *testing.T, code int, body, what string) {
	t.Helper()
	require.Equal(t, 200, code, "%s：探测端点必须 200（结论在体内）: %s", what, body)
	require.True(t, gjson.Get(body, "success").Bool(), "%s：探测结论必须 success=true: %s", what, body)
	require.Equal(t, 200, int(gjson.Get(body, "status_code").Int()), "%s：上游状态 200: %s", what, body)
}

// rpServerLogJSONLines 服务日志逐行 JSON 解码（zap JSON 行；非 JSON 行跳过——
// 只用于观测面断言，不把启动期任何非结构化输出当失败）。
func rpServerLogJSONLines(t *testing.T, h *rpHarness) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(rpLogFile(t, h), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// rpDialGatewayWS 客户端拨网关 /v1/responses（Authorization: Bearer <用户 key>）。
func rpDialGatewayWS(t *testing.T, env *e2eEnv, key string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+env.addr+"/v1/responses", &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + key}},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	require.NoError(t, err, "网关 /v1/responses WS 升级必须成功")
	return conn
}

// rpDrainWS 读到连接关闭，返回收到的全部帧（正常关闭 = nil error）。
func rpDrainWS(t *testing.T, conn *websocket.Conn, timeout time.Duration) ([][]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out [][]byte
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return out, nil
			}
			return out, err
		}
		out = append(out, append([]byte(nil), msg...))
	}
}

// TestRequestPatchE2E request_patch 端到端主场景（A：HTTP chat；B：Responses WS）。
//
// 全部路由就绪门走 /routing/plan（后台编译异步），全程无 sleep；每组 = 独立
// 模板/账号/用户 key（选号确定，避免同组多模板的随机候选）。
func TestRequestPatchE2E(t *testing.T) {
	h := rpBoot(t)
	env := h.env
	capture := h.capture
	captureURL := capture.srv.URL
	captureHost := strings.TrimPrefix(captureURL, "http://")

	// ---- 0. 基建：用户 + 5 组/模板/账号/key ----
	userID := createUser(t, env, "rp-user@example.com", 0)

	formatsChat := []string{"openai-chat"}
	formatsWS := []string{"openai-chat", "openai-responses-ws"}
	modelsChat := []string{rpModelChat}
	modelsLeak := []string{rpModelLeak}
	modelsWS := []string{rpModelChat, rpModelWS}

	tplReal := rpCreateCustomTemplate(t, env, "rp-tpl-real", "http://"+h.upAddr, formatsChat, modelsChat)
	tplCap := rpCreateCustomTemplate(t, env, "rp-tpl-cap", captureURL, formatsChat, modelsChat)
	tplCtrl := env.create("/templates", map[string]any{
		"name": "rp-tpl-ctrl", "base_url": captureURL,
		"supported_formats": formatsChat, "models": modelsChat,
	})
	tplLeak := rpCreateCustomTemplate(t, env, "rp-tpl-leak", captureURL, formatsChat, modelsLeak)
	tplWS := rpCreateCustomTemplate(t, env, "rp-tpl-ws", captureURL, formatsWS, modelsWS)

	gReal := env.create("/groups", map[string]any{"name": "rp-g-real"})
	gCap := env.create("/groups", map[string]any{"name": "rp-g-cap"})
	gCtrl := env.create("/groups", map[string]any{"name": "rp-g-ctrl"})
	gLeak := env.create("/groups", map[string]any{"name": "rp-g-leak"})
	gWS := env.create("/groups", map[string]any{"name": "rp-g-ws"})

	rpCreateAccount(t, env, "rp-acc-real", tplReal, "rp-key-real", gReal)
	rpCreateAccount(t, env, "rp-acc-cap", tplCap, "rp-key-cap", gCap)
	rpCreateAccount(t, env, "rp-acc-ctrl", tplCtrl, "rp-key-ctrl", gCtrl)
	rpCreateAccount(t, env, "rp-acc-leak", tplLeak, rpLeakAccountKey, gLeak)
	rpCreateAccount(t, env, "rp-acc-ws", tplWS, "rp-key-ws", gWS)

	_, kReal := userKey(t, env, userID, gReal)
	_, kCap := userKey(t, env, userID, gCap)
	_, kCtrl := userKey(t, env, userID, gCtrl)
	_, kLeak := userKey(t, env, userID, gLeak)
	_, kWS := userKey(t, env, userID, gWS)
	t.Logf("基建：templates real=%d cap=%d ctrl=%d leak=%d ws=%d groups real=%d cap=%d ctrl=%d leak=%d ws=%d",
		tplReal, tplCap, tplCtrl, tplLeak, tplWS, gReal, gCap, gCtrl, gLeak, gWS)

	// ---- A1 配置面（spec §11.1）----
	t.Log("A1 配置面：custom ext 写入/回显（占位符原样）、非法/非 custom 配置 400 且不落库")
	patchCap := map[string]any{
		"headers": map[string]any{
			"Authorization":   "E2E {api_key}",
			"Host":            "patched.example.invalid",
			"User-Agent":      "rp-e2e-ua/1.0",
			"Accept":          "application/vnd.rp+json",
			"X-RP-Global":     "g1",
			"X-RP-Layer":      "global",
			"X-RP-Drop":       "",
			"X-RP-Global-Del": "global-set-then-deleted",
		},
		"formats": map[string]any{
			"openai-chat": map[string]any{
				"headers": map[string]any{
					"X-RP-Layer":      "format",
					"X-RP-Format":     "f1",
					"X-RP-Global-Del": "",
				},
				"set": map[string]any{
					"model":                rpModelPatched,
					"metadata.call_source": "rp-e2e",
					"temperature":          0.25,
				},
				"delete": []string{"prompt_cache_key", "presence_penalty", "temperature"},
			},
		},
	}
	c, rb := rpPutExt(t, env, tplCap, rpExtBody("custom", map[string]string{"openai-chat": "v1/rp-chat"}, patchCap))
	require.Equal(t, 200, c, "写 custom ext（upstream_paths + request_patch）: %s", rb)
	extBody := rpGetExt(t, env, tplCap)
	require.Equal(t, "E2E {api_key}", gjson.Get(extBody, "request_patch.headers.Authorization").String(),
		"GET 原样回显占位符（不展开、不掩码）")
	require.Equal(t, "patched.example.invalid", gjson.Get(extBody, "request_patch.headers.Host").String())
	require.Equal(t, rpModelPatched, gjson.Get(extBody, "request_patch.formats.openai-chat.set.model").String())
	require.Equal(t, "v1/rp-chat", gjson.Get(extBody, "upstream_paths.openai-chat").String())
	require.NotContains(t, extBody, "rp-key-cap", "配置回显不得含运行期账号 key")

	// 非法请求修饰：保存期 400（wire 校验在 domain.ParseRequestPatch 唯一入口）。
	invalidPatches := []struct {
		name  string
		patch map[string]any
	}{
		{"未知 format 键", map[string]any{"formats": map[string]any{
			"openai-image": map[string]any{"set": map[string]any{"model": "x"}},
		}}},
		{"Accept-Encoding 非空", map[string]any{"headers": map[string]any{"accept-encoding": "gzip"}}},
		{"header 值含控制字符", map[string]any{"headers": map[string]any{"X-Bad": "a\nb"}}},
	}
	for _, tc := range invalidPatches {
		code, body := rpPutExt(t, env, tplCap, rpExtBody("custom", nil, tc.patch))
		require.Equal(t, 400, code, "%s 必须保存期 400: %s", tc.name, body)
	}
	// 非 custom（api_key）模板不可挂 ext / 非空 request_patch ⇒ 400。
	code, body := rpPutExt(t, env, tplCtrl, rpExtBody("custom", nil, map[string]any{
		"headers": map[string]any{"X-A": "b"},
	}))
	require.Equal(t, 400, code, "非 custom 模板提交 request_patch 必须 400: %s", body)
	// 400 先于写：上方失败写不得清空 A1 的成功配置。
	extBody = rpGetExt(t, env, tplCap)
	require.Equal(t, "E2E {api_key}", gjson.Get(extBody, "request_patch.headers.Authorization").String(),
		"失败的 PUT 不得落库/不得清空已有配置")
	require.Equal(t, "v1/rp-chat", gjson.Get(extBody, "upstream_paths.openai-chat").String())

	// ---- A2 真实 fakeupstream：Authorization 覆盖 + {api_key} 展开 + body set/delete ----
	t.Log("A2 真实 fakeupstream：Authorization 覆盖 + {api_key} 展开为账号 key + body set/delete")
	code, body = rpPutExt(t, env, tplReal, rpExtBody("custom", nil, map[string]any{
		"headers": map[string]any{"Authorization": "Token {api_key}"},
		"formats": map[string]any{"openai-chat": map[string]any{
			"set":    map[string]any{"model": rpModelPatched},
			"delete": []string{"prompt_cache_key"},
		}},
	}))
	require.Equal(t, 200, code, "写 fakeupstream 模板 ext: %s", body)
	rpWaitRoute(t, env, gReal, "openai-chat", rpModelChat, "A2 路由就绪")
	// 行为收敛门：ext 写入的生效点是异步快照重载（"路由已存在"不覆盖配置内容变更）。
	code, body = rpChatUntil(t, env, kReal, rpModelChat, nil, "A2 patch 生效（{api_key} 覆盖 Authorization）",
		func(code int, _ string) bool {
			if code != 200 {
				return false
			}
			return rpUpAuditLast(t, h.upAddr, "/v1/chat/completions").Key == "Token rp-key-real"
		})
	require.Equal(t, 200, code, "A2 chat 必须 200: %s", body)
	entry := rpUpAuditLast(t, h.upAddr, "/v1/chat/completions")
	require.Equal(t, "Token rp-key-real", entry.Key,
		"Authorization 被 patch 覆盖（scheme 变为 Token）且 {api_key} 展开为账号 key")
	require.Equal(t, rpModelPatched, entry.Model, "body set 生效（出站 model 被 patch 改写）")
	require.Empty(t, entry.CacheKey, "body delete 生效（客户端 prompt_cache_key 被删）")
	require.Equal(t, 200, entry.Status)
	t.Logf("A2 上游审计：key=%q model=%q prompt_cache_key=%q status=%d",
		entry.Key, entry.Model, entry.CacheKey, entry.Status)

	// ---- A3 全矩阵捕获：头面 + body 面（spec §11.2/§11.3）----
	t.Log("A3 全矩阵捕获上游：Host/UA/Accept/两层头 Set+Del/客户端头透传/upstream_paths/body 嵌套 set、delete、set 胜")
	rpWaitRoute(t, env, gCap, "openai-chat", rpModelChat, "A3 路由就绪")
	clientHeaders := http.Header{
		"X-RP-Drop":     {"client-value"},
		"X-Client-Keep": {"kept"},
	}
	code, body = rpChatUntil(t, env, kCap, rpModelChat, clientHeaders, "A3 patch 生效（捕获 X-RP-Global=g1）",
		func(code int, _ string) bool {
			if code != 200 {
				return false
			}
			return capture.lastHTTP(t).Header.Get("X-RP-Global") == "g1"
		})
	require.Equal(t, 200, code, "A3 chat 必须 200: %s", body)
	rec := capture.lastHTTP(t)

	require.Equal(t, "/v1/rp-chat", rec.Path, "upstream_paths 生效（自定义路径原样拼接）")
	require.Equal(t, "patched.example.invalid", rec.Host, "Host 覆盖走 req.Host（Header map 里的 Host 会被 net/http 忽略）")
	require.Equal(t, "E2E rp-key-cap", rec.Header.Get("Authorization"), "Authorization 覆盖 + {api_key} 展开")
	require.Equal(t, "rp-e2e-ua/1.0", rec.Header.Get("User-Agent"), "UA 覆盖")
	require.Equal(t, "application/vnd.rp+json", rec.Header.Get("Accept"), "Accept 覆盖网关默认值")
	require.Equal(t, "g1", rec.Header.Get("X-RP-Global"), "全局层 Set")
	require.Equal(t, "format", rec.Header.Get("X-RP-Layer"), "格式层逐键覆盖全局层同名头")
	require.Equal(t, "f1", rec.Header.Get("X-RP-Format"), "格式层 Set")
	require.Empty(t, rec.Header.Get("X-RP-Drop"), "全局层空值 = Del（删除客户端带的头）")
	require.Empty(t, rec.Header.Get("X-RP-Global-Del"), "格式层 Del 覆盖全局层 Set（三态）")
	require.Equal(t, "kept", rec.Header.Get("X-Client-Keep"), "客户端头 default-allow 透传不受 patch 影响")

	upBody := rec.Body
	require.Equal(t, rpModelPatched, gjson.GetBytes(upBody, "model").String(), "body set（model）")
	require.Equal(t, "rp-e2e", gjson.GetBytes(upBody, "metadata.call_source").String(), "body set 嵌套 path（自动建中间对象）")
	require.Equal(t, 0.25, gjson.GetBytes(upBody, "temperature").Float(), "同 path 同时 delete+set：set 胜")
	require.False(t, gjson.GetBytes(upBody, "prompt_cache_key").Exists(), "body delete（prompt_cache_key）")
	require.False(t, gjson.GetBytes(upBody, "presence_penalty").Exists(), "body delete（presence_penalty）")
	require.Equal(t, "keep", gjson.GetBytes(upBody, "custom_client_field").String(), "客户端 body 其它字段不变")
	require.Equal(t, "hi", gjson.GetBytes(upBody, "messages.0.content").String(), "messages 原样")
	t.Logf("A3 上游捕获：path=%s host=%s auth=%q ua=%q accept=%q x-rp-global=%q x-rp-layer=%q",
		rec.Path, rec.Host, rec.Header.Get("Authorization"), rec.Header.Get("User-Agent"),
		rec.Header.Get("Accept"), rec.Header.Get("X-RP-Global"), rec.Header.Get("X-RP-Layer"))
	t.Logf("A3 上游体：model=%q metadata.call_source=%q temperature=%.2f has_prompt_cache_key=%v has_presence_penalty=%v",
		gjson.GetBytes(upBody, "model").String(), gjson.GetBytes(upBody, "metadata.call_source").String(),
		gjson.GetBytes(upBody, "temperature").Float(),
		gjson.GetBytes(upBody, "prompt_cache_key").Exists(), gjson.GetBytes(upBody, "presence_penalty").Exists())

	// ---- A4 配置变更生效（spec §11.9：配置进 digest ⇒ 新叶子；行为可证）----
	t.Log("A4 配置变更：改 patch 后新配置生效（行为 + plan generation 观测）")
	genBefore, _ := rpPlan(t, env)
	patchCapV2 := map[string]any{
		"headers": map[string]any{
			"Authorization": "E2E {api_key}",
			"Host":          "patched.example.invalid",
			"User-Agent":    "rp-e2e-ua/2.0",
			"X-RP-Global":   "g2",
		},
		"formats": map[string]any{
			"openai-chat": map[string]any{
				"set": map[string]any{"model": rpModelPatchV2},
			},
		},
	}
	code, body = rpPutExt(t, env, tplCap, rpExtBody("custom", map[string]string{"openai-chat": "v1/rp-chat"}, patchCapV2))
	require.Equal(t, 200, code, "A4 改 patch: %s", body)
	require.Equal(t, "rp-e2e-ua/2.0",
		gjson.Get(rpGetExt(t, env, tplCap), "request_patch.headers.User-Agent").String(), "GET 回显新配置")

	var lastRec rpHTTPReq
	pollUntil(t, "A4 配置变更生效（捕获到 X-RP-Global=g2 + 新 model）", func() (bool, string) {
		code, body := rpAIReq(t, env, kCap, "/v1/chat/completions", rpChatBody(rpModelChat), nil)
		if code != 200 {
			return false, fmt.Sprintf("code=%d body=%.200s", code, body)
		}
		lastRec = capture.lastHTTP(t)
		return lastRec.Header.Get("X-RP-Global") == "g2", "捕获 X-RP-Global=" + lastRec.Header.Get("X-RP-Global")
	})
	require.Equal(t, "rp-e2e-ua/2.0", lastRec.Header.Get("User-Agent"), "变更后的 UA patch 生效")
	require.Equal(t, rpModelPatchV2, gjson.GetBytes(lastRec.Body, "model").String(), "变更后的 body set 生效")
	genAfter, _ := rpPlan(t, env)
	t.Logf("A4 plan generation before=%d after=%d（patch 进 digest ⇒ 换叶证据）", genBefore, genAfter)
	require.Greater(t, genAfter, genBefore,
		"改 request_patch ⇒ digest 变化 ⇒ 发布新计划（generation 必须前进，否则补丁未进 planKey）")

	// ---- A5 非 custom 对照（spec §11.7：行为与未配置一致）----
	t.Log("A5 非 custom（api_key）对照：同一 harness 下无 patch 行为")
	rpWaitRoute(t, env, gCtrl, "openai-chat", rpModelChat, "A5 路由就绪")
	body = rpWaitChatOK(t, env, kCtrl, rpModelChat, "A5 鉴权/路由快照收敛")
	rec = capture.lastHTTP(t)
	require.Equal(t, "/v1/chat/completions", rec.Path, "非 custom 走内置默认端点（无 upstream_paths）")
	require.Equal(t, captureHost, rec.Host, "非 custom 的 Host = 上游 URL host（无 patch）")
	require.Equal(t, "Bearer rp-key-ctrl", rec.Header.Get("Authorization"), "非 custom = 网关默认鉴权头")
	for _, name := range []string{"X-RP-Global", "X-RP-Format", "X-RP-Layer", "X-RP-Global-Del", "X-RP-Drop"} {
		require.Empty(t, rec.Header.Get(name), "非 custom 不得出现 patch 头 %s", name)
	}
	require.NotEqual(t, "rp-e2e-ua/1.0", rec.Header.Get("User-Agent"), "非 custom 的 UA 未被 patch")
	require.NotEqual(t, "application/vnd.rp+json", rec.Header.Get("Accept"), "非 custom 的 Accept 未被 patch")
	require.Equal(t, rpModelChat, gjson.GetBytes(rec.Body, "model").String(), "非 custom 的 model 未被改写")
	require.Equal(t, rpClientCache, gjson.GetBytes(rec.Body, "prompt_cache_key").String(), "非 custom 的 body 字段原样")
	require.False(t, gjson.GetBytes(rec.Body, "metadata").Exists(), "非 custom 不注入 patch 字段")

	// ---- A6 日志面脱敏（spec §11.16 / §8）----
	t.Log("A6 日志面：上游回显 key → err_logs/server.log 脱敏；客户端 4xx 原文面不脱敏（明示裁决）")
	code, body = rpPutExt(t, env, tplLeak, rpExtBody("custom", nil, map[string]any{
		"headers": map[string]any{
			"Authorization": "E2E {api_key}",
			"X-RP-Echo":     "auth",
		},
	}))
	require.Equal(t, 200, code, "写脱敏场景 ext: %s", body)
	rpWaitRoute(t, env, gLeak, "openai-chat", rpModelLeak, "A6 路由就绪")

	_, leakResp := rpChatUntil(t, env, kLeak, rpModelLeak, nil, "A6 上游收到请求并以 4xx 回显凭据",
		func(code int, body string) bool {
			return code == 400 && strings.Contains(body, rpLeakAccountKey)
		})
	require.Contains(t, leakResp, "E2E "+rpLeakAccountKey,
		"客户端 4xx 原文面不脱敏（spec §8 明示裁决：沿用既有 transmit 语义）")

	var errMsg string
	pollUntil(t, "A6 err_logs 4xx 行落盘且 error_message 已脱敏", func() (bool, string) {
		err := env.pg.QueryRow(context.Background(),
			`SELECT COALESCE(error_message,'') FROM err_logs WHERE model=$1 ORDER BY id DESC LIMIT 1`, rpModelLeak).Scan(&errMsg)
		if err != nil {
			return false, err.Error()
		}
		return strings.Contains(errMsg, "[redacted]"), "error_message=" + errMsg
	})
	require.NotContains(t, errMsg, rpLeakAccountKey, "err_logs.error_message 不含 {api_key} 展开后的 key")
	require.Contains(t, errMsg, "[redacted]", "脱敏占位可见")
	t.Logf("A6 客户端 4xx 原文（不脱敏）= %.200s", leakResp)
	t.Logf("A6 err_logs.error_message（已脱敏）= %s", errMsg)
	logText := rpLogFile(t, h)
	require.NotContains(t, logText, rpLeakAccountKey, "server.log 不含 {api_key} 展开后的 key")
	// 成功路径零 patch 日志（spec §11.20）：配置值/账号 key 一律不进日志。
	for _, secret := range []string{"patched.example.invalid", "rp-e2e-ua", "rp-key-real", "rp-key-cap", "rp-key-ws"} {
		require.NotContains(t, logText, secret, "成功路径日志不得含 patch 配置/账号 key（%s）", secret)
	}

	// ---- B Responses WS（尽力；spec §11.5 / §7.2）----
	t.Log("B WS：握手头/Host/{api_key}/格式层删网关默认 beta 头；首帧 + 后续 create 帧 patch；cancel 字节不变")
	code, body = rpPutExt(t, env, tplWS, rpExtBody("custom",
		map[string]string{
			"openai-chat":         "v1/rp-ws-chat",
			"openai-responses-ws": "v1/responses",
		},
		map[string]any{
			"headers": map[string]any{
				"Authorization":  "E2E {api_key}",
				"Host":           "ws.patched.example.invalid",
				"User-Agent":     "rp-e2e-ws-ua/1.0",
				"X-RP-WS-Global": "g1",
				"X-RP-WS-Del":    "global",
			},
			"formats": map[string]any{
				"openai-responses-ws": map[string]any{
					"headers": map[string]any{
						"Responses-Websockets": "",
						"X-RP-WS-Global":       "format",
						"X-RP-WS-Format":       "f1",
						"X-RP-WS-Del":          "",
					},
					"set": map[string]any{
						"model":                rpModelWSPatch,
						"metadata.call_source": "rp-ws",
					},
					"delete": []string{"prompt_cache_key"},
				},
			},
		}))
	require.Equal(t, 200, code, "写 WS 模板 ext: %s", body)
	rpWaitRoute(t, env, gWS, "openai-chat", rpModelChat, "B 预热（chat）路由就绪")
	rpWaitRoute(t, env, gWS, "openai-responses-ws", rpModelWS, "B WS 路由就绪")
	// 行为收敛门：全局层头对 openai-chat 同样生效 ⇒ chat 捕获到 "E2E rp-key-ws" 即证明
	// WS 模板的新快照已生效（WS 格式层 patch 与全局层同属一份 digest/快照）。
	code, body = rpChatUntil(t, env, kWS, rpModelChat, nil, "B 预热 chat（key/auth + WS 模板 patch 快照收敛）",
		func(code int, _ string) bool {
			if code != 200 {
				return false
			}
			return capture.lastHTTP(t).Header.Get("Authorization") == "E2E rp-key-ws"
		})
	require.Equal(t, 200, code, "B 预热 chat: %s", body)

	conn := rpDialGatewayWS(t, env, kWS)
	t.Cleanup(func() { conn.CloseNow() })
	frame0 := []byte(`{"type":"response.create","model":"` + rpModelWS + `","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"prompt_cache_key":"rp-ws-client-cache"}`)
	frame1 := []byte(`{"type":"response.create","model":"` + rpModelWS + `","input":[{"role":"user","content":[{"type":"input_text","text":"again"}]}],"prompt_cache_key":"rp-ws-client-cache"}`)
	for _, frame := range [][]byte{frame0, frame1, []byte(rpWSCancelFrame)} {
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		require.NoError(t, conn.Write(wctx, websocket.MessageText, frame), "客户端帧写出")
		cancel()
	}
	got, err := rpDrainWS(t, conn, 20*time.Second)
	require.NoError(t, err, "WS 会话应正常关闭（上游 1000）")

	wsRec := capture.lastWS(t)
	require.Len(t, wsRec.Frames, 3, "上游应恰好收到 3 帧（首帧 + 第二个 create + cancel）")
	require.Equal(t, "/v1/responses", wsRec.Path, "WS 上游路径来自 upstream_paths")
	require.Equal(t, "ws.patched.example.invalid", wsRec.Host, "WS Host 覆盖走 DialOptions.Host")
	require.Equal(t, "E2E rp-key-ws", wsRec.Header.Get("Authorization"), "WS 握手 Authorization 覆盖 + {api_key} 展开")
	require.Equal(t, "rp-e2e-ws-ua/1.0", wsRec.Header.Get("User-Agent"), "WS 握手 UA 覆盖")
	require.Equal(t, "format", wsRec.Header.Get("X-RP-WS-Global"), "WS 格式层覆盖全局层")
	require.Equal(t, "f1", wsRec.Header.Get("X-RP-WS-Format"), "WS 格式层 Set")
	require.Empty(t, wsRec.Header.Get("X-RP-WS-Del"), "WS 格式层 Del 覆盖全局层 Set")
	require.Empty(t, wsRec.Header.Get("Responses-Websockets"), "WS 格式层空值可删网关默认 beta 头")

	require.Equal(t, rpModelWSPatch, gjson.GetBytes(wsRec.Frames[0], "model").String(), "首帧 body patch（model）")
	require.Equal(t, "rp-ws", gjson.GetBytes(wsRec.Frames[0], "metadata.call_source").String(), "首帧 body set（嵌套 path）")
	require.False(t, gjson.GetBytes(wsRec.Frames[0], "prompt_cache_key").Exists(), "首帧 body delete")
	require.Equal(t, rpModelWSPatch, gjson.GetBytes(wsRec.Frames[1], "model").String(), "后续 response.create 帧也 patch")
	require.Equal(t, "rp-ws", gjson.GetBytes(wsRec.Frames[1], "metadata.call_source").String())
	require.Equal(t, rpWSCancelFrame, string(wsRec.Frames[2]), "response.cancel 帧逐字节不变（控制帧原样转发）")

	completed := false
	for _, f := range got {
		if gjson.GetBytes(f, "type").String() == "response.completed" {
			completed = true
		}
	}
	require.True(t, completed, "客户端收到 response.completed（WS 中继链路真实可用；收到帧数=%d）", len(got))
	t.Logf("B 上游握手：path=%s host=%s auth=%q ua=%q x-rp-ws-global=%q beta=%q",
		wsRec.Path, wsRec.Host, wsRec.Header.Get("Authorization"), wsRec.Header.Get("User-Agent"),
		wsRec.Header.Get("X-RP-WS-Global"), wsRec.Header.Get("Responses-Websockets"))
	t.Logf("B 上游帧：n=%d f0.model=%q f0.metadata=%q f1.model=%q f2.cancel_bytes_unchanged=%v 客户端收到帧数=%d",
		len(wsRec.Frames), gjson.GetBytes(wsRec.Frames[0], "model").String(),
		gjson.GetBytes(wsRec.Frames[0], "metadata.call_source").String(),
		gjson.GetBytes(wsRec.Frames[1], "model").String(),
		string(wsRec.Frames[2]) == rpWSCancelFrame, len(got))

	// ---- C probe（account-model-test，P5 数据面；spec §11.6/§7.3/§7.4/§10 P5）----
	//
	// probe 是 custom 模板的诊断面：它必须与生产 HTTP 面同一份 wire 语义，否则
	// 「探测结果」不能作为配置生效的证据。本段走**真实管理面探测端点**
	// （POST /accounts/{id}/test）→ service → prober → 捕获上游，断言探测请求实际
	// 携带的 patch 握手头/body；并用 SQL 直写脏库触发 probe 面残余失败 WARN。
	t.Log("C1 probe 出站报文：真实管理面探测端点 → 捕获上游断言 patch 握手头/body；7 键按出站格式取层")
	tplProbe := rpCreateCustomTemplate(t, env, "rp-tpl-probe", captureURL,
		[]string{"openai-chat", "openai-responses", "openai-responses-ws", "openai-images"},
		[]string{rpModelProbeChat, rpModelProbeResp, rpModelProbeWS, rpModelProbeImg})
	gProbe := env.create("/groups", map[string]any{"name": "rp-g-probe"})
	accProbe := rpCreateAccount(t, env, "rp-acc-probe", tplProbe, "rp-key-probe", gProbe)
	pngDataURL := "data:image/png;base64," + capture.imgB64
	code, body = rpPutExt(t, env, tplProbe, rpExtBody("custom",
		map[string]string{
			"openai-chat":         "v1/rp-probe-chat",
			"openai-responses":    "v1/rp-probe-resp",
			"openai-responses-ws": "v1/rp-probe-ws",
			"openai-images.edits": "v1/rp-probe-img-edits",
		},
		map[string]any{
			"headers": map[string]any{
				"Authorization":     "E2E {api_key}",
				"Host":              "probe.example.invalid",
				"User-Agent":        "rp-probe-ua/1.0",
				"X-RP-Probe-Global": "g1",
			},
			"formats": map[string]any{
				"openai-chat": map[string]any{
					"headers": map[string]any{"X-RP-Probe-Format": "chat"},
					"set": map[string]any{
						"model":                rpProbeChatPatched,
						"metadata.call_source": "rp-probe-chat",
					},
					"delete": []string{"max_tokens"},
				},
				"openai-responses": map[string]any{
					"headers": map[string]any{"X-RP-Probe-Resp": "resp"},
					"set": map[string]any{
						"model":                rpProbeRespPatched,
						"metadata.call_source": "rp-probe-resp",
					},
					"delete": []string{"max_output_tokens"},
				},
				"openai-responses-ws": map[string]any{
					"headers": map[string]any{"X-RP-Probe-WS": "ws"},
				},
				"openai-images.edits": map[string]any{
					"headers": map[string]any{"X-RP-Probe-ImgEdit": "edit"},
					"set":     map[string]any{"prompt": "patched-by-e2e"},
				},
			},
		}))
	require.Equal(t, 200, code, "写 probe 模板 ext: %s", body)

	// probe 出站报告面（每次 probe 恰好一条上游请求）：全局层 + 该格式自己的层。
	assertProbeUpstream := func(rec rpHTTPReq, wantPath, wantFormatHeader, wantFormatValue, what string) {
		t.Helper()
		require.Equal(t, wantPath, rec.Path, "%s：probe 出站路径来自 upstream_paths（与 patch 同一把格式键）", what)
		require.Equal(t, "probe.example.invalid", rec.Host, "%s：probe Host 覆盖走 req.Host", what)
		require.Equal(t, "E2E rp-key-probe", rec.Header.Get("Authorization"),
			"%s：probe Authorization 被 patch 覆盖 + {api_key} 展开为账号 key", what)
		require.Equal(t, "rp-probe-ua/1.0", rec.Header.Get("User-Agent"), "%s：probe UA 覆盖", what)
		require.Equal(t, "g1", rec.Header.Get("X-RP-Probe-Global"), "%s：probe 全局层头", what)
		require.Equal(t, wantFormatValue, rec.Header.Get(wantFormatHeader), "%s：probe 格式层头取自己的键", what)
	}

	code, body = rpProbe(t, env, accProbe, map[string]any{"model": rpModelProbeChat, "format": "openai-chat"})
	rpRequireProbeSuccess(t, code, body, "C1 probe chat")
	rec = capture.lastHTTP(t)
	assertProbeUpstream(rec, "/v1/rp-probe-chat", "X-RP-Probe-Format", "chat", "C1 chat")
	require.Equal(t, rpProbeChatPatched, gjson.GetBytes(rec.Body, "model").String(), "C1 chat：body set（model）")
	require.Equal(t, "rp-probe-chat", gjson.GetBytes(rec.Body, "metadata.call_source").String(), "C1 chat：body 嵌套 set")
	require.False(t, gjson.GetBytes(rec.Body, "max_tokens").Exists(), "C1 chat：body delete（max_tokens）")
	require.Equal(t, "hi", gjson.GetBytes(rec.Body, "messages.0.content").String(), "C1 chat：探测载荷 input 原样")

	code, body = rpProbe(t, env, accProbe, map[string]any{"model": rpModelProbeResp, "format": "openai-responses"})
	rpRequireProbeSuccess(t, code, body, "C1 probe responses")
	rec = capture.lastHTTP(t)
	assertProbeUpstream(rec, "/v1/rp-probe-resp", "X-RP-Probe-Resp", "resp", "C1 responses")
	require.Empty(t, rec.Header.Get("X-RP-Probe-Format"), "C1 responses：不消费 openai-chat 层头")
	require.Equal(t, rpProbeRespPatched, gjson.GetBytes(rec.Body, "model").String(), "C1 responses：body set（本层）")
	require.False(t, gjson.GetBytes(rec.Body, "max_output_tokens").Exists(), "C1 responses：body delete（本层）")

	// responses-ws：探测实际走 HTTP responses 语义，但 patch 键与 URL 路径同一把
	// （openai-responses-ws）——必须不消费 openai-responses 键的头/体（spec §7.3）。
	code, body = rpProbe(t, env, accProbe, map[string]any{"model": rpModelProbeWS, "format": "openai-responses-ws"})
	rpRequireProbeSuccess(t, code, body, "C1 probe responses-ws")
	rec = capture.lastHTTP(t)
	assertProbeUpstream(rec, "/v1/rp-probe-ws", "X-RP-Probe-WS", "ws", "C1 responses-ws")
	require.Empty(t, rec.Header.Get("X-RP-Probe-Resp"),
		"C1 responses-ws：键是 openai-responses-ws——不消费 openai-responses 层头（7 键映射）")
	require.Equal(t, rpModelProbeWS, gjson.GetBytes(rec.Body, "model").String(),
		"C1 responses-ws：未配 body 层 ⇒ 不消费 openai-responses 层 body patch")
	require.True(t, gjson.GetBytes(rec.Body, "max_output_tokens").Exists(),
		"C1 responses-ws：openai-responses 层 delete 不作用于本键")
	require.False(t, gjson.GetBytes(rec.Body, "metadata").Exists())

	// images edits：multipart 非 JSON 容器 ⇒ 结构性跳过 body（只注入头）；上游回
	// 可解析 b64 PNG ⇒ probe 结论 success=true 且预览非空（整条探测链路可用）。
	code, body = rpProbe(t, env, accProbe, map[string]any{
		"model": rpModelProbeImg, "format": "openai-images",
		"image_mode": "edits", "image_url": pngDataURL,
	})
	rpRequireProbeSuccess(t, code, body, "C1 probe images edits")
	require.NotEmpty(t, gjson.Get(body, "image_previews").Array(), "C1 images：200 回包 b64 PNG 通过校验（真实预览非空）")
	rec = capture.lastHTTP(t)
	require.Equal(t, "/v1/rp-probe-img-edits", rec.Path, "C1 images edits：出站路径取自 openai-images.edits 键")
	require.Equal(t, "E2E rp-key-probe", rec.Header.Get("Authorization"), "C1 images edits：头装饰生效")
	require.Equal(t, "edit", rec.Header.Get("X-RP-Probe-ImgEdit"), "C1 images edits：只注入格式层头")
	require.True(t, strings.HasPrefix(rec.Header.Get("Content-Type"), "multipart/form-data"),
		"images edits 走 multipart（非 JSON）: %s", rec.Header.Get("Content-Type"))
	require.NotContains(t, string(rec.Body), "patched-by-e2e",
		"C1 images edits：multipart 非 JSON ⇒ body patch 结构性跳过（只头）")
	require.Contains(t, string(rec.Body), "Turn this image into a watercolor painting",
		"C1 images edits：probe 默认 prompt 原样")
	t.Logf("C1 probe 上游：chat/responses/responses-ws 各自键的 path=%q auth=%q 头层+body 逐项生效；images edits contentType=%q header=%q body_patch_skipped=true",
		"/v1/rp-probe-chat", "E2E rp-key-probe", rec.Header.Get("Content-Type"), rec.Header.Get("X-RP-Probe-ImgEdit"))

	// ---- C2 probe 真实脏库 → 残余失败 WARN（spec §7.4/§8/§11.16；P5 fix1 观测面）----
	t.Log("C2 probe 真实脏库：SQL 直写 template_exts.request_patch → 残余失败 WARN（固定文案/限频/零泄漏）+ 探测结论不变")
	tplDirty := rpCreateCustomTemplate(t, env, "rp-tpl-dirty", captureURL, []string{"openai-chat"}, []string{rpModelDirty})
	gDirty := env.create("/groups", map[string]any{"name": "rp-g-dirty"})
	accDirty := rpCreateAccount(t, env, "rp-acc-dirty", tplDirty, rpDirtyAccountKey, gDirty)
	code, body = rpPutExt(t, env, tplDirty, rpExtBody("custom", nil, nil))
	require.Equal(t, 200, code, "C2 建 ext 行（request_patch 先为空）: %s", body)
	// 脏值构造：路径文法合法、但下标超过 sjson 数组 padding 上限（maxpad=100000）⇒
	// 只在运行时 splice 失败。保存期写面不做执行试跑（spec §5.2 评审 I6：空文档试跑
	// 只作辅助），所以「保存期本应拒绝」在 wire 文法层并不成立——**这类残余失败只可能
	// 由手工改库/极端组合引入**；读路径与保存期共用同一 ParseRequestPatch，非法 wire
	// 会被视为未配置，因此 SQL 直写是该观测面的唯一真实入口。直写同时绕过快照/
	// digest 副作用（probe 每次从 DB 读模板 ext，不依赖调度快照）。
	dirtyPatch := fmt.Sprintf(
		`{"headers":{"Authorization":"E2E {api_key}","X-RP-Dirty":"d1"},"formats":{"openai-chat":{"set":{"secret_marker":"%s","%s":1}}}}`,
		rpDirtySecretMarker, rpDirtyPathMarker)
	_, err = env.pg.Exec(context.Background(),
		`UPDATE public.template_exts SET request_patch = $1::jsonb WHERE template_id = $2`,
		dirtyPatch, tplDirty)
	require.NoError(t, err, "SQL 直写脏 request_patch（模拟手工改库）")
	// 两次探测：第二次用于锁「同一 template+format 1min 内只 1 条 WARN」（共享限频表）。
	for i := 1; i <= 2; i++ {
		code, body = rpProbe(t, env, accDirty, map[string]any{"model": rpModelDirty, "format": "openai-chat"})
		rpRequireProbeSuccess(t, code, body, fmt.Sprintf("C2 脏库 probe #%d", i))
	}
	rec = capture.lastHTTP(t)
	require.Equal(t, "E2E "+rpDirtyAccountKey, rec.Header.Get("Authorization"),
		"C2：头装饰与 body 残余失败相互独立（头照常生效）")
	require.Equal(t, "d1", rec.Header.Get("X-RP-Dirty"), "C2：全局层头照常注入")
	require.Equal(t, rpModelDirty, gjson.GetBytes(rec.Body, "model").String(), "C2：残余失败 ⇒ body 裸透传（不半应用）")
	require.True(t, gjson.GetBytes(rec.Body, "max_tokens").Exists(), "C2：探测载荷原样（max_tokens 保留）")
	require.False(t, gjson.GetBytes(rec.Body, "secret_marker").Exists(), "C2：残余失败不得半应用任何 set")
	require.NotContains(t, string(rec.Body), rpDirtySecretMarker)

	var warnLines []map[string]any
	for _, line := range rpServerLogJSONLines(t, h) {
		if line["msg"] == rpProbeWarnMessage {
			warnLines = append(warnLines, line)
		}
	}
	require.Len(t, warnLines, 1, "C2：残余失败必须恰好 1 条固定文案 WARN（同 template+format 1min 限频）")
	warnLine := warnLines[0]
	require.Equal(t, float64(tplDirty), warnLine["template_id"], "C2：WARN 含 template_id")
	require.Equal(t, "openai-chat", warnLine["format"], "C2：WARN 含出站格式键")
	for _, forbidden := range []string{"path", "value", "body", "set", "delete", "headers", "authorization", "upstream_key"} {
		require.NotContains(t, warnLine, forbidden, "C2：WARN 字段集不得含 %s（不打印 path/value/body）", forbidden)
	}
	logText = rpLogFile(t, h)
	for _, leak := range []string{rpDirtySecretMarker, rpDirtyPathMarker, rpDirtyAccountKey} {
		require.NotContains(t, logText, leak, "C2：日志不得泄漏 %s", leak)
	}
	t.Logf("C2 脏库 probe WARN：msg=%q template_id=%v format=%v（两次探测共 %d 条；上游 body 逐字节裸透传）",
		warnLine["msg"], warnLine["template_id"], warnLine["format"], len(warnLines))
}
