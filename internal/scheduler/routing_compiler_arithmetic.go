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

func nonNegPrice(p *int64) uint64 {
	if p == nil || *p < 0 {
		return 0
	}
	return uint64(*p)
}

// inputUnitPurchaseCost is
// trunc(mult * (billable*inputPrice + cached*cachePrice) / (denom * 10000 * 1000000)).
// denom is divided first because it is one limb. The fixed 10^10 scale is divided
// second. Reversing those divisions drops a remainder. denom is nonzero before
// the call. Only a quotient above MaxInt64 saturates.
func inputUnitPurchaseCost(mult, billable, cached, inputPrice, cachePrice, denom uint64) int64 {
	pHi, pLo := weightedPrice(billable, inputPrice, cached, cachePrice)
	n2, n1, n0 := mul192(pHi, pLo, mult)
	q2, q1, q0 := div192By64(n2, n1, n0, denom)
	s2, s1, s0 := div192By64(q2, q1, q0, 10000*1000000)
	if s2 != 0 || s1 != 0 || s0 > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(s0)
}

func div192By64(n2, n1, n0, d uint64) (uint64, uint64, uint64) {
	q2, r2 := bits.Div64(0, n2, d)
	q1, r1 := bits.Div64(r2, n1, d)
	q0, _ := bits.Div64(r1, n0, d)
	return q2, q1, q0
}

func weightedPrice(billable, inputPrice, cached, cachePrice uint64) (uint64, uint64) {
	hi, lo := bits.Mul64(billable, inputPrice)
	hi2, lo2 := bits.Mul64(cached, cachePrice)
	sum, carry := bits.Add64(lo, lo2, 0)
	return hi + hi2 + carry, sum
}

func mul192(hi, lo, m uint64) (uint64, uint64, uint64) {
	loHi, loLo := bits.Mul64(lo, m)
	hiHi, hiLo := bits.Mul64(hi, m)
	mid, carry := bits.Add64(loHi, hiLo, 0)
	return hiHi + carry, mid, loLo
}
