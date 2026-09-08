// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
)

// AvgTokens rounds sum/successes half-up (0 when no successes). Shared by the
// compiler cost lane and the service quality-cost frontier.
func AvgTokens(sum int64, successes int64) int64 {
	if successes <= 0 {
		return 0
	}
	if sum > math.MaxInt64-successes/2 {
		return sum / successes
	}
	return (sum + successes/2) / successes
}

// SaturatingMulDiv computes a*b/divisor saturating at MaxInt64 on 64x64
// overflow. Shared by the compiler cost lane and the service frontier.
func SaturatingMulDiv(a, b, divisor int64) int64 {
	if divisor == 0 {
		return math.MaxInt64
	}
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 {
		return math.MaxInt64
	}
	return int64(lo / uint64(divisor))
}
