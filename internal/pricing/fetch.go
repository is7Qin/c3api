// SPDX-License-Identifier: AGPL-3.0-or-later
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

const FetchTimeout = 30 * time.Second
const maxPriceTableBytes = 64 << 20

type Fetcher interface {
	Fetch(ctx context.Context, sourceURL string) (*FetchResult, error)
}

type FetchResult struct {
	PriceEntries []*domain.PriceEntry
	Variants     []*domain.PriceVariant
	Skipped      int
}

func NewFetcher(client *http.Client, log *logx.Logger) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client, log: log}
}

type httpFetcher struct {
	client *http.Client
	log    *logx.Logger
}

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
	InputCostPerImageToken              *float64 `json:"input_cost_per_image_token"`
	OutputCostPerImageToken             *float64 `json:"output_cost_per_image_token"`
	OutputCostPerImage                  *float64 `json:"output_cost_per_image"`
	InputCostPerQuery                   *float64 `json:"input_cost_per_query"`
}

type providerSpecificEntry struct {
	Fast *float64 `json:"fast"`
}

func Parse(data []byte, log *logx.Logger) (*FetchResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse price table: %w", err)
	}
	res := &FetchResult{
		PriceEntries: make([]*domain.PriceEntry, 0, len(raw)),
		Variants:     make([]*domain.PriceVariant, 0),
	}
	for model, entry := range raw {
		pe, vars, ok := parsePriceEntry(model, entry, log)
		if ok {
			res.PriceEntries = append(res.PriceEntries, pe)
			res.Variants = append(res.Variants, vars...)
			continue
		}
		if pe2, ok2 := parseImagePriceEntry(model, entry); ok2 {
			res.PriceEntries = append(res.PriceEntries, pe2)
			continue
		}
		if pe3, ok3 := parseFunctionPriceEntry(model, entry); ok3 {
			res.PriceEntries = append(res.PriceEntries, pe3)
			continue
		}
		res.Skipped++
	}
	return res, nil
}

func parsePriceEntry(model string, raw json.RawMessage, log *logx.Logger) (*domain.PriceEntry, []*domain.PriceVariant, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, false
	}
	if e.Mode == nil || (*e.Mode != "chat" && *e.Mode != "responses") {
		return nil, nil, false
	}
	if !validCost(e.InputCostPerToken) || !validCost(e.OutputCostPerToken) {
		return nil, nil, false
	}
	pe := &domain.PriceEntry{
		Model:  model,
		Mode:   domain.PriceModeToken,
		Source: domain.PricingSourceLitellm,
		Raw:    raw,
	}
	in := toMilliCentsPerMillion(*e.InputCostPerToken)
	pe.InputPerM = &in
	out := toMilliCentsPerMillion(*e.OutputCostPerToken)
	pe.OutputPerM = &out
	if v := cacheCost(e.CacheReadInputTokenCost); v != nil {
		pe.CacheReadPerM = v
	}
	if v := cacheCost(e.CacheCreationInputTokenCost); v != nil {
		pe.CacheWritePerM = v
	}
	if t := windowTokens(e.MaxInputTokens); t > 0 {
		pe.MaxInputTokens = &t
	}
	if t := windowTokens(e.MaxOutputTokens); t > 0 {
		pe.MaxOutputTokens = &t
	}
	pe.Provider = e.Provider
	pe.SupportsPromptCaching = e.SupportsPromptCaching
	var vars []*domain.PriceVariant
	seq := 1
	if e.InputCostPerTokenPriority != nil || e.OutputCostPerTokenPriority != nil {
		if validCost(e.InputCostPerTokenPriority) || validCost(e.OutputCostPerTokenPriority) {
			st := "priority"
			var setIn, setOut *int64
			if validCost(e.InputCostPerTokenPriority) {
				v := toMilliCentsPerMillion(*e.InputCostPerTokenPriority)
				setIn = &v
			}
			if validCost(e.OutputCostPerTokenPriority) {
				v := toMilliCentsPerMillion(*e.OutputCostPerTokenPriority)
				setOut = &v
			}
			if setIn != nil || setOut != nil {
				vars = append(vars, &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st, SetInputPerM: setIn, SetOutputPerM: setOut})
				seq++
			}
		}
	}
	if e.InputCostPerTokenFlex != nil || e.OutputCostPerTokenFlex != nil {
		if validCost(e.InputCostPerTokenFlex) || validCost(e.OutputCostPerTokenFlex) {
			st := "flex"
			var setIn, setOut *int64
			if validCost(e.InputCostPerTokenFlex) {
				v := toMilliCentsPerMillion(*e.InputCostPerTokenFlex)
				setIn = &v
			}
			if validCost(e.OutputCostPerTokenFlex) {
				v := toMilliCentsPerMillion(*e.OutputCostPerTokenFlex)
				setOut = &v
			}
			if setIn != nil || setOut != nil {
				vars = append(vars, &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st, SetInputPerM: setIn, SetOutputPerM: setOut})
				seq++
			}
		}
	}
	aboveMap := extractAboveAll(model, raw)
	type ab struct {
		n    int64
		vals [2]*int64
	}
	var abs []ab
	for n, v := range aboveMap {
		abs = append(abs, ab{n: n, vals: v})
	}
	for i := 0; i < len(abs); i++ {
		for j := i + 1; j < len(abs); j++ {
			if abs[j].n > abs[i].n {
				abs[i], abs[j] = abs[j], abs[i]
			}
		}
	}
	for _, a := range abs {
		nv := a.n * 1000
		vars = append(vars, &domain.PriceVariant{Model: model, Seq: seq, CtxMin: &nv, SetInputPerM: a.vals[0], SetOutputPerM: a.vals[1]})
		seq++
	}
	if e.ProviderSpecificEntry != nil && e.ProviderSpecificEntry.Fast != nil {
		v := e.ProviderSpecificEntry.Fast
		if !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v > 0 {
			m := int(math.Round(*v * 1e4))
			if m > 100000 {
				m = 100000
			}
			st := "fast"
			vars = append(vars, &domain.PriceVariant{Model: model, Seq: seq, ServiceTier: &st, MultBP: &m})
			seq++
		}
	}
	// warn for multi-tier above groups via original extractAbove logic (preserve log semantics for priority/flex groups)
	if log != nil {
		_ = extractAbove(model, raw, log)
	}
	return pe, vars, true
}

