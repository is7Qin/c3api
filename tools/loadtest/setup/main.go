// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// setup 构造多租户压测数据（Phase 3a 数据模型 + Phase 5 计费字段）：
// 模板（三格式 × 随机模型池）→ 公开组 → 账号（分散模板/组/上游）→ 用户（可选
// 余额/并发区间）→ 逐个登录建 key（可选多 key/并发/额度区间），key 明文写文件
// （loadtest -keys 用）。-price-models 给随机 N 个模型 manual 定价（验证计费
// 链路有价）；-billing-enabled 一步到位：默认余额区间 + 全部模型池定价。
//
//	用法: go run ./tools/loadtest/setup \
//	  -addr http://127.0.0.1:8080 -admin-token <C3API_ADMIN_TOKEN> \
//	  -upstream http://127.0.0.1:9100 \
//	  -users 5000 -accounts 5000 -groups 20 -keys-out keys.txt
//
// 说明：
//   - 模板 base_url = -upstream（裸根约定：不含 /v1，服务端校验会拒绝尾 /v1）；
//     -upstreams 逗号分隔多上游时轮流分配（压测机跑多个 fakeup 实例不同端口）
//   - 组全部 public（key 可选性无限制）；账号 upstream_key 统一 "sk-upstream"
//   - 用户密码统一 "loadtest-pass-1"（bcrypt 校验可验证）
//   - key 并发/额度随机区间内取值，随机值 0 = 该 key 不设限制（"随机挑选填充"）
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	addr        = flag.String("addr", "http://127.0.0.1:8080", "gateway base url")
	adminToken  = flag.String("admin-token", "", "C3API_ADMIN_TOKEN (admin API auth)")
	upstream    = flag.String("upstream", "http://127.0.0.1:9100", "fake upstream base url (bare root)")
	upstreams   = flag.String("upstreams", "", "comma-separated upstream base urls, templates round-robin (empty = single -upstream)")
	users       = flag.Int("users", 5000, "number of users")
	accounts    = flag.Int("accounts", 5000, "number of upstream accounts")
	groups      = flag.Int("groups", 20, "number of public groups")
	templates   = flag.Int("templates", 6, "number of templates (random 1-20 models from the pool, formats round-robin)")
	keysPerUser = flag.Int("keys-per-user", 1, "keys per user (each key independent name + random group)")
	keysOut     = flag.String("keys-out", "keys.txt", "output file with one key per line")
	workers     = flag.Int("workers", 64, "parallelism for user/key creation (bcrypt heavy)")
	userBalUsd  = flag.String("user-balance-usd", "0", "user balance USD random interval like 1-100 (0 = don't fill)")
	userMaxCC   = flag.String("user-max-concurrency", "0", "user max_concurrency random interval like 4-16 (0 = don't set)")
	keyMaxCC    = flag.String("key-max-concurrency", "0", "key max_concurrency random interval (0 = don't set)")
	keyQuota    = flag.String("key-quota", "0", "key quota (tokens) random interval; random 0 = that key unlimited")
	priceModels = flag.Int("price-models", 0, "random N models from the pool get manual pricing (0 = none)")
	billingOn   = flag.Bool("billing-enabled", false, "fill user balances (default 10-100 USD) + price the whole model pool (billing loadtest)")
)

const (
	pass = "loadtest-pass-1"
	user = "user%d@loadtest.test"
)

// modelPool 模板模型池（38 个常见模型名；fakeup 回显不做真实性校验，名字仅用于
// 模板 models 集合与 manual 定价键；不含 "/" 保证 URL 路径段安全）。
var modelPool = []string{
	"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano",
	"gpt-4-turbo", "gpt-3.5-turbo", "gpt-5", "gpt-5-mini", "gpt-5.6-sol",
	"o1", "o1-mini", "o3", "o3-mini",
	"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022",
	"claude-3-opus-20240229", "claude-3-7-sonnet-20250219",
	"claude-opus-4-1", "claude-sonnet-4-5", "claude-haiku-4-5",
	"claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-7",
	"gemini-1.5-pro", "gemini-1.5-flash", "gemini-1.5-flash-8b",
	"gemini-2.0-flash", "gemini-2.0-pro", "gemini-2.5-pro", "gemini-2.5-flash",
	"llama-3.3-70b-instruct", "llama-3.1-8b-instruct", "llama-3.1-405b-instruct",
	"deepseek-chat", "deepseek-reasoner", "qwen-max", "qwen-plus",
	"mistral-large-latest", "mistral-medium", "mixtral-8x7b-instruct",
}

