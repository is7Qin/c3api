// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package sdkbridge 是 SDK 适配层与网关之间的契约面（零 SDK 依赖——SDK
// 调用从此后）：统一失效回调 + 失效处理链装配（写失效字段 / 调度摘除 /
// 审计）、网关侧信封错误与凭据传递形态（AccountCredential 派生在
// internal/domain）。
package sdkbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// FailureHandler 账号失效上报的唯一入口：SDK 适配层（此后）把 SDK 内部判死
// （OnAuthFatal / errors.As fatal 四类——RefreshOAuthError / AuthPermanentlyRevokedError /
// AccountDisabledError / CallbackDeliveryError；RefreshError 可重试，有意排除）翻译成一次统一回调；
// 网关侧只处理这一个入口。
//
// 语义：
//   - 账号级终止（凭据永久失效/上游封禁/判死）才上报；**可重试类（RefreshError
//     等）不上报**（网关按既有 failover 分类处理）
//   - **信封类错误不上报**（透传协议——网关 statusOf/upstreamErrMsg 零改动复用）
//   - 双源去重：rotationAuth 路径同一 fatal 既触发 OnAuthFatal 又随返回错误
//     errors.As 命中——**以回调为准去重、单次上报**（结构或语义级去重，
//     适配层实现；本契约只定义回调形态）
type FailureHandler func(accountID int64, fatal error)

// FailureStore 失效字段落库面（repository.AccountRepo 满足；接口化供测试注入
// 与装配侧解耦）。
type FailureStore interface {
	// SetAccountFailed 幂等写 failed_at + last_error（失效原因文本，复用既有
	// last_error——用户裁决 2026-08-13：两原因字段并存会漂移；失效后账号摘除
	// 不再被调度，普通失败写点不会覆盖失效原因，复用安全）：failed_at 已置
	//（首次上报）→ 不覆盖（保持首次失效时刻与原因；重复上报不重复写）。
	SetAccountFailed(ctx context.Context, accountID int64, failedAt time.Time, reason string) error
}

type casStore interface {
	FailAccountCAS(ctx context.Context, id int64, expectedRevision int64, source string, failedAt time.Time, reason string) error
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
}

// casStoreTemplate 可选能力：按 id 取账号并**预载模板**。失效判决的候选指纹与
// 凭据判别符都读模板（凭据类型 + 生效 origin），而部分实现的 GetAccount 只取
// 账号行（repository.AccountRepo 即如此）⇒ 缺本能力时判决无法成形。实现方若不
// 预载模板，必须实现本接口，否则失效链在围栏路径上无法工作。
type casStoreTemplate interface {
	GetAccountWithTemplate(ctx context.Context, id int64) (*domain.Account, error)
}

// ensureTemplate 补齐判决所需的模板：已预载则原样返回；否则经可选能力重取。
// 无法补齐时返回原值——后续按"缺凭据判别符"拒绝，不静默放过。
func ensureTemplate(ctx context.Context, cs casStore, acct *domain.Account, accountID int64) (*domain.Account, error) {
	if acct == nil || acct.Template != nil {
		return acct, nil
	}
	withTpl, ok := cs.(casStoreTemplate)
	if !ok {
		return acct, nil
	}
	return withTpl.GetAccountWithTemplate(ctx, accountID)
}

type groupGetter interface {
	GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error)
}

type Latcher interface {
	TryAcquire(accountID int64, fingerprint string, identityRevision int64) bool
	Clear(accountID int64)
	IsLatched(accountID int64, fingerprint string, identityRevision int64) bool
}

type GroupPublisher interface {
	PublishGroups(ctx context.Context, gids []int64)
}

// AccountFailer 调度摘除面（*scheduler.Scheduler 满足；接口化供测试注入）。
type AccountFailer interface {
	// FailAccount 快照置 StatusDisabled（运行时摘除）。持久化事实 = failed_at
	//（同链 DB 步落库；重启快照重载经 failed_at 仍摘除——恢复唯一入口
	// /recover fenced 端点）。
	FailAccount(accountID int64)
}

// FailureDeps 失效处理链依赖（main 装配：repository.Accounts + scheduler）。
type FailureDeps struct {
	Store  FailureStore
	Failer AccountFailer
	// Log 处理错误日志（评审：同一失败只记一条——记在回调侧
	// NewFailureHandler，HandleFailure 不重复记）；nil = no-op。
	Log       *logx.Logger
	Latch     Latcher
	Publisher GroupPublisher
}

// HandleFailure 网关侧失效处理链（§3——统一回调装配；适配层在
// FailureHandler 回调中调用；冷面——失败上报低频）：
//
//  1. DB 写 failed_at + last_error（失效原因文本，复用既有 last_error——用户
//     裁决 2026-08-13：两原因字段并存会漂移；幂等：重复上报不重复写，首次
//     失效时刻保持）
//  2. 调度器状态置 StatusDisabled（快照运行时摘除）——持久化事实 = failed_at
//     （第 1 步/CAS 步落库；重启快照重载经 failed_at 仍摘除）；复用既有选号
//     disabled 过滤器与 MarkResult 防复活守卫（置位后在途请求结果短路）
//  3. 失败请求自身不在此链——由 proxy 既有分类路径处理（fatal → 连接级
//     MarkResult 分流，failover 不重试同一账号；forward.go 语义，不改动）
//
// DB 写失败不阻断摘除（fail-closed：账号已判死，摘除优先；错误返回供日志）。
// 返回 DB 写错误（nil = 成功）；调度摘除为 void（快照外账号 no-op）。
// 本函数不记日志——处理错误统一由回调侧（NewFailureHandler）记一条（
// 评审：同一失败不得双条 Warn）。
var (
	ErrMissingCredentialDiscriminator = errors.New("sdkbridge: missing credential discriminator")
	ErrMissingCandidateFingerprint    = errors.New("sdkbridge: missing candidate fingerprint")
	ErrCandidateFingerprintMismatch   = errors.New("sdkbridge: candidate fingerprint mismatch")
	ErrMissingExpectedRevision        = errors.New("sdkbridge: missing expected revision")
	ErrStaleFailureRevision           = errors.New("sdkbridge: stale failure revision")
)

