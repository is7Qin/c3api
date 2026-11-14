// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package rule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestPersistFailuresAreClassified 钉住 persist 失败四分类（spec 2026-11-14 §2）：
// 四类各命中一次 ⇒ 四个字段各自 +1，且总数恰好 +4（总数语义不变，分类是额外字段）。
// 未包装的错误（如仓储 IO / health sink 失败）只进总数——分类字段不虚增。
func TestPersistFailuresAreClassified(t *testing.T) {
	causes := []error{
		&PersistError{Kind: PersistErrMissingRevision, Err: errors.New("missing K")},
		&PersistError{Kind: PersistErrMissingFingerprint, Err: errors.New("missing I")},
		&PersistError{Kind: PersistErrFingerprintMismatch, Err: errors.New("I mismatch")},
		&PersistError{Kind: PersistErrStaleRevision, Err: errors.New("stale K")},
	}
	next := 0
	e := New(Config{EventQueueSize: 8, PersistQueueSize: 8}, newFakeRuleStore(), nil, newFakeSink(8),
		func(context.Context, PersistItem) error {
			err := causes[next]
			next++
			return err
		})

	for range causes {
		e.enqueuePersist(Event{AccountID: 1, Kind: Kind5xx}, domain.RuleThen{FailAccount: true})
	}
	// 未 Start ⇒ Close 的 flushPersist 同步排空（确定性，无 sleep-as-sync）。
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.Close(closeCtx))
	require.Equal(t, len(causes), next, "四条 persist item 都应被消费")

	st := e.Stats().(RuleEngineStats)
	require.Equal(t, int64(4), st.PersistFailures, "总数语义不变（每次失败 +1）")
	require.Equal(t, int64(1), st.PersistFailuresMissingRevision)
	require.Equal(t, int64(1), st.PersistFailuresMissingFingerprint)
	require.Equal(t, int64(1), st.PersistFailuresFingerprintMismatch)
	require.Equal(t, int64(1), st.PersistFailuresStaleRevision)

	// 未分类失败：只推进总数。
	e.countPersistFailure(errors.New("repository io"))
	st = e.Stats().(RuleEngineStats)
	require.Equal(t, int64(5), st.PersistFailures)
	require.Equal(t, int64(1), st.PersistFailuresMissingRevision)
	require.Equal(t, int64(1), st.PersistFailuresMissingFingerprint)
	require.Equal(t, int64(1), st.PersistFailuresFingerprintMismatch)
	require.Equal(t, int64(1), st.PersistFailuresStaleRevision)
}

// TestPersistErrorUnwrapKeepsSentinelIdentity 包装不得吃掉原始错误：
// scheduler/repository 侧哨兵的 errors.Is 语义是既有调用方与测试的契约。
func TestPersistErrorUnwrapKeepsSentinelIdentity(t *testing.T) {
	base := errors.New("boom")
	wrapped := &PersistError{Kind: PersistErrStaleRevision, Err: base}
	require.ErrorIs(t, wrapped, base)
	require.Equal(t, PersistErrStaleRevision, ClassifyPersistError(wrapped))
	require.Equal(t, PersistErrOther, ClassifyPersistError(base), "未包装 ⇒ other，不进四个分类")
	require.Equal(t, PersistErrOther, ClassifyPersistError(nil))
	require.Equal(t, "stale_revision", PersistErrStaleRevision.String())
	require.Contains(t, wrapped.Error(), "stale_revision")
}