// tplFormats 模板格式轮流序：默认 6 个模板 = 每格式 ×2（-b 后缀配对，与旧行为一致）。
var tplFormats = []string{"openai-chat", "openai-responses", "anthropic"}

// 响应解析用最小结构（JSON 字段名 = Go 字段名 / openapi tag，见 api.gen.go）。
type tpl struct{ ID int64 }
type grp struct{ ID int64 }
type acc struct{ ID int64 }
type usr struct{ ID int64 }
type loginResp struct {
	Token string `json:"token"`
}
type keyResp struct {
	Key string `json:"key"`
}

func main() {
	flag.Parse()
	if *adminToken == "" {
		fmt.Fprintln(os.Stderr, "-admin-token required (C3API_ADMIN_TOKEN)")
		os.Exit(2)
	}
	// -billing-enabled：默认余额区间 + 全部模型池定价——用户有钱 + 模型有价是
	// 计费压测前提（缺价 402 全拒/免费用户是事故，不是被测行为）；显式传参覆盖。
	if *billingOn {
		if *userBalUsd == "0" {
			*userBalUsd = "10-100"
		}
		if *priceModels == 0 {
			*priceModels = len(modelPool)
		}
	}
	if *priceModels > len(modelPool) {
		fmt.Fprintf(os.Stderr, "-price-models %d > pool size %d\n", *priceModels, len(modelPool))
		os.Exit(2)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	// callNoExit 发请求：auth = 完整 "Authorization" 头值（"" = 不带头；登录公开）。
	// 非 200 返回 error（不退出）——调用方决定重试/上报；out 解析成功才返回 nil。
	callNoExit := func(method, path, auth string, body any, out any) error {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, *addr+path, rd)
		if err != nil {
			return err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s %s → %d: %s", method, path, resp.StatusCode, b)
		}
		if out != nil {
			return json.Unmarshal(b, out)
		}
		return nil
	}
	// call 发请求：失败即退出（既有调用点语义不变）。
	call := func(method, path, auth string, body any, out any) {
		if err := callNoExit(method, path, auth, body, out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	admin := func(method, path string, body any, out any) {
		call(method, path, "Bearer "+*adminToken, body, out)
	}
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))

	// 1) 模板（-templates 个；三格式轮流 ×2 配对，随机 1-20 模型；多上游轮流 base_url）
	upURLs := []string{*upstream}
	if *upstreams != "" {
		upURLs = strings.Split(*upstreams, ",")
		for i := range upURLs {
			upURLs[i] = strings.TrimSpace(upURLs[i])
			if upURLs[i] == "" {
				fmt.Fprintln(os.Stderr, "-upstreams: empty entry")
				os.Exit(2)
			}
		}
	}
	tplStart := time.Now()
	tplIDs := make([]int64, 0, *templates)
	for i := 0; i < *templates; i++ {
		f := tplFormats[(i/2)%len(tplFormats)]
		// 名字仅 6 个唯一（3 格式 ×2），-templates > 6 会撞唯一键 409；
		// 从第 7 个起追加序号保证唯一（名字仅装饰，压测用 ID 引用）。
		name := fmt.Sprintf("tpl-%s", f)
		if i%2 == 1 {
			name += "-b"
		}
		if i >= len(tplFormats)*2 {
			name += fmt.Sprintf("-%d", i)
		}
		var out tpl
		admin(http.MethodPost, "/admin/templates", map[string]any{
			"name": name, "base_url": upURLs[i%len(upURLs)],
			"supported_formats": []string{f}, "models": randomModels(rng),
		}, &out)
		tplIDs = append(tplIDs, out.ID)
	}
	fmt.Printf("templates: %d %v (%s)\n", len(tplIDs), tplIDs, time.Since(tplStart).Round(time.Millisecond))

	// 2) 公开组 ×N
	gStart := time.Now()
	groupIDs := make([]int64, 0, *groups)
	for i := 0; i < *groups; i++ {
		var out grp
		admin(http.MethodPost, "/admin/groups", map[string]any{
			"name": fmt.Sprintf("pool-%d", i), "visibility": "public",
		}, &out)
		groupIDs = append(groupIDs, out.ID)
	}
	fmt.Printf("groups: %d (%s)\n", len(groupIDs), time.Since(gStart).Round(time.Millisecond))

	// 3) 账号 ×N：模板/组随机分配（必须解耦——若模板与组同用 i%N，组 g 只会
	// 绑到单个模板，三格式请求在非对应组 404 "no account supports this
	// request format"；随机化后每组含全格式模板账号，且每模板都分布到多组）
	aStart := time.Now()
	for i := 0; i < *accounts; i++ {
		var out acc
		admin(http.MethodPost, "/admin/accounts", map[string]any{
			"name":         fmt.Sprintf("acc-%d", i),
			"template_id":  tplIDs[rng.IntN(len(tplIDs))],
			"upstream_key": "sk-upstream",
			"group_ids":    []int64{groupIDs[rng.IntN(len(groupIDs))]},
			// max_concurrency 显式 100000：service 校验把 0 兜底为 8，
			// 8 槽 × 1666 chat 账号 = 13k 槽 < 30k 并发 → 大量 429 选号失败
			// （压测目标 = 网关热路径，账号槽不设限，与 §7 SQL 直插同语义）
			"weight": 100, "max_concurrency": 100000,
		}, &out)
	}
	fmt.Printf("accounts: %d (%s)\n", *accounts, time.Since(aStart).Round(time.Millisecond))

	// 4) manual 定价（可选）：模型池随机 N 个（billing-enabled = 全部）——
	// 基础价 + 随机 1-2 个矩阵字段（priority/fast 等），保证计费链路有价。
	pStart := time.Now()
	for _, model := range pickModels(rng, *priceModels) {
		admin(http.MethodPut, "/admin/pricing?model="+url.QueryEscape(model), randomPricingBody(rng), nil)
	}
	fmt.Printf("pricing: %d models (%s)\n", *priceModels, time.Since(pStart).Round(time.Millisecond))

	// 5) 用户 ×N（可选余额/并发区间）+ 6) 逐个登录建 key（可选多 key/并发/
	// 额度；bcrypt 重，并行 worker）
	keysFile, err := os.Create(*keysOut)
	must(err)
	defer keysFile.Close()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workers)
	var created int64
	uStart := time.Now()
	for i := 0; i < *users; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, rng *rand.Rand) {
			defer wg.Done()
			defer func() { <-sem }()
			email := fmt.Sprintf(user, i)
			userBody := map[string]any{"email": email, "password": pass}
			if bal, ok := randUSD(rng, *userBalUsd); ok {
				userBody["balance"] = bal
			}
			if v, ok := randIntRange(rng, *userMaxCC); ok {
				userBody["max_concurrency"] = v
			}
			var u usr
			admin(http.MethodPost, "/admin/users", userBody, &u)
			var lr loginResp
			call(http.MethodPost, "/user/auth/login", "", map[string]any{
				"email": email, "password": pass,
			}, &lr)
			for k := 0; k < *keysPerUser; k++ {
				keyBody := map[string]any{
					"name":     fmt.Sprintf("load-%d-%d", i, k),
					"group_id": groupIDs[rng.IntN(len(groupIDs))], // 随机分配组
				}
				if v, ok := randIntRange(rng, *keyMaxCC); ok {
					keyBody["max_concurrency"] = v
				}
				if v, ok := randIntRange(rng, *keyQuota); ok {
					keyBody["quota"] = v
				}
				var kr keyResp
				// 新建用户快照 NOTIFY 传播窗口：admin 建用户后立即 keys 可能撞
				// 401（网关内存快照未刷新——RequireJWT 用户状态 fail-closed）。
				// 重试至多 3 次（间隔 300ms），窗口 ~1s 内必过；压测 fill 场景
				// 用户是预热建的（慢于快照周期），重试仅覆盖 setup 冷启动。
				keysCall := func() error {
					var e error
					for attempt := 0; attempt < 3; attempt++ {
						if attempt > 0 {
							time.Sleep(300 * time.Millisecond)
						}
						if e = callNoExit(http.MethodPost, "/user/keys", "Bearer "+lr.Token, keyBody, &kr); e == nil {
							return nil
						}
					}
					return e
				}
				if err := keysCall(); err != nil {
					fmt.Fprintf(os.Stderr, "keys create failed after retries: %v\n", err)
					os.Exit(1)
				}
				mu.Lock()
				fmt.Fprintln(keysFile, kr.Key)
				created++
				if created%1000 == 0 {
					fmt.Printf("keys: %d\n", created)
				}
				mu.Unlock()
			}
		}(i, rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(i+1))))
	}
	wg.Wait()
	fmt.Printf("users: %d (%s)\n", *users, time.Since(uStart).Round(time.Millisecond))
	fmt.Printf("done: users=%d keys=%d → %s\n", *users, created, *keysOut)
}

