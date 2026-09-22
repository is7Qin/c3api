// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// configRevisionStore 是"镜像真实 guard"的替身：CAS 只围栏身份代际 K，并以
// **相对自增**推进 C——与 repository.FailAccountCAS 同语义。它记录收到的
// expected 值，以便断言调用方传的是哪一代。
type configRevisionStore struct {
	acct      *domain.Account
	gotExpect []int64
	casErr    error
	// raceIdentityAdvance 在 CAS 之前先推进身份（模拟"判决形成后、CAS 之前
	// 管理面完成了一次身份写入"的竞态窗口），使 guard 必然陈旧。
	raceIdentityAdvance bool
	// templateOnDemand 非 nil 时实现 GetAccountWithTemplate（按需预载模板的
	// 可选能力）；为 nil 则模拟"GetAccount 只取账号行"的实现。
	templateOnDemand *domain.Template
}

func (s *configRevisionStore) GetAccount(context.Context, int64) (*domain.Account, error) {
	cp := *s.acct
	return &cp, nil
}

func (s *configRevisionStore) GetAccountWithTemplate(context.Context, int64) (*domain.Account, error) {
	cp := *s.acct
	cp.Template = s.templateOnDemand
	return &cp, nil
}

func (s *configRevisionStore) SetAccountFailed(context.Context, int64, time.Time, string) error {
	return nil
}

func (s *configRevisionStore) FailAccountCAS(_ context.Context, _ int64, expected int64, _ string, failedAt time.Time, _ string) error {
	s.gotExpect = append(s.gotExpect, expected)
	if s.raceIdentityAdvance {
		s.acct.IdentityRevision++
	}
	if s.casErr != nil {
		return s.casErr
	}
	if s.acct.IdentityRevision != expected {
		return repository.ErrStaleIdentityRevision
	}
	s.acct.LifecycleRevision++
	s.acct.FailedAt = &failedAt
	return nil
}

func (s *configRevisionStore) GetAccountGroups(context.Context, int64) ([]int64, error) {
	return nil, nil
}

// TestHandleFailureFencesOnIdentityGenerationNotConfigRevision 是**回归用例**：
// 失效判决的围栏维度必须是身份代际 K，不是配置代际 C。C 与 K 独立推进，故
// "刚改过配置、身份未变"是常态（C ≠ K）——若判决携带 C，guard 会认为陈旧、
// 判决被丢弃且账号继续服务（fail-open），而这在 C == K 的新账号上**不可观测**，
// 正是该缺陷能长期潜伏的原因。
func TestHandleFailureFencesOnIdentityGenerationNotConfigRevision(t *testing.T) {
	acct := newCodexAccountForRetry(7, 3) // K = 3
	acct.LifecycleRevision = 9            // C = 9：配置改过多次，身份未动
	store := &configRevisionStore{acct: acct}
	latch := newRetryFakeLatch()
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch}

	require.NoError(t, HandleFailure(context.Background(), deps, 7, errors.New("fatal")))
	require.Equal(t, []int64{3}, store.gotExpect, "the verdict must carry the identity generation K, never the config generation C")
	require.NotNil(t, store.acct.FailedAt, "a fatal failure must persist while the identity is unchanged")
	require.Equal(t, int64(10), store.acct.LifecycleRevision, "the failure write advances C")
	require.Equal(t, int64(3), store.acct.IdentityRevision, "the failure write must not advance K")
}

// TestHandleFailureVoidedWhenIdentityMovedSinceObservation 反向用例：判决形成
// （读到 K）之后、CAS 落库之前若发生了管理面身份写入，guard 必须拒绝该判决并把它
// 报告为陈旧（fail-closed），且清掉陈旧锁存让新身份可被选号——旧凭据下的判死
// 不得钉住新凭据。
func TestHandleFailureVoidedWhenIdentityMovedSinceObservation(t *testing.T) {
	acct := newCodexAccountForRetry(7, 4)
	store := &configRevisionStore{acct: acct, raceIdentityAdvance: true}
	latch := newRetryFakeLatch()
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch}

	err := HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
	require.ErrorIs(t, err, ErrStaleFailureRevision, "a verdict whose K moved before the CAS must be reported stale")
	require.Nil(t, store.acct.FailedAt, "a voided verdict must not disable the account")
	require.Empty(t, latch.m, "the stale latch must be cleared so the new identity is selectable")
}

// TestSDKRefreshDoesNotVoidFailureVerdict 钉住 K 中性的那一侧：SDK 自动刷新
// （WriteOAuthRotation，不触 accounts 行）不改 K，故它之后同一账号的失效判决
// 仍然有效——"刷新令牌"不是身份写入。
func TestSDKRefreshDoesNotVoidFailureVerdict(t *testing.T) {
	acct := newCodexAccountForRetry(7, 2)
	acct.LifecycleRevision = 5
	store := &configRevisionStore{acct: acct}
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: newRetryFakeLatch()}

	// 刷新令牌的语义等价物：ext 行变了、accounts 行不变。
	store.acct.Ext = &domain.AccountExt{
		CredentialType:         credential.TypeCodexOAuth,
		CodexOAuthToken:        strPtrSDK("rotated-access"),
		CodexOAuthRefreshToken: strPtrSDK("rotated-refresh"),
	}

	require.NoError(t, HandleFailure(context.Background(), deps, 7, errors.New("fatal")))
	require.NotNil(t, store.acct.FailedAt, "a token refresh must not void the verdict")
	require.Equal(t, []int64{2}, store.gotExpect)
}

func strPtrSDK(s string) *string { return &s }

// TestHandleFailureNeedsTemplateCapableStore 钉住围栏路径的**存储前提**：判决的
// 候选指纹与凭据判别符都读模板，而 GetAccount 不预载模板的实现（repository 的
// AccountRepo 即如此）会让判决无法成形。此前该情形以"缺凭据判别符"被拒——一旦
// 装配了 Latch，SDK 失效就会**静默地完全不再落库**（fail-open），故必须走
// GetAccountWithTemplate 兜底。
func TestHandleFailureNeedsTemplateCapableStore(t *testing.T) {
	newAcct := func() *domain.Account {
		a := newCodexAccountForRetry(7, 1)
		a.Template = nil // 模拟 GetAccount 只取账号行（不预载模板）
		return a
	}

	t.Run("store without template capability cannot form a verdict", func(t *testing.T) {
		store := &configRevisionStore{acct: newAcct()}
		deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: newRetryFakeLatch()}
		err := HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
		require.ErrorIs(t, err, ErrMissingCredentialDiscriminator)
		require.Empty(t, store.gotExpect, "判决不得成形，故不得尝试 CAS")
	})

	t.Run("template-capable store forms the verdict", func(t *testing.T) {
		store := &configRevisionStore{acct: newAcct(), templateOnDemand: newCodexAccountForRetry(7, 1).Template}
		deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: newRetryFakeLatch()}
		require.NoError(t, HandleFailure(context.Background(), deps, 7, errors.New("fatal")))
		require.Equal(t, []int64{1}, store.gotExpect, "补齐模板后判决以 K 成形并 CAS")
		require.NotNil(t, store.acct.FailedAt)
	})
}
