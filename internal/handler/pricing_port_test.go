// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/pricingresult"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// fakePricingSync 可控返回值的消费方接口实现（不需要真实 worker）。
// 与 pricing_sync_test.go 的真实 *pricing.SyncWorker 分工：那边验行为不变
// （全链路），这里只验**错误谓词的精确性**。
type fakePricingSync struct{ err error }

func (f *fakePricingSync) SyncNow(ctx context.Context) (*pricingresult.SyncStats, error) {
	return nil, f.err
}

func (f *fakePricingSync) Preview(ctx context.Context) (*pricingresult.Preview, error) {
	return nil, f.err
}

// TestPricingSyncPredicateIsPrecise 谓词精确性：只有 pricingresult.FetchError 映射
// 502 固定文案；裸 serviceerr.ErrUpstream（其他上游故障的形态，被 codex_login /
// payment 等多处共享生产）不得被误标成「价格同步失败」。
//
// ⚠ 这是本单唯一能守住该不变量的机械判据：把 price_entry.go 的谓词改回
// errors.Is(err, serviceerr.ErrUpstream) 或任何哨兵判定，第二个子用例立刻红。
func TestPricingSyncPredicateIsPrecise(t *testing.T) {
	t.Run("FetchError 502", func(t *testing.T) {
		h := New(nil, OpsOptions{PricingSync: &fakePricingSync{
			err: fmt.Errorf("%w", &pricingresult.FetchError{Err: fmt.Errorf("boom")}),
		}})
		rec := postPricing(t, h, "/pricing/sync")
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "pricing sync failed")
	})

	t.Run("裸 ErrUpstream 不误标", func(t *testing.T) {
		h := New(nil, OpsOptions{PricingSync: &fakePricingSync{err: serviceerr.ErrUpstream}})
		rec := postPricing(t, h, "/pricing/sync")
		require.NotContains(t, rec.Body.String(), "pricing sync failed",
			"无关上游故障不得被误标成价格同步失败")
	})

	// preview 与 sync 共用同一条谓词。基线 bug（本单修）：preview 分支只走
	// WriteServiceErr，而 FetchError 不匹配任何 serviceerr 哨兵 ⇒ 500
	// "internal error"；若只把 FetchError 包上 ErrUpstream 让 WriteServiceErr 兜住，
	// 它会把 err.Error() 原样回显（上游 fetch 原文）——所以这里两条都钉住。
	t.Run("preview fetch 失败 502 且不泄漏内部文本", func(t *testing.T) {
		h := New(nil, OpsOptions{PricingSync: &fakePricingSync{
			err: &pricingresult.FetchError{Err: fmt.Errorf("secret internal detail")},
		}})
		rec := postPricing(t, h, "/pricing/sync/preview")
		require.Equal(t, http.StatusBadGateway, rec.Code, "不是 500")
		require.Contains(t, rec.Body.String(), "pricing sync failed")
		require.NotContains(t, rec.Body.String(), "secret internal detail", "不泄漏上游错误文本")
	})

	t.Run("preview 裸 ErrUpstream 不误标", func(t *testing.T) {
		h := New(nil, OpsOptions{PricingSync: &fakePricingSync{err: serviceerr.ErrUpstream}})
		rec := postPricing(t, h, "/pricing/sync/preview")
		require.NotContains(t, rec.Body.String(), "pricing sync failed",
			"preview 的谓词也必须专属，不能认通用 ErrUpstream")
	})
}
