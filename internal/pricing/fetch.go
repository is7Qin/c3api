// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package pricing 实现 litellm 模型价格同步：官方价格表拉取（fetcher）+ 定期
// 同步 worker（gronx cron 调度）。同步链路：fetch → UpsertFromLiteLLM（仓库
// 500/批、manual 行级互斥）→ service 快照 Reload。失败保留旧价格，下个周期
// 重试（不重试风暴）。
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// FetchTimeout 单次拉取 HTTP 超时。
const FetchTimeout = 30 * time.Second

// maxPriceTableBytes 拉取体上限（官方表 ~3MB，64MB 防御上限防异常源打爆内存）。
const maxPriceTableBytes = 64 << 20

// Fetcher 拉取 litellm 官方价格表（GitHub raw JSON，URL 由 settings 配置可换）。
type Fetcher interface {
	Fetch(ctx context.Context, sourceURL string) (*FetchResult, error)
}

// FetchResult 一次拉取的解析结果。
type FetchResult struct {
	Rows         []*domain.Pricing       // 数值有效的模型价格行（source=litellm；有文本价）
	ImageRows    []*domain.ImagePrice    // image 价行（source=litellm；任一 image 价分量有效）
	FunctionRows []*domain.FunctionPrice // 按单元计费功能类价行（source=litellm；search mode 且带 per-query 价）
	PriceEntries []*domain.PriceEntry    // 新统一价表行
	Variants     []*domain.PriceVariant  // 关联变体（平铺）
	// Skipped 无效条目跳过数（字段类型非法 / 文本价、image 价与 function 价
	// 均无效——判定含 image/function 分量；同一条目三线独立判定，互不干扰，
	// 只产生任一行即不计 skip）。
	Skipped int
}

// NewFetcher 构造 HTTP fetcher（client 复用调用方连接池；nil → http.DefaultClient；
// log nil 可用（静默，与 SyncWorkerConfig.Log 同款约定）；超时由 Fetch 内
// context 保证，独立于 client.Timeout）。
func NewFetcher(client *http.Client, log *logx.Logger) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client, log: log}
}

type httpFetcher struct {
	client *http.Client
	log    *logx.Logger // A-P2-12 方案 A 可观测化：多档位 Warn 目标；nil = 静默
}

// Fetch GET 拉取并解析。状态非 200 / 体超限 / JSON 顶层解析失败 → 错误；单行
// 非法（字段类型错/缺价/NaN/非正数）→ 跳过该模型，不影响其余行。
func (f *httpFetcher) Fetch(ctx context.Context, sourceURL string) (*FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch %s: status %d", sourceURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPriceTableBytes))
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: read body: %w", sourceURL, err)
	}
	return Parse(data, f.log)
}

