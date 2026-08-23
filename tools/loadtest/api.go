// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// ---- api 模式：管理面/用户面全端点读写混合锤 ----
//
// -mode api-admin：管理面全端点（静态 token 鉴权）——18 类读 + 写建混合。
//   写入只增不删（压测目的=灌数据），实体名带进程号+序号防撞键；账号创建
//   绑定启动时引导创建的 stress-root 组（不污染 v1 流量的选号池）。
//   codes.gen 场景把生成的兑换码逐行追加 -codes-out（用户面消费）。
// -mode api-user：用户面全端点——JWT 池（-fill-user-file 每 worker 绑定一个
//   身份登录一次，401 自动重登）；读 + keys create/rotate + 兑换码核销
//   （-codes-in 追加式文件顺序消费）+ register（bcrypt 天然限速）。
//
// 每场景独立 metrics（total/errs/avg/p99 直方图），RESULT 输出按 total 降序
// 的场景表。兑换码跨进程交接：admin -codes-out 追加写 → user -codes-in 周期
// 重读顺序核销；多 user 进程并跑会有重复核销 4xx（预期噪声，错误明细可见）。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// apiScenario 单端点场景：build 返回 (nil, nil) 表示前置条件缺失（如兑换码
// 未产出、无自建 key 可轮换）——静默跳过不计数。
type apiScenario struct {
	name   string
	write  bool
	weight int
	build  func(w *apiWorker, tag string, rng *rand.Rand) (*http.Request, error)
}

// apiWorker 单并发 worker 状态（每 goroutine 一个）。
type apiWorker struct {
	plane  string // admin | user
	client *http.Client
	rng    *rand.Rand
	ident  int     // 用户池身份索引（api-user）
	token  string  // user plane JWT
	keyIDs []int64 // 自建 key id 环形缓冲（rotate 用）
	keyPos int
}

// apiEntry 注册表项：场景指标 + 分组标记（报告用）。
type apiEntry struct {
	m     *metrics
	write bool
}

var (
	apiRegMu sync.Mutex
	apiReg   = map[string]*apiEntry{}
	apiSeq   atomic.Int64
	// stressGID admin 账号创建绑定的压力组 id（启动时引导创建；失败回退
	// -fill-group-id）。原子读写：worker 与 bootstrap 无锁竞争。
	stressGID atomic.Int64
)

func apiMetrics(name string, write bool) *metrics {
	apiRegMu.Lock()
	defer apiRegMu.Unlock()
	e := apiReg[name]
	if e == nil {
		e = &apiEntry{m: &metrics{errDetail: make(map[string]int64)}, write: write}
		apiReg[name] = e
	}
	return e.m
}

// apiResultSection 全局合计（跨场景合并直方图）+ 场景表（total 降序）。
func apiResultSection() string {
	apiRegMu.Lock()
	defer apiRegMu.Unlock()
	type row struct {
		name  string
		e     *apiEntry
		total int64
	}
	rows := make([]row, 0, len(apiReg))
	var total, errs, latSum int64
	var merged [sampleBuckets]atomic.Int64
	for n, e := range apiReg {
		t := e.m.total.Load()
		total += t
		errs += e.m.errs.Load()
		latSum += e.m.latencyMS.Load()
		for i := range merged {
			merged[i].Add(e.m.latencySamples[i].Load())
		}
		rows = append(rows, row{n, e, t})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })
	var b strings.Builder
	done := max(1, total-errs)
	fmt.Fprintf(&b, "total=%d\nerrs=%d\n", total, errs)
	fmt.Fprintf(&b, "avg_latency_ms=%.1f\n", float64(latSum)/float64(done))
	fmt.Fprintf(&b, "p99_latency_ms=%d\n", p99Buckets(&merged))
	fmt.Fprintf(&b, "scenarios=%d\n", len(rows))
	for _, r := range rows {
		m := r.e.m
		d := max(1, m.total.Load()-m.errs.Load())
		fmt.Fprintf(&b, "scenario=%s %s total=%d errs=%d avg_ms=%.1f p99_ms=%d\n",
			r.name, map[bool]string{false: "read", true: "WRITE"}[r.e.write],
			m.total.Load(), m.errs.Load(),
			float64(m.latencyMS.Load())/float64(d), p99Latency(m))
	}
	return b.String()
}

// ---- 场景表 ----

