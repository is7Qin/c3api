// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/identityenv"
)

// BPSDefaultEndpoint is the fixed endpoint identity used by BPS routing. The
// adapter owns URL construction; this value is only a domain validation and
// fingerprint input.
const BPSDefaultEndpoint = "https://bps.openai.com/basispoints/api"

// BPSBaseURLAllowed accepts only the SDK-owned endpoint or an empty value. An
// empty value means "use the fixed endpoint"; arbitrary origins are never
// allowed to receive BPS OAuth credentials.
func BPSBaseURLAllowed(raw string) bool {
	return raw == "" || raw == BPSDefaultEndpoint
}

// BPSProfileHash returns the hex digest of a validated BPS profile, or "" when
// the value is absent, null, not a JSON object, or not canonicalizable. Staying
// in domain is deliberate: this is the BPS routing fingerprint, a domain
// concept, while the canonicalisation engine itself lives in identityenv.
func BPSProfileHash(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if err := ValidateBPSProfileJSON(trimmed); err != nil {
		return ""
	}
	h, err := CanonicalJSONHash(trimmed)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(h[:])
}

// BPSProfileDigest 返回 BPS 模板的 profile 摘要，是路由指纹与计划键共用的
// **唯一**派生点（收口前三处调用逐字节一致）。
//
// 契约（顺序有意义，不得重排）：
//
//  1. t 不是 BPS ⇒ ""。**必须先判模板类型**：非 BPS 账号的 ext 可能残留一段
//     BPSProfile（S1 刻意保留的语义债——写面不清理旧字段），若只按 ext 判空，
//     那截残留就会进摘要，把非 BPS 候选的身份绑到一段它根本不消费的数据上。
//  2. ext == nil ⇒ ""（字段容器契约：没有 AccountExt 就没有 profile）。
//  3. 否则 BPSProfileHash(ext.BPSProfile)：空/JSON null/非 object 一律回落 ""
//     （BPSProfileHash 的既有语义），即"未配置 profile 的 BPS 账号没有摘要"，
//     由候选过滤按不可路由处理，而不是退化成某个可比较的伪摘要。
func BPSProfileDigest(t credential.Type, ext *AccountExt) string {
	if !t.IsBPS() {
		return ""
	}
	if ext == nil {
		return ""
	}
	return BPSProfileHash(ext.BPSProfile)
}

// ValidateBPSProfileJSON enforces the phase-1 storage boundary: a profile is
// a complete non-null JSON object. Detailed DeviceProfile validation remains in
// sdkbridge, where bps-sdk is the authority.
func ValidateBPSProfileJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("domain: BPS profile is required")
	}
	canonical, err := CanonicalJSON(trimmed)
	if err != nil {
		return fmt.Errorf("domain: invalid BPS profile: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return fmt.Errorf("domain: BPS profile must be a JSON object")
	}
	return nil
}

// 以下是 identityenv 侧 canonical 定义的**同名别名**，不是新类型 / 新函数：
// ent 生成代码要引用 IdentityEnvelope（而 ent 不许反向依赖本包），repository 与
// sdkbridge 则经本包消费，别名让两侧同时成立且只有一份实现。
//
// 保留清单的依据是"搬家块之外，本包内还有谁在用"：
//   - CanonicalJSON：BPSProfileHash 与 ValidateBPSProfileJSON（两者都留在 domain）；
//   - CanonicalJSONHash：BPSProfileHash；
//   - IdentityJSONEqual：repository/ext_codex_repo.go 的变更判定；
//   - ParseIdentityEnvelope / IdentityPatch：本包测试与 envelope 的构造路径。
//
// CanonicalIdentityJSON / writeCanonicalJSON / canonicalNumber 刻意**不**留别名：
// 它们在 domain 之外零消费者，留一个无人使用的导出名等于给包扩面。
type (
	IdentityEnvelope = identityenv.IdentityEnvelope
	IdentityPatch    = identityenv.IdentityPatch
)

var (
	ParseIdentityEnvelope = identityenv.ParseIdentityEnvelope
	CanonicalJSON         = identityenv.CanonicalJSON
	CanonicalJSONHash     = identityenv.CanonicalJSONHash
	IdentityJSONEqual     = identityenv.IdentityJSONEqual
)
