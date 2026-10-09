// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- T3: bounded streaming compare sink ---

// TestCompareSinkExactness pins the exact equality predicate of the compare
// sink: equal ⇒ true; any single-byte difference (first/middle/last), a longer
// stream, a shorter stream (remaining old suffix) and an empty old all ⇒ false.
func TestCompareSinkExactness(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)})
	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	old := decisionViewBytes(dv)
	require.NotEmpty(t, old)

	compare := func(target []byte, mutate func([]byte)) bool {
		cpy := append([]byte(nil), target...)
		if mutate != nil {
			mutate(cpy)
		}
		var e decisionEncoder
		return e.compareCanonical(dv, cpy)
	}

	require.True(t, compare(old, nil), "identical bytes must be equal")
	require.False(t, compare(old, func(b []byte) { b[0] ^= 0xff }), "first-byte difference ⇒ unequal")
	require.False(t, compare(old, func(b []byte) { b[len(b)/2] ^= 0xff }), "middle-byte difference ⇒ unequal")
	require.False(t, compare(old, func(b []byte) { b[len(b)-1] ^= 0xff }), "last-byte difference ⇒ unequal")
	require.False(t, compare(old[:len(old)-1], nil), "a shorter old leaves a remaining suffix ⇒ unequal")
	require.False(t, compare(append(append([]byte(nil), old...), 0x00), nil), "a longer old ⇒ unequal")
	require.False(t, compare(nil, nil), "an empty old with non-empty output ⇒ unequal")
}

// TestCompareSinkEmptyViewIsNil: a nil DecisionView still encodes to nil (the
// d == nil guard) and compares unequal against a nil old only by length.
func TestCompareSinkNilViewIsNil(t *testing.T) {
	var e decisionEncoder
	require.Nil(t, e.encode(nil))
}

// TestDecisionEncoderHoldsNoOutputBuffer: the encoder must not retain any output
// buffer across fires (T3 removes the ~64MB resident bytes.Buffer). Only the
// refs/ids scratch slices remain.
func TestDecisionEncoderHoldsNoOutputBuffer(t *testing.T) {
	rt := reflect.TypeOf(decisionEncoder{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		require.NotEqual(t, "bytes.Buffer", f.Type.String(),
			"decisionEncoder must not hold an output buffer field %q", f.Name)
		require.Equal(t, reflect.Slice, f.Type.Kind(),
			"decisionEncoder field %q must be a scratch slice", f.Name)
	}
}

func BenchmarkDecisionViewCompareEqual(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCompareEqual = e.compareCanonical(dv, old)
	}
}

func BenchmarkDecisionViewCompareChanged(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	old[len(old)/2] ^= 0xff // first difference at the mid-stream byte
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCompareEqual = e.compareCanonical(dv, old)
	}
}

// BenchmarkDecisionViewRootOnlyChange: bytes are identical but the static root
// changed. The compare still streams the full encoding (equal); the publish path
// then re-uses the held bytes without re-encoding.
func BenchmarkDecisionViewRootOnlyChange(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCompareEqual = e.compareCanonical(dv, old)
	}
}
