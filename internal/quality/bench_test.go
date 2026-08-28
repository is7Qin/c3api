// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import "testing"

func BenchmarkQualityRecorder(b *testing.B) {
	r, _ := NewRecorder(50000)
	f := fp(1)
	q := qc(1)
	tt := int64(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := r.Begin(f, q)
		ctx.Complete(true, &tt, 10, 1, 0)
	}
}
