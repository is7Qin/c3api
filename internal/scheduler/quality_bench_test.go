// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"
)

func BenchmarkWilson95(b *testing.B) {
	for b.Loop() {
		_ = Wilson95(80, 100)
	}
}

func BenchmarkLogTTFT(b *testing.B) {
	logged := math.Log(100)
	sum, sumSq := logged*30, logged*logged*30
	b.ReportAllocs()
	for b.Loop() {
		_, _ = LogTTFTInterval(sum, sumSq, 30)
	}
}

func BenchmarkQualityClassify(b *testing.B) {
	logged := math.Log(100)
	candidates := []QualityCandidate{
		{AccountID: 1, Successes: 28, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 10},
		{AccountID: 2, Successes: 25, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 20},
		{AccountID: 3, Successes: 5, Attempts: 10, Cost: 5},
	}
	workspace := NewQualityWorkspace(len(candidates))
	out := NewQualityClassification(len(candidates))
	var err error
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		err = ClassifyQuality(candidates, &workspace, &out)
	}
	if err != nil {
		b.Fatal(err)
	}
}
