// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/logx"
)

// persistClassified 把一次 persist 失败投影成 rule 侧可识别的分类
// （rule.PersistError）：包依赖方向 scheduler → rule，分类枚举只能由
// scheduler 侧写入、rule 侧 errors.As 读取。原始错误保留（Unwrap）——
// ErrMissingExpectedRevision / ErrMissingCandidateFingerprint /
// ErrCandidateFingerprintMismatch / repository.ErrStaleIdentityRevision 的
// errors.Is 语义与 wrap 前完全一致（测试与调用方均依赖）。
func persistClassified(kind rule.PersistErrorKind, err error) error {
	return &rule.PersistError{Kind: kind, Err: err}
}

type rulePersistStore interface {
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	FailAccountCAS(ctx context.Context, id int64, expectedRevision int64, source string, failedAt time.Time, reason string) error
	GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error)
}

type rulePersistTemplateStore interface {
	GetAccountWithTemplate(ctx context.Context, id int64) (*domain.Account, error)
}

func NewRulePersistFunc(store rulePersistStore, latchStore *latch.LatchStore, pub interface {
	PublishGroups(ctx context.Context, gids []int64)
}, log *logx.Logger) rule.PersistFunc {
	return func(ctx context.Context, item rule.PersistItem) error {
		if !item.Then.FailAccount {
			return nil
		}
		if item.Event.ExpectedIdentityRevision <= 0 {
			return persistClassified(rule.PersistErrMissingRevision, ErrMissingExpectedRevision)
		}
		acct, err := store.GetAccount(ctx, item.Event.AccountID)
		if err != nil {
			return err
		}
		if acct.Template == nil {
			if withTemplate, ok := store.(rulePersistTemplateStore); ok {
				acct, err = withTemplate.GetAccountWithTemplate(ctx, item.Event.AccountID)
				if err != nil {
					return err
				}
			}
		}
		fp, ferr := candidateFingerprint(acct)
		if ferr != nil {
			// candidateFingerprint 已 wrap ErrMissingCandidateFingerprint（缺身份字段）——
			// 分类相同：身份不可计算 ⇒ 判决无法与身份绑定。
			return persistClassified(rule.PersistErrMissingFingerprint, ferr)
		}
		if item.Event.CandidateFingerprint == "" {
			return persistClassified(rule.PersistErrMissingFingerprint, ErrMissingCandidateFingerprint)
		}
		if item.Event.CandidateFingerprint != fp {
			return persistClassified(rule.PersistErrFingerprintMismatch, ErrCandidateFingerprintMismatch)
		}
		reason := domain.TruncateErrMsg(item.Event.ErrorMessage)
		if reason == "" {
			reason = "rule fail_account"
		}
		err = store.FailAccountCAS(ctx, item.Event.AccountID, item.Event.ExpectedIdentityRevision, domain.FailureSourceRule, time.Now(), reason)
		if err != nil {
			if errors.Is(err, repository.ErrStaleIdentityRevision) {
				if fresh, ferr := store.GetAccount(ctx, item.Event.AccountID); ferr == nil && fresh.IdentityRevision > item.Event.ExpectedIdentityRevision && latchStore != nil {
					latchStore.Clear(item.Event.AccountID)
				}
				return persistClassified(rule.PersistErrStaleRevision, err)
			}
			return err
		}
		if latchStore != nil {
			latchStore.Clear(item.Event.AccountID)
		}
		if pub != nil {
			gids, _ := store.GetAccountGroups(ctx, item.Event.AccountID)
			if len(gids) > 0 {
				pub.PublishGroups(context.WithoutCancel(ctx), gids)
			}
		}
		return nil
	}
}
