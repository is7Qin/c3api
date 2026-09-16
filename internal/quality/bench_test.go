// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"fmt"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/repository"
)

func BenchmarkQualityRecorder(b *testing.B) {
	r, _ := NewRecorder(50000)
	f := fp(1)
	q := qc(1)
	cell := r.GetOrCreateCell(CanonicalKey(f, q, f))
	var ctx AttemptContext
	tt := int64(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok := r.InitAttemptContext(cell, &ctx)
		if !ok {
			b.Fatal("init failed")
		}
		ctx.Complete(true, &tt, 10, 1, 0)
	}
}

// BenchmarkFlowOwnerDuplicateMergeSteadyState preloads an owner with a fixed
// retained distinct identity count outside the measured section, then folds
// one fixed 8-row duplicate chain per op through the live cell fold.
// B/op is the gate (independent of the
// retained count); allocs/op is secondary evidence.
func BenchmarkFlowOwnerDuplicateMergeSteadyState(b *testing.B) {
	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	minute := fixed.Unix()
	for _, retained := range []int{10, 2000} {
		b.Run(fmt.Sprintf("retained-%d", retained), func(b *testing.B) {
			rec, err := NewRecorder(50000)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < retained; i++ {
				if err := foldConsumerRows(rec.FlowOwner(), minute, []repository.RoutingFlowRow{flowTestRow(fixed, 1, int64(i), "success", true)}); err != nil {
					b.Fatal(err)
				}
			}
			dup := make([]repository.RoutingFlowRow, 0, 8)
			for i := 0; i < 8; i++ {
				dup = append(dup, flowTestRow(fixed, 1, int64(i), "success", true))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = foldConsumerRows(rec.FlowOwner(), minute, dup)
			}
		})
	}
}

// BenchmarkFlowRedisPayload：缓存路径（生产）vs 旧"每发布重物化+json.Marshal"
// 路径（snapshotForRedis 仍保留，作为对照）。夹具 = 单分钟 2000 行。
func BenchmarkFlowRedisPayload(b *testing.B) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, err := NewRecorder(50000)
	if err != nil {
		b.Fatal(err)
	}
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	m := fixed.Truncate(time.Minute).Unix()
	rows := make([]repository.RoutingFlowRow, 0, 2000)
	for i := int64(1); i <= 2000; i++ {
		rows = append(rows, ownerTestRow(i))
	}
	if err := foldConsumerRows(owner, m, rows); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		blob, _, _ := owner.redisPayload(m)
		sinkFlowBlob = len(blob)
	}
}

var sinkFlowBlob int