func isCodexCredentialType(t credential.Type) bool {
	return t == credential.TypeCodexOAuth || t == credential.TypeCodexPAT
}

func credentialTypeOf(acct *domain.Account) (credential.Type, bool) {
	if acct == nil || acct.Template == nil {
		return "", false
	}
	if acct.Template.CredentialType == "" {
		return "", false
	}
	return acct.Template.CredentialType, true
}

func canonicalFingerprint(acct *domain.Account) (string, error) {
	if acct == nil || acct.Template == nil {
		return "", fmt.Errorf("%w: missing account or template", ErrMissingCandidateFingerprint)
	}
	if _, ok := credentialTypeOf(acct); !ok {
		return "", ErrMissingCredentialDiscriminator
	}
	fp, err := domain.AccountCandidateFingerprint(acct)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMissingCandidateFingerprint, err)
	}
	return domain.CandidateFPHex(fp), nil
}

func HandleFailure(ctx context.Context, deps FailureDeps, accountID int64, fatal error) error {
	if fatal == nil {
		return nil // 防御：无错误不上报
	}
	reason := domain.TruncateErrMsg(fatal.Error())
	// Unified path with latch+revision+NOTIFY when store supports CAS and latch present.
	if deps.Latch != nil {
		if cs, ok := deps.Store.(casStore); ok {
			acct, err := cs.GetAccount(ctx, accountID)
			if err != nil {
				return err
			}
			acct, err = ensureTemplate(ctx, cs, acct, accountID)
			if err != nil {
				return err
			}
			ct, ok := credentialTypeOf(acct)
			if !ok {
				return ErrMissingCredentialDiscriminator
			}
			if !isCodexCredentialType(ct) {
				return nil
			}
			fp, ferr := canonicalFingerprint(acct)
			if ferr != nil {
				return ferr
			}
			// 失效判决的围栏维度是 K（身份代际），不是 C（配置代际）：SDK 上报
			// 的失效只在"身份未被授权变更"时成立，故取值与 CAS guard 必须同为 K。
			expectedRev := acct.IdentityRevision
			if expectedRev <= 0 {
				return ErrMissingExpectedRevision
			}
			deps.Latch.TryAcquire(accountID, fp, expectedRev)
			deps.Failer.FailAccount(accountID)
			err = cs.FailAccountCAS(ctx, accountID, expectedRev, "sdk", time.Now(), reason)
			if err != nil {
				if errors.Is(err, repository.ErrStaleIdentityRevision) {
					fresh, ferr := cs.GetAccount(ctx, accountID)
					if ferr == nil && fresh.IdentityRevision > expectedRev {
						deps.Latch.Clear(accountID)
					}
					return fmt.Errorf("%w: %v", ErrStaleFailureRevision, err)
				}
				// fencing for fingerprint mismatch is handled via ErrCandidateFingerprintMismatch at retry time;
				// transient errors -> enqueue retry for process lifetime, nonblocking.
				if isTransientFailure(err) {
					enqueueFailureRetry(deps, accountID, fp, expectedRev, reason)
				}
				return err
			}
			deps.Latch.Clear(accountID)
			if deps.Publisher != nil {
				if gg, ok := deps.Store.(groupGetter); ok {
					gids, _ := gg.GetAccountGroups(ctx, accountID)
					if len(gids) > 0 {
						deps.Publisher.PublishGroups(context.WithoutCancel(ctx), gids)
					}
				}
			}
			return nil
		}
	}
	err := deps.Store.SetAccountFailed(ctx, accountID, time.Now(), reason)
	// 摘除恒执行：DB 故障时内存摘除先生效（持久化事实 = failed_at，DB 恢复后
	// 同链重试落库；重启重载经 failed_at 收敛——fail-closed）。
	deps.Failer.FailAccount(accountID)
	return err
}

func isTransientFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, repository.ErrStaleIdentityRevision) || errors.Is(err, ErrStaleFailureRevision) || errors.Is(err, ErrCandidateFingerprintMismatch) || errors.Is(err, ErrMissingCandidateFingerprint) || errors.Is(err, ErrMissingExpectedRevision) || errors.Is(err, ErrMissingCredentialDiscriminator) {
		return false
	}
	return true
}

func NewFailureHandler(deps FailureDeps) FailureHandler {
	return func(accountID int64, fatal error) {
		if err := HandleFailure(context.Background(), deps, accountID, fatal); err != nil && deps.Log != nil {
			deps.Log.Warn("account failure handling failed", logx.Int64("account_id", accountID), logx.Error(err))
		}
	}
}