// apiGet 只读 GET 场景快捷构造。
func apiGet(path string, weight int, name string) apiScenario {
	return apiScenario{name: name, weight: weight, build: func(*apiWorker, string, *rand.Rand) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, *addr+path, nil)
		if err != nil {
			return nil, err
		}
		return req, nil
	}}
}

// apiRandPage 带随机分页的列表路径（page/page_size 或 limit/offset 二选一）。
func apiRandPage(rng *rand.Rand, pages int) string {
	return fmt.Sprintf("page=%d&page_size=%d", rng.IntN(pages)+1, 100)
}

func adminScenarios() []apiScenario {
	w := func(name string, weight int, method string, pathFn func(*rand.Rand) string, bodyFn func(*rand.Rand, string) any) apiScenario {
		return apiScenario{name: name, write: bodyFn != nil, weight: weight,
			build: func(_ *apiWorker, tag string, rng *rand.Rand) (*http.Request, error) {
				var body any
				if bodyFn != nil {
					body = bodyFn(rng, tag)
				}
				return apiReq(method, pathFn(rng), body, "Bearer "+*adminToken)
			}}
	}
	return []apiScenario{
		// ---- 读（18 类）----
		w("users.list", 10, "GET", func(r *rand.Rand) string { return "/api/admin/users?limit=100&offset=" + strconv.Itoa(r.IntN(60)*100) }, nil),
		w("keys.list", 8, "GET", func(r *rand.Rand) string { return "/api/admin/keys?limit=100&offset=" + strconv.Itoa(r.IntN(60)*100) }, nil),
		w("groups.list", 6, "GET", func(*rand.Rand) string { return "/api/admin/groups?limit=100" }, nil),
		w("templates.list", 5, "GET", func(*rand.Rand) string { return "/api/admin/templates?limit=100" }, nil),
		w("accounts.list", 8, "GET", func(r *rand.Rand) string {
			return "/api/admin/accounts?limit=100&offset=" + strconv.Itoa(r.IntN(60)*100)
		}, nil),
		w("accounts.usage", 4, "GET", func(r *rand.Rand) string {
			ids := make([]string, 10)
			for i := range ids {
				ids[i] = strconv.FormatInt(r.Int64N(5000)+1, 10)
			}
			return "/api/admin/accounts/usage?account_ids=" + strings.Join(ids, ",")
		}, nil),
		w("rules.list", 4, "GET", func(*rand.Rand) string { return "/api/admin/rules" }, nil),
		w("redemptions.list", 5, "GET", func(r *rand.Rand) string { return "/api/admin/redemption-codes?" + apiRandPage(r, 200) }, nil),
		w("pricing.list", 5, "GET", func(*rand.Rand) string { return "/api/admin/pricing?page=1&page_size=200" }, nil),
		w("image-price.list", 3, "GET", func(*rand.Rand) string { return "/api/admin/image-price?page=1&page_size=100" }, nil),
		w("function-prices.list", 2, "GET", func(*rand.Rand) string { return "/api/admin/function-prices?page=1&page_size=100" }, nil),
		w("usage_logs.list", 12, "GET", func(r *rand.Rand) string {
			return "/api/admin/usage_logs?" + apiDayRange() +
				"&limit=50&offset=" + strconv.Itoa(r.IntN(500)*50)
		}, nil),
		w("err_logs.list", 6, "GET", func(*rand.Rand) string {
			return "/api/admin/err_logs?" + apiDayRange() + "&limit=50"
		}, nil),
		// stats 重构 T6：旧 /stats 删除，新增 trend/top/entity-trend/ttft 四加权场景（后端 W3 落地前 404 属预期）
		w("stats.trend", 10, "GET", func(r *rand.Rand) string {
			return "/api/admin/stats/trend?" + apiStatsRangeMixed(r) + "&granularity=" + apiGranularity(r)
		}, nil),
		w("stats.top", 8, "GET", func(r *rand.Rand) string {
			entities := []string{"account", "user", "key"}
			bys := []string{"cost", "requests", "tokens"}
			return "/api/admin/stats/top?" + apiStatsRangeMixed(r) +
				"&entity=" + entities[r.IntN(len(entities))] +
				"&by=" + bys[r.IntN(len(bys))] + "&limit=20"
		}, nil),
		w("stats.entity-trend", 6, "GET", func(r *rand.Rand) string {
			entities := []string{"account", "user", "key"}
			ent := entities[r.IntN(len(entities))]
			id := r.Int64N(5000) + 1 // 从既有账号/用户池取样
			return "/api/admin/stats/entity-trend?entity=" + ent +
				"&id=" + strconv.FormatInt(id, 10) +
				"&" + apiStatsRangeMixed(r) + "&granularity=hour"
		}, nil),
		w("stats.ttft", 6, "GET", func(r *rand.Rand) string {
			base := "/api/admin/stats/ttft?" + apiStatsRangeMixed(r)
			// 部分请求带实体过滤走 exact 分支（打 usage_logs），其余走 sketch 分支（cube hist）
			if r.IntN(2) == 0 {
				entities := []string{"account", "user", "key"}
				ent := entities[r.IntN(len(entities))]
				id := r.Int64N(5000) + 1
				base += "&entity=" + ent + "&id=" + strconv.FormatInt(id, 10)
			}
			return base
		}, nil),
		w("overview.get", 8, "GET", func(*rand.Rand) string { return "/api/admin/overview" }, nil),
		w("users-top.get", 4, "GET", func(*rand.Rand) string { return "/api/admin/users-top" }, nil),
		w("ops.workers", 3, "GET", func(*rand.Rand) string { return "/api/admin/ops/workers" }, nil),
		w("temp-balances.list", 4, "GET", func(r *rand.Rand) string { return "/api/admin/temp-balances?" + apiRandPage(r, 50) }, nil),
		w("settings.get", 3, "GET", func(*rand.Rand) string { return "/api/admin/settings" }, nil),

		// ---- 写（只增灌数据；实体名带进程号+seq 防撞键）----
		w("users.create", 6, "POST", func(*rand.Rand) string { return "/api/admin/users" },
			func(_ *rand.Rand, tag string) any {
				return map[string]any{"email": "stress-" + tag + "@loadtest.test",
					"password": "stress-pass-1", "balance": 100.0, "max_concurrency": 8}
			}),
		w("groups.create", 6, "POST", func(*rand.Rand) string { return "/api/admin/groups" },
			func(_ *rand.Rand, tag string) any {
				return map[string]any{"name": "grp-stress-" + tag, "visibility": "public"}
			}),
		w("templates.create", 5, "POST", func(*rand.Rand) string { return "/api/admin/templates" },
			func(r *rand.Rand, tag string) any {
				return map[string]any{"name": "tpl-stress-" + tag, "base_url": *fillUpstream,
					"supported_formats": []string{fillFormats[r.IntN(len(fillFormats))]},
					"models":            []string{fillModels[r.IntN(len(fillModels))]}}
			}),
		w("accounts.create", 6, "POST", func(*rand.Rand) string { return "/api/admin/accounts" },
			func(_ *rand.Rand, tag string) any {
				return map[string]any{"name": "acct-stress-" + tag, "template_id": *fillTplID,
					"upstream_key": "sk-stress", "group_ids": []int64{stressGID.Load()},
					"weight": 100, "max_concurrency": 100000}
			}),
		w("rules.create", 5, "POST", func(*rand.Rand) string { return "/api/admin/rules" },
			func(r *rand.Rand, tag string) any {
				// priority 唯一约束：宽区间随机，进程内碰撞概率可忽略
				return map[string]any{"name": "rule-stress-" + tag,
					"priority": 2000 + r.Int64N(60000), "enabled": true}
			}),
		w("codes.gen", 6, "POST", func(*rand.Rand) string { return "/api/admin/redemption-codes" },
			func(_ *rand.Rand, _ string) any {
				return map[string]any{"count": 100, "type": "balance", "value": 1, "max_uses": 1}
			}),
		w("pricing.put", 2, "PUT", func(r *rand.Rand) string {
			return "/api/admin/pricing?model=" + url.QueryEscape(fillModels[r.IntN(len(fillModels))])
		}, func(r *rand.Rand, _ string) any {
			// 单位契约：元/百万 token（×1e5 存毫分），量级对齐真实价
			return map[string]any{"prompt_price_per_million": 20 + r.Int64N(180),
				"completion_price_per_million": 60 + r.Int64N(540)}
		}),
	}
}