// pickModels 池内随机 N 个互不重复的模型（Perm 洗牌取前 N）。
func pickModels(rng *rand.Rand, n int) []string {
	out := make([]string, n)
	perm := rng.Perm(len(modelPool))
	for i := 0; i < n; i++ {
		out[i] = modelPool[perm[i]]
	}
	return out
}

// randomModels 每模板随机 1-20 个互不重复模型（fakeup 回显不校验模型真实性）。
func randomModels(rng *rand.Rand) []string {
	return pickModels(rng, 1+rng.IntN(20))
}

// randomPricingBody 基础价（prompt/completion，USD/1M tokens——API 契约
// float 直发，handler usdToMillis ×1e5 落库毫分）+ 随机 1-2 个矩阵字段
// （priority/flex 单价替换档、above 分段、fast 倍率）。注意：主价单位 USD
// 非毫分——旧 int 50000-500000 会被当 $50k-500k/1M，×1e5 落库后单请求扣
// 巨款余额秒光（402），压测数据全废。
func randomPricingBody(rng *rand.Rand) map[string]any {
	prompt := 0.5 + rng.Float64()*4.5 // 0.5-5 USD/1M tokens
	completion := prompt * 4
	body := map[string]any{
		"prompt_price_per_million":     prompt,
		"completion_price_per_million": completion,
	}
	matrix := []func(){
		func() { body["priority_prompt_price_per_million"] = prompt * 2 },
		func() { body["priority_completion_price_per_million"] = completion * 2 },
		func() { body["flex_prompt_price_per_million"] = prompt * 3 / 2 },
		func() { body["flex_completion_price_per_million"] = completion * 3 / 2 },
		func() { body["above_threshold"] = 128000 },
		func() { body["above_prompt_price_per_million"] = prompt / 2 },
		func() { body["fast_multiplier"] = 1.0 + rng.Float64()*5.0 }, // ×1.0-6.0（API 契约 float 直发；旧 int 万分数契约已废）
	}
	for _, idx := range rng.Perm(len(matrix))[:1+rng.IntN(2)] {
		matrix[idx]()
	}
	return body
}

