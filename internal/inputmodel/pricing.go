// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package inputmodel

import "github.com/is7qin/c3api/internal/domain"

// PriceEntryManual 手动设价入参。
type PriceEntryManual struct {
	Model          string
	Mode           domain.PriceMode
	InputPerM      *int64
	OutputPerM     *int64
	CacheReadPerM  *int64
	CacheWritePerM *int64
	PricePerCall   *int64
	ImgInTokPerM   *int64
	ImgOutTokPerM  *int64
	PricePerImage  *int64
}