func userScenarios() []apiScenario {
	w := func(name string, weight int, method string, pathFn func(*apiWorker, *rand.Rand) string, bodyFn func(*rand.Rand, string) any) apiScenario {
		return apiScenario{name: name, write: bodyFn != nil, weight: weight,
			build: func(w *apiWorker, tag string, rng *rand.Rand) (*http.Request, error) {
				var body any
				if bodyFn != nil {
					body = bodyFn(rng, tag)
				}
				return apiReq(method, pathFn(w, rng), body, "Bearer "+w.token)
			}}
	}
	return []apiScenario{
		// ---- 读 ----
		w("me.get", 8, "GET", func(*apiWorker, *rand.Rand) string { return "/api/user/auth/me" }, nil),
		w("my-keys.list", 10, "GET", func(*apiWorker, *rand.Rand) string { return "/api/user/keys" }, nil),
		w("my-groups.list", 6, "GET", func(*apiWorker, *rand.Rand) string { return "/api/user/groups" }, nil),
		w("my-usage_logs.list", 12, "GET", func(_ *apiWorker, r *rand.Rand) string {
			return "/api/user/usage_logs?" + apiDayRange() +
				"&limit=50&offset=" + strconv.Itoa(r.IntN(200)*50)
		}, nil),
		w("my-err_logs.list", 5, "GET", func(*apiWorker, *rand.Rand) string {
			return "/api/user/err_logs?" + apiDayRange() + "&limit=50"
		}, nil),
		w("my-stats.get", 12, "GET", func(_ *apiWorker, r *rand.Rand) string {
			return "/api/user/stats?" + apiStatsRangeMixed(r) + "&granularity=" + apiGranularity(r)
		}, nil),
		w("my-stats.ttft", 8, "GET", func(_ *apiWorker, r *rand.Rand) string {
			return "/api/user/stats/ttft?" + apiStatsRangeMixed(r)
		}, nil),
		w("my-redemptions.list", 5, "GET", func(*apiWorker, *rand.Rand) string { return "/api/user/redemptions" }, nil),
		w("my-temp-balances.list", 5, "GET", func(*apiWorker, *rand.Rand) string { return "/api/user/temp-balances" }, nil),

		// ---- 写 ----
		w("my-keys.create", 8, "POST", func(*apiWorker, *rand.Rand) string { return "/api/user/keys" },
			func(r *rand.Rand, tag string) any {
				return map[string]any{"name": "stress-key-" + tag, "group_id": r.Int64N(20) + 1}
			}),
		w("my-keys.rotate", 5, "POST", func(w *apiWorker, _ *rand.Rand) string {
			if len(w.keyIDs) == 0 {
				return "" // 无自建 key：静默跳过
			}
			id := w.keyIDs[w.keyPos%len(w.keyIDs)]
			w.keyPos++
			return "/api/user/keys/" + strconv.FormatInt(id, 10) + "/rotate"
		}, func(*rand.Rand, string) any { return map[string]any{} }),
		w("redeem.code", 8, "POST", func(*apiWorker, *rand.Rand) string { return "/api/user/redemptions" },
			func(*rand.Rand, string) any {
				code := nextRedeemCode()
				if code == "" {
					return nil // 无码可核销：跳过
				}
				return map[string]any{"code": code}
			}),
		w("register", 2, "POST", func(*apiWorker, *rand.Rand) string { return "/api/user/auth/register" },
			func(_ *rand.Rand, tag string) any {
				return map[string]any{"email": "stress-u-" + tag + "@loadtest.test", "password": "stress-pass-1"}
			}),
	}
}