// parseRange 解析 "min-max" / 单值 "v" → (min, max)；""/"0" = 不设置，
// 负数或 min > max → 启动即退出。
func parseRange(s string) (min, max int64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, 0, false
	}
	parts := strings.SplitN(s, "-", 2)
	parse := func(t string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		must(err)
		return v
	}
	min = parse(parts[0])
	max = min
	if len(parts) == 2 {
		max = parse(parts[1])
	}
	if min < 0 || max < min {
		fmt.Fprintf(os.Stderr, "invalid range %q: want min-max with 0 <= min <= max\n", s)
		os.Exit(2)
	}
	return min, max, true
}

// randIntRange 解析区间并返回 [min,max] 内随机整数；""/"0" = 不设置。
// 随机值恰为 0 → 不设置（下限 0 的区间 = "随机挑选实体填充"语义）。
func randIntRange(rng *rand.Rand, s string) (int64, bool) {
	min, max, ok := parseRange(s)
	if !ok || max == 0 {
		return 0, false
	}
	v := min
	if max > min {
		v = min + rng.Int64N(max-min+1)
	}
	if v == 0 {
		return 0, false
	}
	return v, true
}

// randUSD 同 randIntRange，返回 2 位小数 USD（int 区间 ×100 后 /100，
// 如 "1-100" → $1.00-$100.00，毫分换算无精度损失）。
func randUSD(rng *rand.Rand, s string) (float64, bool) {
	min, max, ok := parseRange(s)
	if !ok || max == 0 {
		return 0, false
	}
	v := min*100 + rng.Int64N((max-min)*100+1)
	return float64(v) / 100, true
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
}
