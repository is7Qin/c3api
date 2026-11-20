// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
)

func NonNegPrice(p *int64) uint64 {
	if p == nil || *p < 0 {
		return 0
	}
	return uint64(*p)
}

// inputUnitPurchaseCost is
// trunc(mult * (billable*inputPrice + cached*cachePrice) / (denom * 10000)).
// Prices are already milli-cents per million tokens, and this quotient keeps that
// unit: milli-cents per million input-side tokens. Dividing by another million
// would truncate ordinary prices to zero. denom is divided first because it is
// one limb; the 10000 basis-point scale is divided second. Reversing those
// divisions drops a remainder. denom is nonzero before the call. Only a quotient
// above MaxInt64 saturates.
func InputUnitPurchaseCost(mult, billable, cached, inputPrice, cachePrice, denom uint64) int64 {
	pHi, pLo := weightedPrice(billable, inputPrice, cached, cachePrice)
	n2, n1, n0 := mul192(pHi, pLo, mult)
	q2, q1, q0 := div192By64(n2, n1, n0, denom)
	s2, s1, s0 := div192By64(q2, q1, q0, 10000)
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
