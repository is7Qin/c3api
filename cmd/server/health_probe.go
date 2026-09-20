// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// codexUsageProber codex 凭据类型的探测路由面（*sdkbridge.Codex 满足）：codex
// 账号的 base_url/鉴权/OAuth 刷新全部由 SDK 适配器权威管理，探测必须走同一套
// 凭据栈（fatal 权威保持——探测期 SDK 判死仍经 OnAuthFatal 统一回调链）。
type codexUsageProber interface {
	GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error)
}

// healthProber 是 RuntimeHealth 的真实 probe 适配器。身份权威 = scheduler 选号
// 快照（装配点注入 sched.ProbeAccount——选号门与探测读同一视图）：账号缺失
// （已删/未加载）与 revision 错配一律 fail-closed——stale PROBING 记录不可能
// 经错配的 probe 到达 READY。
//
// 凭据路由（未知类型显式报错，不静默 fallback）：codex 族 → SDK 适配器 usage
// 快照路径（ponytail: TTL 缓存内的成功不重新打上游——探测粒度受缓存 TTL 限制，
// 真实失败仍会经 fatal 回调重开；需要强新鲜探测时给 GetUsageSnapshot 加 bypass
// 参数）；api_key 族（api_key/responses-special）无合成探测面——GET /v1/models
// 与真实流量无关（端点级限流下 models-200 而 chat-429），已删除，探测视为通过，
// 恢复由时间窗 + 真实流量判定。
type healthProber struct {
	lookup  func(accountID int64) (*domain.Account, bool)
	codex   codexUsageProber
	timeout time.Duration
}

// newHealthProber 构造适配器并返回 scheduler.ProbeFunc 形态（装配点经
// healthWorker 在 Start 期一次性交给 RuntimeHealth）。
func newHealthProber(lookup func(accountID int64) (*domain.Account, bool), codex codexUsageProber,
	timeout time.Duration) scheduler.ProbeFunc {
	p := &healthProber{lookup: lookup, codex: codex, timeout: timeout}
	return p.probe
}

// healthWorker 适配 RuntimeHealth 启动：probe 是 Start 期依赖（组合根在
// sched/codex 就绪后构造），适配器让 worker.Manager 契约不变、Name 保持
// "runtime-health"（注册序=反序排空语义依赖）。Stats 原样透出——ops 运维面
// 不因适配掉线。
type healthWorker struct {
	h     *scheduler.RuntimeHealth
	probe scheduler.ProbeFunc
}

func (w healthWorker) Name() string                    { return w.h.Name() }
func (w healthWorker) Stats() any                      { return w.h.Stats() }
func (w healthWorker) Start(ctx context.Context) error { return w.h.Start(ctx, w.probe) }
func (w healthWorker) Close(ctx context.Context) error { return w.h.Close(ctx) }

func (p *healthProber) probe(ctx context.Context, key scheduler.HealthKey) error {
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	acct, ok := p.lookup(key.AccountID)
	if !ok || acct.IdentityRevision != key.IdentityRevision {
		// 视图缺失（已删/未同步）或**身份代际 K 错配**——fail-closed：探测失败
		// → probeTick 重开记录，stale PROBING 不可能经错配 probe 变 READY。
		// 比的是 K（identity_revision）而非客户端 CAS 令牌 C：健康记录按 K 隔离。
		return fmt.Errorf("%w: account %d not healthy-routable at identity revision %d", scheduler.ErrProbeStaleRevision, key.AccountID, key.IdentityRevision)
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
		// api_key 族无合成探测面（/v1/models 与流量无关，已删除）——探测视为通过，恢复由时间窗+真实流量判定。
		return nil
	default:
		return fmt.Errorf("health probe: account %d has unknown credential type %q", key.AccountID, tpl.CredentialType)
	}
}