// litellmEntry litellm 价格表单模型行的目标字段（input/output_cost_per_token 与
// cache_read/creation_input_token_cost 为 USD/token；max_*_tokens 为上下文窗口；
// provider/mode/supports_prompt_caching 为显式元数据；其余字段（字符/音频价格、
// rpm/supports_vision 等）仅经 raw 完整镜像保留）。指针字段：null/缺失 → nil。
// priority/flex 单价档（service_tier 替换档，Phase 5）：litellm 官方表对应
// *_cost_per_token_{priority,flex} 字段；ProviderSpecificEntry.Fast 为
// Anthropic Fast Mode 倍率（provider_specific_entry.fast，倍率语义）。
// above 阶梯（_above_{N}k_tokens，N 任意）不在此 struct——键名动态，走
// extractAbove 扫描 raw map（见下）。
type litellmEntry struct {
	InputCostPerToken                   *float64               `json:"input_cost_per_token"`
	OutputCostPerToken                  *float64               `json:"output_cost_per_token"`
	CacheReadInputTokenCost             *float64               `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost         *float64               `json:"cache_creation_input_token_cost"`
	InputCostPerTokenPriority           *float64               `json:"input_cost_per_token_priority"`
	OutputCostPerTokenPriority          *float64               `json:"output_cost_per_token_priority"`
	CacheReadInputTokenCostPriority     *float64               `json:"cache_read_input_token_cost_priority"`
	CacheCreationInputTokenCostPriority *float64               `json:"cache_creation_input_token_cost_priority"`
	InputCostPerTokenFlex               *float64               `json:"input_cost_per_token_flex"`
	OutputCostPerTokenFlex              *float64               `json:"output_cost_per_token_flex"`
	CacheReadInputTokenCostFlex         *float64               `json:"cache_read_input_token_cost_flex"`
	CacheCreationInputTokenCostFlex     *float64               `json:"cache_creation_input_token_cost_flex"`
	ProviderSpecificEntry               *providerSpecificEntry `json:"provider_specific_entry"`
	MaxInputTokens                      *float64               `json:"max_input_tokens"`
	MaxOutputTokens                     *float64               `json:"max_output_tokens"`
	Provider                            *string                `json:"litellm_provider"`
	Mode                                *string                `json:"mode"`
	SupportsPromptCaching               *bool                  `json:"supports_prompt_caching"`
	// image 价字段（Task A 扩展）：input/output_cost_per_image_token 为
	// USD/token（×1e11 → 毫分/1M）；output_cost_per_image 为 USD/张（×1e5 →
	// 毫分/张——与 token 价不同换算系、不同单位，计费不走 /1e6 除法）。
	// image 价独立判定：任一字段有效 → 产出 image_price 行，不碰 pricings
	// 行有效性（gpt-image-2 形态只有 image 价无文本价）。
	InputCostPerImageToken  *float64 `json:"input_cost_per_image_token"`
	OutputCostPerImageToken *float64 `json:"output_cost_per_image_token"`
	OutputCostPerImage      *float64 `json:"output_cost_per_image"`
	// 按单元计费功能类价字段（Task 价格表三件套）：input_cost_per_query 为
	// USD/query（×1e5 → 毫分/次——与 token 价不同换算系、不同单位，计费不走
	// /1e6 除法；search 计费每次查询按次计价）。
	InputCostPerQuery *float64 `json:"input_cost_per_query"`
}

// providerSpecificEntry litellm provider_specific_entry（供应商专有倍率）。
// Fast = Anthropic Fast Mode 倍率（如 claude-opus-5 2.0、claude-opus-4-6 6.0）；
// us 区域倍率等其余字段不提取（raw 已完整镜像，后置）。
type providerSpecificEntry struct {
	Fast *float64 `json:"fast"`
}

// Parse 解析 litellm 价格表 JSON：顶层 map model_name → 行。换算：per-token USD
// × 1e11 四舍五入取整 → 毫分/1M tokens（1 USD = 100,000 毫分 = 10⁻⁵ USD 精度；
// ×1e6 tokens × 1e5 毫分）；per-image USD × 1e5 → 毫分/张（toMilliCentsPerImage，
// 与 token 价不同换算系）；per-query USD × 1e5 → 毫分/次（toMilliCentsPerCall，
// 与 token 价不同换算系）。
// **mode 分流（用户裁决 2026-08-13，按消费面收）**：
//   - mode ∈ {"chat", "responses"} → 仅入 pricings（responses 端点计费走
//     GetPrice；completion 不收——网关无 /v1/completions 消费端点，宁漏勿错 +
//     手动设价可补；其余 mode（embedding/realtime/audio/moderation/rerank/ocr/
//     video/vector_store）不收——无消费面，未来端点加时按消费面扩展）
//   - mode ∈ {"image_generation", "image_edit"} → 仅入 image_price（edits
//     端点计费走 GetImagePrice）
//   - mode == "search" → 仅入 function_price（parseFunctionEntry 判定）
//   - 其余 mode 与 mode 缺失三表都不收（宁漏勿错，手动设价可补）
//
// 三表按 mode 互斥（同一 mode 只命中一表；litellm 实测 entry 单 mode 单一）。
// mode 命中后文本价、image 价与 function 价独立判定：同一条目只产出命中表的
// 行，互不干扰；三类均无效（或字段类型非法）→ 整条目跳过（Skipped++，计一次）。
// 只保留数值有效行：价格存在、有限且 > 0（NaN/缺失/非正数 → 该分量无效）；
// max_tokens/cache 价/元数据非法或缺失 → nil（unknown），不参与有效性判定；
// raw 对通过判定的行无条件保存（整个原始条目 JSON 完整镜像，含未映射字段，
// 如 rpm/supports_vision）。
// log：A-P2-12 方案 A 可观测化告警（多档位当前按基础价计费）目标；nil 可用
// （静默——测试/无装配场景不告警，与 SyncWorkerConfig.Log 同款约定）。
func Parse(data []byte, log *logx.Logger) (*FetchResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse price table: %w", err)
	}
	res := &FetchResult{
		Rows:         make([]*domain.Pricing, 0, len(raw)),
		ImageRows:    make([]*domain.ImagePrice, 0, len(raw)),
		FunctionRows: make([]*domain.FunctionPrice, 0, len(raw)),
	}
	for model, entry := range raw {
		valid := false
		if p, err := parseEntry(model, entry, log); err == nil {
			res.Rows = append(res.Rows, p)
			pe, vars := convertPricingToEntry(p, entry)
			res.PriceEntries = append(res.PriceEntries, pe)
			res.Variants = append(res.Variants, vars...)
			valid = true
		}
		if img, ok := parseImageEntry(model, entry); ok {
			res.ImageRows = append(res.ImageRows, img)
			pe := &domain.PriceEntry{Model: model, Mode: domain.PriceModeImage, ImgInTokPerM: img.InputImageTokenPricePerMillion, ImgOutTokPerM: img.OutputImageTokenPricePerMillion, PricePerImage: img.OutputCostPerImageMilli, Provider: img.Provider, Raw: img.Raw, Source: img.Source}
			res.PriceEntries = append(res.PriceEntries, pe)
			valid = true
		}
		if fn, ok := parseFunctionEntry(model, entry); ok {
			res.FunctionRows = append(res.FunctionRows, fn)
			pe := &domain.PriceEntry{Model: model, Mode: domain.PriceModeCall, PricePerCall: fn.PricePerCall, Provider: fn.Provider, Raw: fn.Raw, Source: fn.Source}
			res.PriceEntries = append(res.PriceEntries, pe)
			valid = true
		}
		if !valid {
			res.Skipped++
		}
	}
	return res, nil
}

// convertPricingToEntry maps old Pricing matrix to unified entry+variants.
// Mapping: priority_* → variant service_tier=priority; flex_*→flex;
// above_{N}k→ctx_min=N descending full-preservation — C5 fixed;
// fast_multiplier→variant service_tier=fast × mult_bp
// NOTE (momus caveat): cache-above expressiveness tradeoff — original above group
// carried per-component cache prices, but variant model aggregates to row-level mult/set;
// per-component cache-above fidelity is reduced (acceptable per spec — cache分量只受mult影响).
func convertPricingToEntry(p *domain.Pricing, raw json.RawMessage) (*domain.PriceEntry, []*domain.PriceVariant) {
	base := &domain.PriceEntry{
		Model: p.Model, Mode: domain.PriceModeToken,
		InputPerM: &p.PromptPricePerMillion, OutputPerM: &p.CompletionPricePerMillion,
		CacheReadPerM: p.CacheReadPricePerMillion, CacheWritePerM: p.CacheCreationPricePerMillion,
		Provider: p.Provider, Raw: p.Raw, Source: p.Source,
	}
	var vars []*domain.PriceVariant
	seq := 1
	if p.PriorityPromptPricePerMillion != nil || p.PriorityCompletionPricePerMillion != nil {
		st := "priority"
		vars = append(vars, &domain.PriceVariant{Model: p.Model, Seq: seq, ServiceTier: &st, SetInputPerM: p.PriorityPromptPricePerMillion, SetOutputPerM: p.PriorityCompletionPricePerMillion})
		seq++
	}
	if p.FlexPromptPricePerMillion != nil || p.FlexCompletionPricePerMillion != nil {
		st := "flex"
		vars = append(vars, &domain.PriceVariant{Model: p.Model, Seq: seq, ServiceTier: &st, SetInputPerM: p.FlexPromptPricePerMillion, SetOutputPerM: p.FlexCompletionPricePerMillion})
		seq++
	}
	// above thresholds: parse raw for all N and create ctx_min variants sorted descending
	aboveMap := extractAboveAll(p.Model, raw)
	// sort Ns descending for full-preservation: larger thresholds later? Actually first-match-by-seq: smaller ctx_min first would hit always; need descending so larger threshold checked first? Spec descending.
	// We'll order descending so higher threshold variants have lower seq.
	type ab struct {
		n    int64
		vals [2]*int64
	}
	var abs []ab
	for n, v := range aboveMap {
		abs = append(abs, ab{n: n, vals: v})
	}
	// sort descending
	for i := 0; i < len(abs); i++ {
		for j := i + 1; j < len(abs); j++ {
			if abs[j].n > abs[i].n {
				abs[i], abs[j] = abs[j], abs[i]
			}
		}
	}
	for _, a := range abs {
		nv := a.n * 1000
		vars = append(vars, &domain.PriceVariant{Model: p.Model, Seq: seq, CtxMin: &nv, SetInputPerM: a.vals[0], SetOutputPerM: a.vals[1]})
		seq++
	}
	if p.FastMultiplier != nil {
		st := "fast"
		m := int(*p.FastMultiplier)
		vars = append(vars, &domain.PriceVariant{Model: p.Model, Seq: seq, ServiceTier: &st, MultBP: &m})
		seq++
	}
	return base, vars
}

func extractAboveAll(model string, raw json.RawMessage) map[int64][2]*int64 {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make(map[int64][2]*int64)
	for k, v := range m {
		var n int64
		// match input_cost_per_token_above_{N}k_tokens
		var prefix string
		var idx int
		if strings.HasPrefix(k, "input_cost_per_token_above_") && strings.HasSuffix(k, "k_tokens") {
			prefix = "input_cost_per_token_above_"
			idx = 0
		} else if strings.HasPrefix(k, "output_cost_per_token_above_") && strings.HasSuffix(k, "k_tokens") {
			prefix = "output_cost_per_token_above_"
			idx = 1
		} else {
			continue
		}
		rest := k[len(prefix) : len(k)-len("k_tokens")]
		fmt.Sscanf(rest, "%d", &n)
		var f float64
		if json.Unmarshal(v, &f) != nil || f <= 0 {
			continue
		}
		milli := toMilliCentsPerMillion(f)
		arr := out[n]
		arr[idx] = &milli
		out[n] = arr
	}
	// keep only complete pairs
	for n, a := range out {
		if a[0] == nil || a[1] == nil {
			delete(out, n)
		}
	}
	return out
}

func parseEntry(model string, raw json.RawMessage, log *logx.Logger) (*domain.Pricing, error) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err // 字段类型非法（如价格为字符串）→ 整行跳过
	}
	// 用户裁决 2026-08-13（按消费面收）：mode ∈ {"chat", "responses"} 入
	// pricings——chat 与 responses 端点计费均走 GetPrice；completion 不收
	// （网关无 /v1/completions 消费端点，宁漏勿错 + 手动设价可补）；其余 mode
	// （embedding/realtime/audio/moderation/rerank/ocr/video/vector_store）不收
	// （无消费面——未来端点加时按消费面扩展）；mode 缺失 → 跳过（宁漏勿错，
	// 手动设价可补）。image_generation/image_edit 模型仅入 image_price
	// （parseImageEntry 判定），search 模型仅入 function_price
	// （parseFunctionEntry 判定），三表按 mode 互斥。
	if e.Mode == nil || (*e.Mode != "chat" && *e.Mode != "responses") {
		return nil, errors.New("pricing: non-chat/responses mode or missing mode")
	}
	if !validCost(e.InputCostPerToken) || !validCost(e.OutputCostPerToken) {
		return nil, errors.New("pricing: missing or non-positive cost")
	}
	p := &domain.Pricing{
		Model:                     model,
		PromptPricePerMillion:     toMilliCentsPerMillion(*e.InputCostPerToken),
		CompletionPricePerMillion: toMilliCentsPerMillion(*e.OutputCostPerToken),
		Source:                    domain.PricingSourceLitellm,
		// raw 完整镜像：整个原始条目原样保存（含未映射字段；manual 行恒为 nil）。
		Raw: raw,
	}
	if t := windowTokens(e.MaxInputTokens); t > 0 {
		p.MaxInputTokens = &t
	}
	if t := windowTokens(e.MaxOutputTokens); t > 0 {
		p.MaxOutputTokens = &t
	}
	// cache 价与 litellm 表一致可缺失/为 0（很多模型无缓存价）→ nil；有效（非
	// nil、有限、> 0）→ ×1e11 毫分取指针。nil 不参与行有效性判定。
	if v := cacheCost(e.CacheReadInputTokenCost); v != nil {
		p.CacheReadPricePerMillion = v
	}
	if v := cacheCost(e.CacheCreationInputTokenCost); v != nil {
		p.CacheCreationPricePerMillion = v
	}
	// 矩阵价（Phase 5）：priority/flex 单价档 + fast 倍率——有效价（非 nil、
	// 有限、> 0）→ 毫分/万分数取指针；缺失 = nil（计费回退基础价/不涨价），
	// 不参与行有效性判定。
	assign := func(dst **int64, v *float64, scale float64) {
		if v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v > 0 {
			m := int64(math.Round(*v * scale))
			*dst = &m
		}
	}
	assign(&p.PriorityPromptPricePerMillion, e.InputCostPerTokenPriority, 1e11)
	assign(&p.PriorityCompletionPricePerMillion, e.OutputCostPerTokenPriority, 1e11)
	assign(&p.PriorityCacheReadPricePerMillion, e.CacheReadInputTokenCostPriority, 1e11)
	assign(&p.PriorityCacheCreationPricePerMillion, e.CacheCreationInputTokenCostPriority, 1e11)
	assign(&p.FlexPromptPricePerMillion, e.InputCostPerTokenFlex, 1e11)
	assign(&p.FlexCompletionPricePerMillion, e.OutputCostPerTokenFlex, 1e11)
	assign(&p.FlexCacheReadPricePerMillion, e.CacheReadInputTokenCostFlex, 1e11)
	assign(&p.FlexCacheCreationPricePerMillion, e.CacheCreationInputTokenCostFlex, 1e11)
	if e.ProviderSpecificEntry != nil {
		assign(&p.FastMultiplier, e.ProviderSpecificEntry.Fast, 1e4) // 倍率 → 万分数
		// A-P2-4 双保险（防线前移）：钳制而非拒绝（拒绝致该模型整条无价全 402）；
		// 与 billing.Cost fast 分支同值钳制，防超界万分数（仅可经手动 DB 或异常
		// 源进入）再入快照。钳后溢出预算（cost.go 注释）先决条件恒成立。
		if p.FastMultiplier != nil && *p.FastMultiplier > 100000 {
			*p.FastMultiplier = 100000
		}
	}
	// above 阶梯（三组：基础/priority/flex）：扫描 raw key 锚定精确名
	// *_above_{N}k_tokens（+ 可选 _priority/_flex 后缀），N 任意动态提取；
	// 每组取含完整 prompt+completion 价的最大 N；缺失 = nil。
	if a := extractAbove(model, raw, log); a != nil {
		p.AboveThreshold = &a.threshold
		p.AbovePromptPricePerMillion = a.prompt
		p.AboveCompletionPricePerMillion = a.completion
		p.AboveCacheReadPricePerMillion = a.cacheRead
		p.AboveCacheCreationPricePerMillion = a.cacheCreation
		p.AbovePriorityPromptPricePerMillion = a.priorityPrompt
		p.AbovePriorityCompletionPricePerMillion = a.priorityCompletion
		p.AbovePriorityCacheReadPricePerMillion = a.priorityCacheRead
		p.AbovePriorityCacheCreationPricePerMillion = a.priorityCacheCreation
		p.AboveFlexPromptPricePerMillion = a.flexPrompt
		p.AboveFlexCompletionPricePerMillion = a.flexCompletion
		p.AboveFlexCacheReadPricePerMillion = a.flexCacheRead
		p.AboveFlexCacheCreationPricePerMillion = a.flexCacheCreation
	}
	// 显式元数据：缺失/非法 → nil（不影响行有效性；raw 已完整保留）。
	p.Provider = e.Provider
	p.Mode = e.Mode
	p.SupportsPromptCaching = e.SupportsPromptCaching
	return p, nil
}

// aboveStep above 阶梯提取结果（三组 × 4 分量 + 共享阈值）。
type aboveStep struct {
	threshold             int64
	prompt                *int64
	completion            *int64
	cacheRead             *int64
	cacheCreation         *int64
	priorityPrompt        *int64
	priorityCompletion    *int64
	priorityCacheRead     *int64
	priorityCacheCreation *int64
	flexPrompt            *int64
	flexCompletion        *int64
	flexCacheRead         *int64
	flexCacheCreation     *int64
}

// extractAbove 扫描 raw 提取 above 阶梯（三组：基础/priority/flex × 4 分量）。
// 只匹配精确 key：4 类分量前缀 + "_above_{N}k_tokens"（+ 可选 "_priority"/
// "_flex" 后缀）——character/audio/image 阶梯（*_cost_per_character_above_*
// 等）与缓存 TTL 档（above_1hr）不匹配，天然排除。N 任意（128k/200k/256k/
// 272k/512k/未来新档均识别），阈值 = N×1000。每组取含完整 prompt+completion
// 价的最大 N；共享阈值 = 各组选中 N 的最大值（实测同模型各组 N 一致）；组内
// 缺失分量 = nil（如 azure 无 cache_creation above_priority）。全无匹配 → nil。
// **A-P2-12 方案 A（可观测化）**：同组出现多个合格档位（多档位并存）时低档
// 被静默丢弃 → 低档 token 按基础价计费（少收）。不改变提取逻辑（不动数据
// 模型——完整多档模型方案 B 单独立 spec 演进），仅检测到即 log.Warn；log nil
// 可用（静默）。model 仅作 Warn 字段（定位具体模型）。
func extractAbove(model string, raw json.RawMessage, log *logx.Logger) *aboveStep {
	// 组名（Warn 字段用，与计费 tier 语义一致）。
	groupNames := [3]string{"base", "priority", "flex"}
	var bases = [4]string{
		"input_cost_per_token_above_",
		"output_cost_per_token_above_",
		"cache_read_input_token_cost_above_",
		"cache_creation_input_token_cost_above_",
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	// cols[组][档位] = 4 分量价（USD/token）；0 哨兵 = 缺失（validCost 过滤后
	// 价恒 > 0）。
	var cols [3]map[int64][4]float64
	for key, val := range m {
		var n int64
		g, c := -1, -1
		for ci, base := range bases {
			if !strings.HasPrefix(key, base) {
				continue
			}
			rest := key[len(base):] // "<N>k_tokens[+后缀]"
			i := 0
			for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
				n = n*10 + int64(rest[i]-'0')
				i++
			}
			if i == 0 || i >= len(rest) || rest[i] != 'k' {
				break
			}
			switch rest[i+1:] {
			case "_tokens":
				g = 0
			case "_tokens_priority":
				g = 1
			case "_tokens_flex":
				g = 2
			default:
				break
			}
			c = ci
			break
		}
		if g < 0 || n == 0 {
			continue
		}
		var v float64
		if json.Unmarshal(val, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			continue
		}
		if cols[g] == nil {
			cols[g] = make(map[int64][4]float64)
		}
		s := cols[g][n]
		s[c] = v
		cols[g][n] = s
	}
	// 每组取含完整 prompt+completion（分量 0/1）的最大 N。
	var out aboveStep
	found := false
	var maxN int64
	setGroup := func(g int, dst *[4]**int64) {
		best, ok := int64(0), false
		var bestV [4]float64
		qualifying := 0 // 含完整 prompt+completion 的合格档位数（>1 = 低档被丢弃）
		for n, s := range cols[g] {
			if s[0] == 0 || s[1] == 0 {
				continue
			}
			qualifying++
			if n > best {
				best, bestV, ok = n, s, true
			}
		}
		if !ok {
			return
		}
		// A-P2-12 方案 A（多档位当前按基础价计费，仅可观测化）：同组多个合格
		// 档位并存 → 低档静默丢弃（低档 token 按基础价计费少收）。仅告警不改
		// 计费行为；完整多档模型（方案 B）单独立 spec 演进。官方表实测恒单档
		// 不触发，此 Warn 为前瞻性容量缺陷的前置哨兵。
		if qualifying > 1 && log != nil {
			log.Warn("pricing: multi-tier above pricing dropped, lower tiers billed at base price",
				logx.String("model", model), logx.String("group", groupNames[g]),
				logx.Int64("kept_tier_tokens", best*1000))
		}
		for c := 0; c < 4; c++ {
			if bestV[c] != 0 {
				m := toMilliCentsPerMillion(bestV[c])
				*dst[c] = &m
			}
		}
		if best > maxN {
			maxN = best
		}
		found = true
	}
	setGroup(0, &[4]**int64{&out.prompt, &out.completion, &out.cacheRead, &out.cacheCreation})
	setGroup(1, &[4]**int64{&out.priorityPrompt, &out.priorityCompletion, &out.priorityCacheRead, &out.priorityCacheCreation})
	setGroup(2, &[4]**int64{&out.flexPrompt, &out.flexCompletion, &out.flexCacheRead, &out.flexCacheCreation})
	if !found {
		return nil
	}
	out.threshold = maxN * 1000
	return &out
}

// cacheCost 缓存价换算：缺失/非正/NaN/Inf → nil；有效 → 毫分/1M tokens 指针。
func cacheCost(v *float64) *int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return nil
	}
	m := toMilliCentsPerMillion(*v)
	return &m
}

// validCost 价格有效性：存在、有限、正数（litellm 表含 0/负/缺失占位行）。
func validCost(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v > 0
}

// toMilliCentsPerMillion 毫分/1M tokens = per-token USD × 1e11（浮点运算后四舍
// 五入取整；覆盖 litellm 全价格区间至 $0.00001/1M）。
func toMilliCentsPerMillion(perTokenUSD float64) int64 {
	return int64(math.Round(perTokenUSD * 1e11))
}

// parseImageEntry 独立解析 image 价行（用户裁决 2026-08-13：**按消费面切分**——
// mode ∈ {"image_generation", "image_edit"} 的条目进入 image_price 判定
// （image_edit 为 edits 端点消费面，计费走 GetImagePrice——2026-08-12 只收
// image_generation 漏收 edits 模型 31 个 → 402 误拒，本次修正）；chat/
// embedding/audio 等非生图模型即使带 *_cost_per_image_token 也是多模态视觉
// token 价，非生图价，一律不收——防 gpt-4o 等混入 image_price 表误放行 images
// 402 预检；mode 缺失 → 跳过（宁漏勿错，手动设价可补））。mode 命中后：任一
// image 价分量有效（存在、有限、> 0——validCost 同款）→ 产出 image_price 行；
// 全无效 → ok=false（不碰 pricings 行有效性）。换算：token 价 ×1e11
// （toMilliCentsPerMillion 复用，毫分/1M image tokens）；per-image ×1e5
// （toMilliCentsPerImage，毫分/张——禁混用 toMilliCentsPerMillion，错 6 个
// 数量级）。raw 完整镜像原样保存。
func parseImageEntry(model string, raw json.RawMessage) (*domain.ImagePrice, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false // 字段类型非法 → 整行无效（与 parseEntry 同语义）
	}
	if e.Mode == nil || (*e.Mode != "image_generation" && *e.Mode != "image_edit") {
		return nil, false // 非生图/编辑模型、缺失 → 跳过（宁漏勿错）
	}
	if !validCost(e.InputCostPerImageToken) && !validCost(e.OutputCostPerImageToken) &&
		!validCost(e.OutputCostPerImage) {
		return nil, false // 全 nil/非正/NaN → 无有效 image 价，不产出行
	}
	p := &domain.ImagePrice{
		Model:  model,
		Source: domain.PricingSourceLitellm,
		Raw:    raw,
	}
	if validCost(e.InputCostPerImageToken) {
		v := toMilliCentsPerMillion(*e.InputCostPerImageToken)
		p.InputImageTokenPricePerMillion = &v
	}
	if validCost(e.OutputCostPerImageToken) {
		v := toMilliCentsPerMillion(*e.OutputCostPerImageToken)
		p.OutputImageTokenPricePerMillion = &v
	}
	if validCost(e.OutputCostPerImage) {
		v := toMilliCentsPerImage(*e.OutputCostPerImage)
		p.OutputCostPerImageMilli = &v
	}
	// 显式元数据：缺失/非法 → nil（不影响行有效性；raw 已完整保留）。
	p.Provider = e.Provider
	return p, true
}

// parseFunctionEntry 独立解析按单元计费功能类价行（用户裁决 2026-08-13：**按
// 消费面切分**——mode == "search" 的条目进入 function_price 判定；其余 mode
// 不收（audio/video 等未来 per-unit 端点加时按各自 mode 扩展一行——表结构已
// 兼容）；mode 缺失 → 跳过（宁漏勿错，手动设价可补））。mode 命中后：
// input_cost_per_query 有效（存在、有限、> 0——validCost 同款）→ 产出
// function_price 行（PricePerCall = USD/次 ×1e5 毫分/次，toMilliCentsPerCall）；
// 无 per-query 价或非正 → 无效跳过（宁漏勿错——litellm search 模型 18 个中
// 仅带 query 价的收）。不碰 pricings/image_price 行有效性。raw 完整镜像原样保存。
func parseFunctionEntry(model string, raw json.RawMessage) (*domain.FunctionPrice, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false // 字段类型非法 → 整行无效（与 parseEntry 同语义）
	}
	if e.Mode == nil || *e.Mode != "search" {
		return nil, false // 非 search 模式/缺失 → 跳过（宁漏勿错）
	}
	if !validCost(e.InputCostPerQuery) {
		return nil, false // 无 per-query 价或非正 → 跳过（宁漏勿错）
	}
	v := toMilliCentsPerCall(*e.InputCostPerQuery)
	return &domain.FunctionPrice{
		Model:        model,
		PricePerCall: &v,
		Provider:     e.Provider, // litellm_provider（litellm 行才有；manual 行 nil）
		Source:       domain.PricingSourceLitellm,
		// raw 完整镜像：整个原始条目原样保存（含未映射字段；manual 行恒为 nil）。
		Raw: raw,
	}, true
}

// toMilliCentsPerImage 毫分/张 = per-image USD × 1e5（浮点运算后四舍五入取整；
// 1 USD = 100,000 毫分——与 token 价的 ×1e11 不同换算系：per-image 按张 flat，
// 计费（billing.ImageCost）不走 /1e6 除法，禁止与 toMilliCentsPerMillion 混用）。
func toMilliCentsPerImage(perImageUSD float64) int64 {
	return int64(math.Round(perImageUSD * 1e5))
}

// toMilliCentsPerCall 毫分/次 = per-query USD × 1e5（浮点运算后四舍五入取整；
// 1 USD = 100,000 毫分——与 token 价的 ×1e11 不同换算系：per-call 按次 flat，
// 计费（后续 search 计费面）不走 /1e6 除法，禁止与 toMilliCentsPerMillion 混用）。
func toMilliCentsPerCall(perQueryUSD float64) int64 {
	return int64(math.Round(perQueryUSD * 1e5))
}

// windowTokens 上下文窗口：非法/非正 → 0（调用方转 nil）。
func windowTokens(v *float64) int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return 0
	}
	if *v > float64(math.MaxInt64) {
		return 0
	}
	return int64(math.Round(*v))
}