// ---- 执行 ----

// apiDayRange 最近 24h 的 from/to（RFC3339，均 URL 编码）——usage_logs/
// err_logs 的必填时间窗参数。
func apiDayRange() string {
	now := time.Now().UTC()
	from := url.QueryEscape(now.Add(-24 * time.Hour).Format(time.RFC3339))
	to := url.QueryEscape(now.Format(time.RFC3339))
	return "from=" + from + "&to=" + to
}

// apiStatsRangeMixed stats 趋势类端点时间窗：近 24h 与近 7d 混合，复用 RFC3339 编码。
func apiStatsRangeMixed(rng *rand.Rand) string {
	now := time.Now().UTC()
	var from time.Time
	if rng.IntN(2) == 0 {
		from = now.Add(-24 * time.Hour)
	} else {
		from = now.Add(-7 * 24 * time.Hour)
	}
	return "from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Format(time.RFC3339))
}

// apiGranularity 趋势 granularity 轮换（hour/day）。
func apiGranularity(rng *rand.Rand) string {
	if rng.IntN(2) == 0 {
		return "hour"
	}
	return "day"
}

// apiReq 构造 JSON 请求（body nil = GET/无体）；token 为完整凭证值。
func apiReq(method, path string, body any, token string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, *addr+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", token)
	return req, nil
}

