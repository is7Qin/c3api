// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// codexUsageProber codex 凭据类型的探测路由面（*sdkbridge.Codex 满足）：codex
// 账号的 base_url/鉴权/OAuth 刷新全部由 SDK 适配器权威管理，通用 models GET
// 无法覆盖，探测必须走同一套凭据栈（fatal 权威保持——探测期 SDK 判死仍经
// OnAuthFatal 统一回调链）。
type codexUsageProber interface {
	GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error)
}

// healthProber 是 RuntimeHealth 的真实 probe 适配器。身份权威 = scheduler 选号
// 快照（装配点注入 sched.ProbeAccount——选号门与探测读同一视图）：账号缺失
//（已删/未加载）与 revision 错配一律 fail-closed——stale PROBING 记录不可能
// 经错配的 probe 到达 READY。
//
// 凭据路由（未知类型显式报错，不静默 fallback）：codex 族 → SDK 适配器 usage
// 快照路径（ponytail: TTL 缓存内的成功不重新打上游——探测粒度受缓存 TTL 限制，
// 真实失败仍会经 fatal 回调重开；需要强新鲜探测时给 GetUsageSnapshot 加 bypass
// 参数）；api_key/responses-special → GET {base}/v1/models（base_url 裸根契约
// + openai 族补 /v1，aiclient 同款），anthropic 格式族 x-api-key +
// anthropic-version、其余 Bearer。仅设鉴权头，零客户端头透传；状态码即判据，
// 上游 body 不进探测错误（防内部信息外溢）。
type healthProber struct {
	lookup  func(accountID int64) (*domain.Account, bool)
	codex   codexUsageProber
	hc      *http.Client
	timeout time.Duration
}

// newHealthProber 构造适配器并返回 scheduler.ProbeFunc 形态（装配点经
// RuntimeHealth.SetProbeFn 回填，必须在 Start 之前）。
func newHealthProber(lookup func(accountID int64) (*domain.Account, bool), codex codexUsageProber,
	hc *http.Client, timeout time.Duration) scheduler.ProbeFunc {
	p := &healthProber{lookup: lookup, codex: codex, hc: hc, timeout: timeout}
	return p.probe
}

func (p *healthProber) probe(ctx context.Context, key scheduler.HealthKey) error {
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	acct, ok := p.lookup(key.AccountID)
	if !ok || acct.LifecycleRevision != key.Revision {
		// 视图缺失（已删/未同步）或 revision 错配——fail-closed：探测失败
		// → probeTick 重开记录，stale PROBING 不可能经错配 probe 变 READY。
		return fmt.Errorf("%w: account %d not healthy-routable at revision %d", scheduler.ErrProbeStaleRevision, key.AccountID, key.Revision)
	}
	tpl := acct.Template
	if tpl == nil {
		return fmt.Errorf("health probe: account %d has no template", key.AccountID)
	}
	switch tpl.CredentialType {
	case credential.TypeCodexOAuth, credential.TypeCodexPAT:
		if p.codex == nil {
			return fmt.Errorf("health probe: codex adapter not wired for account %d", key.AccountID)
		}
		cred := domain.CredentialFromExt(acct.Ext)
		cred.AccountID = key.AccountID
		if _, err := p.codex.GetUsageSnapshot(cctx, &cred); err != nil {
			return fmt.Errorf("health probe: codex usage probe failed for account %d: %w", key.AccountID, err)
		}
		return nil
	case credential.TypeAPIKey, credential.TypeResponsesSpecial:
		return p.probeModels(cctx, key, acct, tpl)
	default:
		return fmt.Errorf("health probe: account %d has unknown credential type %q", key.AccountID, tpl.CredentialType)
	}
}

// probeModels api_key 族通用探测：GET {base}/v1/models，2xx = 健康。
// 账号级 base_url 覆盖优先于模板（与 aiclient 取 base 同契约）。
func (p *healthProber) probeModels(ctx context.Context, key scheduler.HealthKey, acct *domain.Account, tpl *domain.Template) error {
	base := tpl.BaseURL
	if acct.BaseURL != nil && *acct.BaseURL != "" {
		base = *acct.BaseURL
	}
	if base == "" {
		return fmt.Errorf("health probe: account %d has no base url", key.AccountID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("health probe: account %d invalid probe url: %w", key.AccountID, err)
	}
	if isAnthropicTemplate(tpl) {
		req.Header.Set("x-api-key", acct.UpstreamKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+acct.UpstreamKey)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("health probe: account %d upstream unreachable: %w", key.AccountID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health probe: account %d upstream rejected with status %d", key.AccountID, resp.StatusCode)
	}
	return nil
}

func isAnthropicTemplate(tpl *domain.Template) bool {
	for _, f := range tpl.SupportedFormats {
		if f == domain.FormatAnthropic {
			return true
		}
	}
	return false
}
