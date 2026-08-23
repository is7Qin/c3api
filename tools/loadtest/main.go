// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// loadtest 对网关打压测：固定并发 goroutine 持续请求，支持流式首字节和非流式完整响应延迟。
// 用法: go run ./tools/loadtest -mode stream -addr http://127.0.0.1:8080 -key ck-xxx -concurrency 10000 -duration 5m -healthz http://127.0.0.1:8080/healthz
//
//	go run ./tools/loadtest -mode fill -fill-type users -admin-token <C3API_ADMIN_TOKEN> -concurrency 2000 -duration 5m
//
// 混合压测（模型请求 + 填充 API 并发）：开两个 loadtest 进程同时跑——一个
// -mode stream -keys keys.txt、一个 -mode fill，各自 -out 落盘。同机交错跑 +
// 每请求 CPU 对比（压测机 loadavg 50+，单进程内混流会让 fill 请求被流式
// 长连接饿死，双进程是简单可靠的分流）。
// 相对 brief 原代码的修正（均标注在行内）：
//   - os import 用 -out 兜底：把 RESULT 摘要同时写入文件（验收记录留档）。
//   - 采样 goroutine 的 elapsed 直接取真实经过时间（brief 里 time.Since 套
//     time.Since 的表达式恒为 ~0s，属"简化输出"占位）。
//   - 首字节采样从 sync.Map 改为 mutex map（并发 CAS 实测在 Go 1.26
//     HashTrieMap 上高争用会活锁，压测卡死，见压测记录）。
//   - 连接级失败 100-300ms 抖动退避 + -warmup 预热窗口：Windows 监听
//     backlog≈200，无退避的突发拨号会形成自持 RST 拒绝风暴（500 并发
//     无退避实测 99.4% refused）。
//   - 错误明细（err_detail）输出 + -pprof goroutine 转储，便于定位压测问题。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	concurrency = flag.Int("concurrency", 10000, "concurrent streams")
	duration    = flag.Duration("duration", 5*time.Minute, "test duration")
	warmup      = flag.Duration("warmup", 10*time.Second, "untimed connection warm-up before the clock starts")
	addr        = flag.String("addr", "http://127.0.0.1:8080", "gateway addr")
	key         = flag.String("key", "ck-", "single gateway key (fallback when -keys is empty)")
	keysFile    = flag.String("keys", "", "file with one gateway key per line; pick random per request (multi-key load)")
	format      = flag.String("format", "chat", "request format: chat, responses, anthropic or images")
	healthz     = flag.String("healthz", "", "gateway /healthz url to sample memory")
	out         = flag.String("out", "", "write RESULT summary to this file as well")
	pprof       = flag.String("pprof", "", "listen addr for /debug/pprof (goroutine dump on hang)")
	mode        = flag.String("mode", "stream", "request mode: stream, chat, fill, models, api-admin or api-user")
	// fill 模式（管理面填充 API 压测）：并发创建用户/key/账号/组/模板/定价。
	adminToken   = flag.String("admin-token", "", "C3API_ADMIN_TOKEN (fill/api-admin mode admin APIs; keys fill 走用户面不需要)")
	fillType     = flag.String("fill-type", "users", "fill mode entity: users, keys, accounts, groups, templates, pricing or mixed")
	fillUser     = flag.String("fill-user", "user0@loadtest.test:loadtest-pass-1", "keys fill: 登录账号 email:password（-fill-user-file 为空时兜底）")
	fillUserFile = flag.String("fill-user-file", "", "keys fill: 每行 email:password 的账号文件，随机挑（分散登录压力，对齐 setup 用户命名）")
	fillTplID    = flag.Int64("fill-template-id", 1, "accounts fill: 模板 ID（setup 创建的第 1 个模板，压测前确认存在）")
	fillGroupID  = flag.Int64("fill-group-id", 1, "keys/accounts fill: 组 ID（setup 创建的第 1 个组）")
	fillUpstream = flag.String("fill-upstream", "http://127.0.0.1:9100", "templates fill: base_url（裸根约定）")
	fillKeysOut  = flag.String("fill-keys-out", "", "keys fill: 把创建的 key 明文逐行追加写入此文件（fill 主循环响应解析；压测前 keys.txt 落盘用）")
	// api 模式（管理面/用户面全端点锤）：兑换码跨进程交接文件。
	codesOut  = flag.String("codes-out", "", "api-admin: 生成的兑换码逐行追加此文件（供 api-user -codes-in 消费）")
	codesIn   = flag.String("codes-in", "", "api-user: 可核销兑换码文件（追加式，进程内周期重读；多进程并跑会有重复核销 4xx 噪声）")
	readsOnly = flag.Bool("api-reads-only", false, "api 模式只跑读场景（增长后纯读延迟对比用）")
)