func (w *apiWorker) runScenario(sc *apiScenario, count bool) {
	if w.plane == "user" && w.token == "" {
		// 登录失败的身份：被调度到时先重试登录一次；仍失败则退避跳过
		// （防坏凭据高频轰登录接口）。
		w.relogin()
		if w.token == "" {
			time.Sleep(500 * time.Millisecond)
			return
		}
	}
	tag := fmt.Sprintf("%d-%d", fillProc, apiSeq.Add(1))
	m := apiMetrics(sc.name, sc.write)
	req, err := sc.build(w, tag, w.rng)
	if req == nil && err == nil {
		return // 前置缺失：不计
	}
	if err != nil {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr("build:" + err.Error())
		}
		return
	}
	reqStart := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr("do:" + err.Error())
		}
		time.Sleep(time.Duration(100+w.rng.IntN(200)) * time.Millisecond)
		return
	}
	var bodyB []byte
	keepBody := (sc.name == "codes.gen" && *codesOut != "") || sc.name == "my-keys.create"
	if keepBody {
		bodyB, _ = io.ReadAll(resp.Body)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 401 && w.plane == "user":
		// JWT 过期/失效：重登一次（本次不计，下个请求生效）
		w.relogin()
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// 非 2xx 计错（注意：部分创建端点返回 201，属成功）
		if count {
			m.errs.Add(1)
			m.total.Add(1)
			m.addErr(fmt.Sprintf("status:%d", resp.StatusCode))
		}
	default:
		if count {
			latency := time.Since(reqStart).Milliseconds()
			m.latencyMS.Add(latency)
			storeLatencySample(m, latency)
			m.total.Add(1)
		}
	}
	if keepBody && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var gr struct {
			Codes []struct {
				Code string `json:"Code"`
			} `json:"codes"`
		}
		if json.Unmarshal(bodyB, &gr) == nil {
			codes := make([]string, 0, len(gr.Codes))
			for i := range gr.Codes {
				if gr.Codes[i].Code != "" {
					codes = append(codes, gr.Codes[i].Code)
				}
			}
			appendCodesOut(codes)
		}
	}
	// my-keys.create：登记 id 供 rotate 轮换
	if sc.name == "my-keys.create" && resp.StatusCode >= 200 && resp.StatusCode < 300 && bodyB != nil {
		var kr struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(bodyB, &kr)
		if kr.ID > 0 {
			w.keyIDs = append(w.keyIDs, kr.ID)
			if len(w.keyIDs) > 256 {
				w.keyIDs = w.keyIDs[1:]
			}
		}
	}
}

// apiMergedErrDetails 全场景错误明细合并（按次数降序）——api 模式的 RESULT
// err_detail 段（全局 m 在 api 模式下不计数）。
func apiMergedErrDetails() []string {
	apiRegMu.Lock()
	defer apiRegMu.Unlock()
	merged := map[string]int64{}
	for _, e := range apiReg {
		e.m.mu.Lock()
		for d, c := range e.m.errDetail {
			merged[d] += c
		}
		e.m.mu.Unlock()
	}
	type dc struct {
		d string
		c int64
	}
	arr := make([]dc, 0, len(merged))
	for d, c := range merged {
		arr = append(arr, dc{d, c})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].c > arr[j].c })
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprintf("err_detail: %d x %s", x.c, x.d))
	}
	return out
}

// ---- 兑换码文件交接 ----

var (
	codesMu    sync.Mutex
	codesSeen  []string // 已加载顺序队列
	codesLoadN int      // 已读入行数（文件只追加，重读取增量）
	codesCur   int      // 单调核销游标：只前进不回绕——耗尽且无新增则跳过
)

func nextRedeemCode() string {
	codesMu.Lock()
	defer codesMu.Unlock()
	if codesCur >= len(codesSeen) {
		reloadCodesLocked()
	}
	if codesCur >= len(codesSeen) {
		return "" // 无新码：跳过（绝不回绕重兑已用码）
	}
	code := codesSeen[codesCur]
	codesCur++
	return code
}

