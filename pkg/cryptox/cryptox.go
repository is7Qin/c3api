// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package cryptox

import (
	"crypto/rand"
	"encoding/hex"
)

// NewGroupKey 生成分组客户端 key 明文：raw = "ck-" + 32hex（16B 随机，长度 35）。
// ck- = client key；历史 gk- 遗留前缀作废（key 已独立于分组）。
// 明文直接落库（唯一约束 + 鉴权等值查——去 hash 列，见 key_raw 设计）。
func NewGroupKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptox: rand read failed: " + err.Error())
	}
	return "ck-" + hex.EncodeToString(b)
}

// NewManagementKey 生成管理 API key 明文：raw = "mk-" + 32hex（16B 随机，长度 35）。
// mk- = management key；前缀是管理面鉴权的唯一判别符（eyJ=JWT、ck-=客户端 key、
// mk-=管理 key，三者不重叠）。明文直接落库（唯一约束 + 鉴权等值查）。
func NewManagementKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptox: rand read failed: " + err.Error())
	}
	return "mk-" + hex.EncodeToString(b)
}