func extractAboveAll(model string, raw json.RawMessage) map[int64][2]*int64 {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make(map[int64][2]*int64)
	for k, v := range m {
		var n int64
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
	for n, a := range out {
		if a[0] == nil || a[1] == nil {
			delete(out, n)
		}
	}
	return out
}

func parseImagePriceEntry(model string, raw json.RawMessage) (*domain.PriceEntry, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	if e.Mode == nil || (*e.Mode != "image_generation" && *e.Mode != "image_edit") {
		return nil, false
	}
	if !validCost(e.InputCostPerImageToken) && !validCost(e.OutputCostPerImageToken) && !validCost(e.OutputCostPerImage) {
		return nil, false
	}
	pe := &domain.PriceEntry{Model: model, Mode: domain.PriceModeImage, Source: domain.PricingSourceLitellm, Raw: raw, Provider: e.Provider}
	if validCost(e.InputCostPerImageToken) {
		v := toMilliCentsPerMillion(*e.InputCostPerImageToken)
		pe.ImgInTokPerM = &v
	}
	if validCost(e.OutputCostPerImageToken) {
		v := toMilliCentsPerMillion(*e.OutputCostPerImageToken)
		pe.ImgOutTokPerM = &v
	}
	if validCost(e.OutputCostPerImage) {
		v := toMilliCentsPerImage(*e.OutputCostPerImage)
		pe.PricePerImage = &v
	}
	return pe, true
}

func parseFunctionPriceEntry(model string, raw json.RawMessage) (*domain.PriceEntry, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}
	if e.Mode == nil || *e.Mode != "search" {
		return nil, false
	}
	if !validCost(e.InputCostPerQuery) {
		return nil, false
	}
	v := toMilliCentsPerCall(*e.InputCostPerQuery)
	return &domain.PriceEntry{Model: model, Mode: domain.PriceModeCall, PricePerCall: &v, Provider: e.Provider, Source: domain.PricingSourceLitellm, Raw: raw}, true
}

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

func extractAbove(model string, raw json.RawMessage, log *logx.Logger) *aboveStep {
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
	var cols [3]map[int64][4]float64
	for key, val := range m {
		var n int64
		g, c := -1, -1
		for ci, base := range bases {
			if !strings.HasPrefix(key, base) {
				continue
			}
			rest := key[len(base):]
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
	var out aboveStep
	found := false
	var maxN int64
	setGroup := func(g int, dst *[4]**int64) {
		best, ok := int64(0), false
		var bestV [4]float64
		qualifying := 0
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

func cacheCost(v *float64) *int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return nil
	}
	m := toMilliCentsPerMillion(*v)
	return &m
}

func validCost(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v > 0
}

func toMilliCentsPerMillion(perTokenUSD float64) int64 {
	return int64(math.Round(perTokenUSD * 1e11))
}

func toMilliCentsPerImage(perImageUSD float64) int64 {
	return int64(math.Round(perImageUSD * 1e5))
}

func toMilliCentsPerCall(perQueryUSD float64) int64 {
	return int64(math.Round(perQueryUSD * 1e5))
}

func windowTokens(v *float64) int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return 0
	}
	if *v > float64(math.MaxInt64) {
		return 0
	}
	return int64(math.Round(*v))
}

// legacy symbols for compatibility during migration (removed after build green); keep stub to avoid import errors in tests that reference constants
var _ = errors.New