// keyPool 多 key 模式：每请求随机取一个（-keys 文件行）；空 = 用 -key 单 key。
var keyPool []string

// 采样桶数：10ms/桶 → 覆盖 0-10.24s；超出区间（≥10.24s）进边界桶 1023。
const sampleBuckets = 1024

type metrics struct {
	total       atomic.Int64
	errs        atomic.Int64
	firstByteMS atomic.Int64 // stream 首字节延迟之和
	latencyMS   atomic.Int64 // chat 完整响应延迟之和
	// 延迟采样直方图：固定桶数组 + 原子自增，无锁。原 mutex map 在
	// 30k 并发下每请求抢同一把锁（最大热点）；p99 遍历数组同样无锁。
	samples        [sampleBuckets]atomic.Int64 // stream 首字节延迟采样：10ms/桶
	latencySamples [sampleBuckets]atomic.Int64 // chat 完整响应延迟采样：10ms/桶
	mu             sync.Mutex                  // 仅保护 errDetail（错误路径低频，0 错误零锁）
	errDetail      map[string]int64
}

func (m *metrics) addErr(detail string) {
	m.mu.Lock()
	m.errDetail[detail]++
	m.mu.Unlock()
}

func main() {
	flag.Parse()
	switch *mode {
	case "stream", "chat", "fill", "models", "api-admin", "api-user":
	default:
		fmt.Fprintf(os.Stderr, "invalid -mode %q: want stream, chat, fill, models, api-admin or api-user\n", *mode)
		os.Exit(2)
	}
	if *mode == "fill" {
		validateFillFlags()
	}
	if *mode == "api-admin" {
		if *adminToken == "" {
			fmt.Fprintln(os.Stderr, "-mode api-admin requires -admin-token (C3API_ADMIN_TOKEN)")
			os.Exit(2)
		}
		if *codesIn != "" {
			fmt.Fprintln(os.Stderr, "-mode api-admin ignores -codes-in (admin 生成码用 -codes-out)")
			os.Exit(2)
		}
	}
	if *mode == "api-user" && *fillUserFile != "" {
		loadFillUserFile()
		if *codesIn != "" {
			apiReloadCodes()
			fmt.Printf("loaded %d redeem codes from %s\n", len(codesSeen), *codesIn)
		}
	}
	if *format != "chat" && *format != "responses" && *format != "anthropic" && *format != "images" {
		fmt.Fprintf(os.Stderr, "invalid -format %q: want chat, responses, anthropic or images\n", *format)
		os.Exit(2)
	}
	if *keysFile != "" {
		b, err := os.ReadFile(*keysFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read -keys %s: %v\n", *keysFile, err)
			os.Exit(2)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				keyPool = append(keyPool, line)
			}
		}
		if len(keyPool) == 0 {
			fmt.Fprintf(os.Stderr, "-keys %s: no keys found\n", *keysFile)
			os.Exit(2)
		}
		fmt.Printf("loaded %d keys from %s\n", len(keyPool), *keysFile)
	}
	if *pprof != "" {
		go func() { _ = http.ListenAndServe(*pprof, nil) }() // net/http/pprof 自动挂载
	}
	m := &metrics{errDetail: make(map[string]int64)}
	if *mode != "fill" && *mode != "api-admin" && *mode != "api-user" {
		buildReqTemplate()
	}
	if *mode == "api-admin" {
		apiBootstrapAdmin(http.DefaultClient)
	}
	warmEnd := time.Now().Add(*warmup)
	start := warmEnd
	stop := start.Add(*duration)

	// 每 worker 自建局部随机源：math/rand/v2 全局源带锁，30k 并发下
	// pickKey/退避抢同一把锁；按 worker 索引分种子，退避不会同频共振。
	// transport 同样 per-worker（#19）：全局共享 transport 的连接池锁
	// （connsMu）在 50k 并发下是跨 worker 争抢点；每 worker 独立 transport
	// 后池锁归零竞争，连接数不变（稳态每 worker 恰好一条 keep-alive）。
	randSeed := uint64(time.Now().UnixNano())
	isAPI := *mode == "api-admin" || *mode == "api-user"
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(idx int, rng *rand.Rand) {
			defer wg.Done()
			// 每 worker 独立 transport（池容量 1 即够——稳态恒 1 条连接）：
			// 避免共享 transport 的连接池锁竞争（#19）；参数同全局版。
			transport := &http.Transport{
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     90 * time.Second,
			}
			client := &http.Client{Timeout: 10 * time.Minute, Transport: transport}
			if isAPI {
				// api 模式：worker 自含循环（登录/预热/计时段都在内）。
				apiWorkerLoop(newAPIWorker(idx, client, rng), warmEnd, stop)
				return
			}
			// 预热：先跑不计数的流，把突发拨号造成的 RST 吸收在计时窗口外，
			// 同时让 keep-alive 连接池就位（Windows backlog≈200，见错误退避注释）。
			for time.Now().Before(warmEnd) {
				if *mode == "fill" {
					doFillRequest(client, m, rng, false)
				} else {
					doRequest(client, m, rng, false)
				}
			}
			for time.Now().Before(stop) {
				if *mode == "fill" {
					doFillRequest(client, m, rng, true)
				} else {
					doRequest(client, m, rng, true)
				}
			}
		}(i, rand.New(rand.NewPCG(randSeed, uint64(i+1))))
	}

	// 采样 goroutine：打印即时进度 + /healthz 内存
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var last int64
		for range t.C {
			cur := m.total.Load()
			fmt.Printf("elapsed=%s total=%d rate=%.0f/s errs=%d\n",
				time.Since(start).Round(time.Second),
				cur, float64(cur-last)/10, m.errs.Load())
			last = cur
			if *healthz != "" {
				if resp, err := http.Get(*healthz); err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					fmt.Printf("  gateway: %s\n", b)
				}
			}
		}
	}()

	wg.Wait()
	result := fmt.Sprintf("\n=== RESULT ===\nmode=%s\n", *mode)
	if *mode == "fill" {
		result += fmt.Sprintf("fill_type=%s\n", *fillType)
	}
	if isAPI {
		result += apiResultSection()
	} else {
		result += fmt.Sprintf("total=%d errs=%d\n", m.total.Load(), m.errs.Load())
		if *mode == "chat" || *mode == "models" || *mode == "fill" {
			result += fmt.Sprintf("avg_latency_ms=%.1f\n", float64(m.latencyMS.Load())/float64(max(1, m.total.Load()-m.errs.Load())))
			result += fmt.Sprintf("p99_latency_ms=%d\n", p99Latency(m))
		} else {
			result += fmt.Sprintf("avg_first_byte_ms=%.1f\n", float64(m.firstByteMS.Load())/float64(max(1, m.total.Load()-m.errs.Load())))
			result += fmt.Sprintf("p99_first_byte_ms=%d\n", p99(m))
		}
	}
	result += fmt.Sprintf("elapsed=%s concurrency=%d\n", time.Since(start).Round(time.Second), *concurrency)
	if isAPI {
		for _, line := range apiMergedErrDetails() {
			result += line + "\n"
		}
	} else {
		m.mu.Lock()
		for d, c := range m.errDetail {
			if c >= 1 {
				result += fmt.Sprintf("err_detail: %d x %s\n", c, d)
			}
		}
		m.mu.Unlock()
	}
	fmt.Print(result)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(result), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		}
	}
}