func reloadCodesLocked() {
	if *codesIn == "" {
		return
	}
	b, err := os.ReadFile(*codesIn)
	if err != nil {
		return
	}
	var all []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			all = append(all, line)
		}
	}
	if len(all) > codesLoadN {
		codesSeen = append(codesSeen, all[codesLoadN:]...)
		codesLoadN = len(all)
	}
}

var codesOutMu sync.Mutex

// apiReloadCodes 启动期一次性加载 -codes-in（main 调用；运行期由
// nextRedeemCode 在耗尽时增量重读追加段）。
func apiReloadCodes() {
	codesMu.Lock()
	defer codesMu.Unlock()
	reloadCodesLocked()
}

func appendCodesOut(codes []string) {
	if *codesOut == "" || len(codes) == 0 {
		return
	}
	codesOutMu.Lock()
	defer codesOutMu.Unlock()
	f, err := os.OpenFile(*codesOut, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, c := range codes {
		_, _ = fmt.Fprintln(f, c)
	}
}

// ---- 用户面登录 ----

// apiUserIdentity worker 绑定身份：池内按索引确定性分配（分散 bcrypt 压力）。
func apiUserIdentity(idx int) (email, password string) {
	entry := *fillUser
	if len(fillUserPool) > 0 {
		entry = fillUserPool[idx%len(fillUserPool)]
	}
	email, password, _ = strings.Cut(entry, ":")
	return
}

func apiUserLogin(client *http.Client, email, password string) (string, error) {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequest(http.MethodPost, *addr+"/api/user/auth/login", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("login status:%d", resp.StatusCode)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		return "", fmt.Errorf("login: bad token payload")
	}
	return lr.Token, nil
}

func (w *apiWorker) relogin() {
	email, password := apiUserIdentity(w.ident)
	if tok, err := apiUserLogin(w.client, email, password); err == nil {
		w.token = tok
	}
}

// ---- 引导与循环 ----

// apiBootstrapAdmin 启动期一次性：创建压力根组（账号创建绑定它，隔离 v1 选号池）。
func apiBootstrapAdmin(client *http.Client) {
	req, err := apiReq(http.MethodPost, "/api/admin/groups",
		map[string]any{"name": fmt.Sprintf("stress-root-%d", fillProc), "visibility": "public"},
		"Bearer "+*adminToken)
	if err != nil {
		stressGID.Store(*fillGroupID)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		stressGID.Store(*fillGroupID)
		return
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var gr struct {
		ID int64 `json:"ID"`
	}
	_ = json.Unmarshal(b, &gr)
	if gr.ID > 0 {
		stressGID.Store(gr.ID)
	} else {
		stressGID.Store(*fillGroupID)
	}
}

func newAPIWorker(idx int, client *http.Client, rng *rand.Rand) *apiWorker {
	w := &apiWorker{plane: *mode, client: client, rng: rng, ident: idx}
	if *mode == "api-user" {
		email, password := apiUserIdentity(idx)
		tok, err := apiUserLogin(client, email, password)
		if err != nil {
			fmt.Fprintf(os.Stderr, "api-user worker %d login failed: %v\n", idx, err)
			w.token = "" // 循环内 401 会再触发重登
		} else {
			w.token = tok
		}
	}
	return w
}

// apiWorkerLoop 单 worker 主循环（warmup 不计数 → 计时段）。
func apiWorkerLoop(w *apiWorker, warmEnd, stop time.Time) {
	scenarios := adminScenarios()
	if *mode == "api-user" {
		scenarios = userScenarios()
	}
	if *readsOnly {
		reads := scenarios[:0]
		for _, sc := range scenarios {
			if !sc.write {
				reads = append(reads, sc)
			}
		}
		scenarios = reads
	}
	pick := func(rng *rand.Rand) *apiScenario {
		total := 0
		for i := range scenarios {
			total += scenarios[i].weight
		}
		n := rng.IntN(total)
		for i := range scenarios {
			n -= scenarios[i].weight
			if n < 0 {
				return &scenarios[i]
			}
		}
		return &scenarios[len(scenarios)-1]
	}
	for time.Now().Before(warmEnd) {
		w.runScenario(pick(w.rng), false)
	}
	for time.Now().Before(stop) {
		w.runScenario(pick(w.rng), true)
	}
}
