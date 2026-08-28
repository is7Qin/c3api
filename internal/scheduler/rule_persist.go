// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/logx"
)

type rulePersistStore interface {
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	FailAccountCAS(ctx context.Context, id int64, expectedRevision int64, source string, failedAt time.Time, reason string) error
	GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error)
}

func NewRulePersistFunc(store rulePersistStore, latch *latchStore, pub interface {
	PublishGroups(ctx context.Context, gids []int64)
}, log *logx.Logger) rule.PersistFunc {
	return func(ctx context.Context, item rule.PersistItem) error {
		if !item.Then.FailAccount {
			return nil
		}
		acct, err := store.GetAccount(ctx, item.Event.AccountID)
		if err != nil {
			if latch != nil {
				latch.Clear(item.Event.AccountID)
			}
			return nil
		}
		if acct.LifecycleRevision != item.Event.ExpectedRevision {
			return nil
		}
		fp := acct.UpstreamKey
		if acct.BaseURL != nil {
			fp += "|" + *acct.BaseURL
		}
		// stale fingerprint fence: if latch fingerprint differs, clear old
		if latch != nil {
			// latch already acquired in HealthController; verify still latched
			if !latch.IsLatched(item.Event.AccountID, fp) {
				return nil
			}
		}
		reason := domain.TruncateErrMsg(item.Event.ErrorMessage)
		if reason == "" {
			reason = "rule fail_account"
		}
		err = store.FailAccountCAS(ctx, item.Event.AccountID, item.Event.ExpectedRevision, "rule", time.Now(), reason)
		if err != nil {
			if errors.Is(err, repository.ErrStaleRevision) {
				if fresh, ferr := store.GetAccount(ctx, item.Event.AccountID); ferr == nil && fresh.LifecycleRevision > item.Event.ExpectedRevision && latch != nil {
					latch.Clear(item.Event.AccountID)
				}
			}
			return err
		}
		if latch != nil {
			latch.Clear(item.Event.AccountID)
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