// pickKey 每请求选 key：多 key 池随机（-keys 文件）→ 单 key（-key）兜底。
// rng 为 worker 局部随机源（全局 rand 带锁，见 main 内注释）。
func pickKey(rng *rand.Rand) string {
	if len(keyPool) > 0 {
		return keyPool[rng.IntN(len(keyPool))]
	}
	return *key
}

// newLoadtestRequest builds the minimal request for the selected mode + format.
// 三格式（多模板多格式压测）：chat → /v1/chat/completions（Bearer），
// responses → /v1/responses（Bearer），anthropic → /v1/messages（x-api-key）。
func newLoadtestRequest(base, groupKey, requestMode string) *http.Request {
	if requestMode == "models" {
		// GET /v1/models：网关内存快照直出（零上游、零 DB），延迟口径同 chat。
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+groupKey)
		return req
	}
	var path, body string
	switch *format {
	case "responses":
		path = "/v1/responses"
		body = `{"model":"gpt-4o","input":"hi"}`
		if requestMode == "stream" {
			body = `{"model":"gpt-4o","stream":true,"input":"hi"}`
		}
	case "anthropic":
		path = "/v1/messages"
		body = `{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		if requestMode == "stream" {
			body = `{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
	case "images":
		// images generations：非流式 JSON（data 数组计图计费）；流式变体需
		// 上游 SSE 帧规格支持，fakeupstream 未实现——压测覆盖非流式。
		path = "/v1/images/generations"
		body = `{"model":"gpt-image-1","prompt":"a cat","n":1,"size":"1024x1024"}`
	default: // chat
		path = "/v1/chat/completions"
		body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
		if requestMode == "stream" {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte(body)))
	if *format == "anthropic" {
		req.Header.Set("x-api-key", groupKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+groupKey)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// 请求模板：format×mode 在进程内固定，URL/body/固定头只构建一次；每请求
// Clone + 换 key，避免 http.NewRequest 的 URL 解析 + body 字符串复制 +
// 头表构建（4 万+ req/s 下是 GC 的主要来源之一，见上机 profile）。
var (
	reqTmpl  *http.Request
	tmplBody []byte
)

// buildReqTemplate 按当前 flags 预构建请求模板（main 启动时调用一次）。
func buildReqTemplate() {
	reqTmpl = newLoadtestRequest(*addr, *key, *mode)
	if reqTmpl.Body != nil { // GET（models）无体
		tmplBody, _ = io.ReadAll(reqTmpl.Body)
	}
	reqTmpl.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tmplBody)), nil
	}
}

