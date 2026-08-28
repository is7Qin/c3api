// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import "errors"

var (
	ErrInvalidQualityStats         = errors.New("invalid quality statistics")
	ErrQualityOverflow             = errors.New("quality arithmetic overflow")
	ErrInsufficientQualityCapacity = errors.New("insufficient quality capacity")
	ErrDuplicateQualityCandidate   = errors.New("duplicate quality candidate")
)
