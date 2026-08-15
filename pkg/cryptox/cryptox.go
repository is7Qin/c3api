// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package cryptox

import (
	"crypto/rand"
	"encoding/hex"
)

// NewGroupKey 生成分组客户端 key 明文：raw = "gk-" + 32hex（16B 随机，长度 35）。
// 明文直接落库（唯一约束 + 鉴权等值查——去 hash 列，见 key_raw 设计）。
func NewGroupKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptox: rand read failed: " + err.Error())
	}
	return "gk-" + hex.EncodeToString(b)
}