// newRequestFromTemplate 克隆模板并按 key 设置认证头（其余静态）。
func newRequestFromTemplate(groupKey string) *http.Request {
	req := reqTmpl.Clone(context.Background())
	if *format == "anthropic" {
		req.Header.Set("x-api-key", groupKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+groupKey)
	}
	req.Body = io.NopCloser(bytes.NewReader(tmplBody))
	req.ContentLength = int64(len(tmplBody))
	return req
}

// doRequest executes one request; count=true includes it in the result metrics.
// Connection failures retain the jittered backoff used by the stream benchmark.
func doRequest(client *http.Client, m *metrics, rng *rand.Rand, count bool) {
	req := newRequestFromTemplate(pickKey(rng))
	reqStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr("do:" + err.Error())
		}
		// 连接级失败退避（100-300ms 抖动）：Windows 监听 backlog（SOMAXCONN≈200）
		// 下突发拨号会被 RST，无退避的立即重试会形成自持拒绝风暴
		// （Task 9 实测：500 并发无退避 99.4% refused，150 并发无失败）。
		time.Sleep(time.Duration(100+rng.IntN(200)) * time.Millisecond)
		return
	}
	if resp.StatusCode != 200 {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr(fmt.Sprintf("status:%d", resp.StatusCode))
		}
		// 排空非 200 响应体再 Close：不读完 body 就 Close 会让传输层判定
		// body 不完整、keep-alive 连接不可回池 → 429 风暴下每响应重拨
		// （压测实测：客户端 syscall 87.4% 打满 14 核 + 本地端口耗尽
		// cannot assign requested address，规则引擎冷却后 429 自放大级联）。
		// 同 fill 路径惯例（doFillRequest 无条件 io.Copy(io.Discard) 后再
		// Close）；排空失败读错误忽略，非 200 已计错误明细，不影响分类。
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return
	}
	if *mode == "chat" || *mode == "models" {
		_, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if count {
				m.errs.Add(1)
				m.total.Add(1)
				m.addErr("read:" + readErr.Error())
			}
			return
		}
		if count {
			latency := time.Since(reqStart).Milliseconds()
			m.latencyMS.Add(latency)
			storeLatencySample(m, latency)
			m.total.Add(1)
		}
		return
	}
	if count {
		firstByte := time.Since(reqStart).Milliseconds()
		m.firstByteMS.Add(firstByte)
		storeSample(m, firstByte)
	}
	br := sseReaderPool.Get().(*bufio.Reader)
	br.Reset(resp.Body)
	_ = drainSSE(br) // 排空尽力而为：读错误（服务端提前断流）不影响压测统计
	// [DONE] 后把响应体尾部读到 EOF（chunked/Content-Length 的 EOF 由帧结构
	// 决定，µs 级到达）：让传输层判定 body 完整、keep-alive 连接回池复用——
	// 原实现每请求关连接重拨（profile 里 dialConn 占 11% CPU）。服务端不
	// 结束流时 drainTail 50ms 超时放弃，不挂死。
	_ = drainTail(resp.Body) // 超时/EOF 均属预期，尽力而为
	br.Reset(nil)
	sseReaderPool.Put(br)
	resp.Body.Close()
	if count {
		m.total.Add(1)
	}
}

