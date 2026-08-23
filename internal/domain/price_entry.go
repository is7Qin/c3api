// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"encoding/json"
	"time"
)

// PriceMode 统一价格条目主计费方式。
type PriceMode string

const (
	PriceModeToken PriceMode = "token"
	PriceModeCall  PriceMode = "call"
	PriceModeImage PriceMode = "image"
)

func (m PriceMode) Valid() bool {
	switch m {
	case PriceModeToken, PriceModeCall, PriceModeImage:
		return true
	}
	return false
}

// PriceEntry 统一价格条目：每模型一行，mode 声明主计费方式但分量跨模式可选配。
type PriceEntry struct {
	ID                    int64
	Model                 string
	Mode                  PriceMode
	InputPerM             *int64 // 毫分/1M tokens；mode=token 必填
	OutputPerM            *int64
	CacheReadPerM         *int64
	CacheWritePerM        *int64
	PricePerCall          *int64 // 毫分/次；mode=call 必填
	ImgInTokPerM          *int64 // 毫分/1M image tokens
	ImgOutTokPerM         *int64
	PricePerImage         *int64 // 毫分/张
	Provider              *string
	MaxInputTokens        *int64
	MaxOutputTokens       *int64
	SupportsPromptCaching *bool
	Raw                   json.RawMessage
	Source                PricingSource
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// PriceVariant 条件变体：模型+seq 唯一，条件全可空=通配 AND 组合。
type PriceVariant struct {
	ID            int64
	Model         string
	Seq           int
	ServiceTier   *string // equality
	CtxMin        *int64  // promptTokens >= min
	CtxMax        *int64  // promptTokens < max (spec says containment; use >=min && <max or <=max? spec says ctx_min/ctx_max contain; define >=min && <=max for simplicity, test will clarify)
	TimeStart     *string // HH:MM本地时间
	TimeEnd       *string
	DowMask       *int
	MultBP        *int // 万分数
	SetInputPerM  *int64
	SetOutputPerM *int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ResolvedPrices 解析后生效单价组。
type ResolvedPrices struct {
	Mode           PriceMode
	InputPerM      *int64
	OutputPerM     *int64
	CacheReadPerM  *int64
	CacheWritePerM *int64
	PricePerCall   *int64
	ImgInTokPerM   *int64
	ImgOutTokPerM  *int64
	PricePerImage  *int64
	VariantSeq     *int // 命中变体 seq，无命中 nil
	Provider       *string
}