// sseReaderPool 复用每请求的 bufio.Reader（4KB 缓冲，4 万+ req/s 下零化/
// 分配量可观）；回池前 Reset(nil) 丢弃跨连接残留缓冲数据。
var sseReaderPool = sync.Pool{New: func() any { return bufio.NewReader(nil) }}

// ---- fill 模式：管理面填充 API 压测 ----

// fillSeq 全局请求序号（实体名/邮箱后缀，同进程内必唯一）。
var fillSeq atomic.Int64

// fillProc 进程号：与 fillSeq 组合成全局唯一名——单进程无重复键；多进程
// 并跑同服务时撞键 → 服务端 409/400 计入错误明细（预期，不特殊重试）。
var fillProc = int64(os.Getpid())

// fillMix mixed 填充类型的循环轮转序。
var fillMix = []string{"users", "keys", "accounts", "groups", "templates", "pricing"}

// fillFormats templates 填充轮流格式。
var fillFormats = []string{"openai-chat", "openai-responses", "anthropic"}

// fillModels 模板/定价填充的模型名池（fakeup 回显不做真实性校验）。
var fillModels = []string{
	"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-5", "o3-mini",
	"claude-3-5-sonnet-20241022", "claude-opus-4-6", "gemini-2.5-pro",
	"llama-3.3-70b-instruct", "deepseek-chat", "qwen-max", "mistral-large-latest",
}

// fillUserPool keys 填充的登录账号池（-fill-user-file 每行 email:password）；
// 空 = 单账号 -fill-user 兜底（对齐 setup 用户命名约定）。
var fillUserPool []string

// loadFillUserFile 加载 -fill-user-file 登录账号池（fill keys / api-user 共用）。
func loadFillUserFile() {
	if *fillUserFile == "" || len(fillUserPool) > 0 {
		return
	}
	b, err := os.ReadFile(*fillUserFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read -fill-user-file %s: %v\n", *fillUserFile, err)
		os.Exit(2)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fillUserPool = append(fillUserPool, line)
		}
	}
	if len(fillUserPool) == 0 {
		fmt.Fprintf(os.Stderr, "-fill-user-file %s: no entries found\n", *fillUserFile)
		os.Exit(2)
	}
	fmt.Printf("loaded %d fill users from %s\n", len(fillUserPool), *fillUserFile)
}

// validateFillFlags fill 模式启动校验：fill-type 枚举 + admin token 依赖 +
// 登录账号文件加载。
func validateFillFlags() {
	switch *fillType {
	case "users", "accounts", "groups", "templates", "pricing", "mixed":
		if *adminToken == "" {
			fmt.Fprintf(os.Stderr, "-mode fill with -fill-type %s requires -admin-token (C3API_ADMIN_TOKEN)\n", *fillType)
			os.Exit(2)
		}
	case "keys":
		// keys 填充走用户面（登录 + /api/user/keys），不需要 admin token
	default:
		fmt.Fprintf(os.Stderr, "invalid -fill-type %q: want users, keys, accounts, groups, templates, pricing or mixed\n", *fillType)
		os.Exit(2)
	}
	loadFillUserFile()
}

// fillTypeFor mixed 模式按全局请求序号循环轮流（每请求一种填充类型）。
func fillTypeFor(seq int64) string {
	return fillMix[int(seq-1)%len(fillMix)]
}

// pickFillUser 随机挑登录账号：池内（-fill-user-file）→ 单账号（-fill-user）兜底。
func pickFillUser(rng *rand.Rand) (email, password string) {
	entry := *fillUser
	if len(fillUserPool) > 0 {
		entry = fillUserPool[rng.IntN(len(fillUserPool))]
	}
	email, password, _ = strings.Cut(entry, ":")
	return
}

// newFillRequest 构造一次填充请求：实体名带 进程号+全局序号（不重复创建）；
// keys 类型先登录取 JWT（登录失败返回 preErr 非空，调用方计为错误）。
func newFillRequest(client *http.Client, rng *rand.Rand) (req *http.Request, preErr string) {
	seq := fillSeq.Add(1)
	tag := fmt.Sprintf("fill-%d-%d", fillProc, seq)
	mk := func(method, path string, body any) *http.Request {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, *addr+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+*adminToken)
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	typ := *fillType
	if typ == "mixed" {
		typ = fillTypeFor(seq)
	}
	switch typ {
	case "users":
		// 含余额/并发：计费预检需要用户有钱（余额快照）+ 用户级在途门禁
		return mk(http.MethodPost, "/api/admin/users", map[string]any{
			"email": tag + "@loadtest.test", "password": "fill-pass-1",
			"balance": 100.0, "max_concurrency": 8,
		}), ""
	case "keys":
		// 登录（bcrypt 校验，管理面最重路径之一）+ 建 key，同一事务计延迟
		email, password := pickFillUser(rng)
		loginBody, _ := json.Marshal(map[string]any{"email": email, "password": password})
		req, _ := http.NewRequest(http.MethodPost, *addr+"/api/user/auth/login", bytes.NewReader(loginBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, "login:do:" + err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Sprintf("login:status:%d", resp.StatusCode)
		}
		var lr struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(b, &lr)
		req = mk(http.MethodPost, "/api/user/keys", map[string]any{
			"name": "fill-key-" + tag, "group_id": *fillGroupID,
		})
		req.Header.Set("Authorization", "Bearer "+lr.Token)
		return req, ""
	case "accounts":
		return mk(http.MethodPost, "/api/admin/accounts", map[string]any{
			"name": tag, "template_id": *fillTplID, "upstream_key": "sk-fill",
			"group_ids": []int64{*fillGroupID}, "weight": 100, "max_concurrency": 100000,
		}), ""
	case "groups":
		return mk(http.MethodPost, "/api/admin/groups", map[string]any{
			"name": "grp-" + tag, "visibility": "public",
		}), ""
	case "templates":
		return mk(http.MethodPost, "/api/admin/templates", map[string]any{
			"name": "tpl-" + tag, "base_url": *fillUpstream,
			"supported_formats": []string{fillFormats[int(seq-1)%len(fillFormats)]},
			"models":            []string{fillModels[rng.IntN(len(fillModels))]},
		}), ""
	case "pricing":
		// PUT 幂等 upsert：同模型重复设价 = 覆盖更新，不撞唯一键。
		// 单位契约：/api/admin/prices/{model} 的 input_per_m/output_per_m 为
		// "USD/百万 token"（handler ×1e5 存毫分）。此前 fill 误传毫分（250000）→
		// 定价虚高 1e5 倍，每请求扣费 3.3 万元 → 压测 402 风暴根因（实测 2026-08-18）。
		// 量级对齐真实模型价（claude sonnet ≈ 21/105 元每百万 in/out）。
		return mk(http.MethodPut, "/api/admin/prices/"+url.PathEscape(fillModels[rng.IntN(len(fillModels))]), map[string]any{
			"mode":         "token",
			"input_per_m":  20 + rng.Int64N(180), // USD/百万 token
			"output_per_m": 60 + rng.Int64N(540), // USD/百万 token
		}), ""
	}
	return nil, "fill:unknown-type:" + typ // 不可达（validateFillFlags 已校验）
}

// doFillRequest 执行一次填充事务（count=true 计入结果统计）：成功（200）计
// 完整响应延迟（入 latency 直方图，同 chat 模式语义）；非 200 计错误明细
// （撞键 409/400 属预期）；连接级失败沿用请求模式的抖动退避防 RST 风暴。
func doFillRequest(client *http.Client, m *metrics, rng *rand.Rand, count bool) {
	reqStart := time.Now()
	req, preErr := newFillRequest(client, rng)
	if preErr != "" {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr(preErr)
		}
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr("do:" + err.Error())
		}
		time.Sleep(time.Duration(100+rng.IntN(200)) * time.Millisecond)
		return
	}
	// keys fill：响应体先整体读入（小 JSON，读完即排空、连接可复用）解析 key 明文
	// 落盘（DB 只存 hash 不可恢复；否则 -fill-keys-out 无效）。其余类型走 Discard。
	var fillKeyB []byte
	if *fillKeysOut != "" && *fillType == "keys" {
		fillKeyB, _ = io.ReadAll(resp.Body)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body) // 响应体排空，连接回池复用（O3 复核：此路径与 keys 登录子请求均已排空，唯一缺口在 doRequest 非 200 分支，已修）
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr(fmt.Sprintf("status:%d", resp.StatusCode))
		}
		return
	}
	if count {
		latency := time.Since(reqStart).Milliseconds()
		m.latencyMS.Add(latency)
		storeLatencySample(m, latency)
		m.total.Add(1)
	}
	if fillKeyB != nil {
		var kr struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(fillKeyB, &kr); err == nil && kr.Key != "" {
			f, err := os.OpenFile(*fillKeysOut, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = f.WriteString(kr.Key + "\n")
				_ = f.Close()
			}
		}
	}
}

// errDrainTimeout [DONE] 后服务端未在 50ms 内结束响应体：放弃该连接复用。
var errDrainTimeout = errors.New("drainTail: body not ended within 50ms")

// drainTail [DONE] 后把响应体剩余字节读到 EOF，使传输层判定 body 完整 →
// 连接回池复用；服务端不主动结束流时 50ms 超时 Close 放弃（Close 会解除
// 阻塞中的 Read，不留泄漏 goroutine）。返回 nil 表示读到 EOF（连接可复用）。
func drainTail(body io.ReadCloser) error {
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, body); drained <- err }()
	select {
	case err := <-drained:
		return err
	case <-time.After(50 * time.Millisecond):
		body.Close()
		return errDrainTimeout
	}
}

// sseDone 每行检查复用同一 []byte，避免逐行转换分配。
var sseDone = []byte("[DONE]")

// drainSSE 读完整条流直到 [DONE] 行（返回 nil）或流结束（返回读取错误）。
// 原 ReadString 每行分配一个 string，30k 并发下堆压力大；ReadSlice 复用
// bufio 内部缓冲零分配，长行（ErrBufferFull）分段累积到局部 buf，行尾
// 一次性检查。注意 ReadSlice 返回的 slice 在下次读取后即失效，须立即消费。
func drainSSE(br *bufio.Reader) error {
	var buf []byte
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull { // 长行：累积分段，等后续续行
			buf = append(buf, line...)
			continue
		}
		if len(buf) > 0 {
			line = append(buf, line...) // 行完整：拼接分段后立即检查
			buf = buf[:0]
		}
		if bytes.Contains(line, sseDone) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func storeSample(m *metrics, v int64) {
	idx := v / 10
	if idx >= sampleBuckets {
		idx = sampleBuckets - 1
	}
	m.samples[idx].Add(1)
}

func storeLatencySample(m *metrics, v int64) {
	idx := v / 10
	if idx >= sampleBuckets {
		idx = sampleBuckets - 1
	}
	m.latencySamples[idx].Add(1)
}

func p99(m *metrics) int64 { return p99Buckets(&m.samples) }

func p99Latency(m *metrics) int64 { return p99Buckets(&m.latencySamples) }

// p99Buckets 遍历固定桶数组求 99% 分位（无锁），语义与旧 mutex map 版
// 一致：从低到高累积计数，首个累积 ≥ total*99/100 的桶返回上界（b*10 ms）。
func p99Buckets(samples *[sampleBuckets]atomic.Int64) int64 {
	var total int64
	for i := range samples {
		total += samples[i].Load()
	}
	if total == 0 {
		return -1
	}
	target := total * 99 / 100
	var acc int64
	for i := range samples {
		acc += samples[i].Load()
		if acc >= target {
			return int64(i) * 10
		}
	}
	return -1 // 不可达：桶总和 = total > target，必有桶越过阈值
}
