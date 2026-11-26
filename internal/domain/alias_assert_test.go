// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"testing"

	"github.com/is7qin/c3api/internal/identityenv"
	"github.com/is7qin/c3api/internal/modelmap"
	"github.com/stretchr/testify/require"
)

// 运行期断言：两侧**共享同一份数据**（值语义一致）。
//
// ⚠ 诚实边界：这条**不是**"别名 vs named type"的判别器——实测 Go 的 map 转换
// **不拷贝**（共享底层 map），所以即使本包的类型是 named type，写回也照样可见。
// 真正拦 named type 的是 alias_assert.go 里的**编译期无转换赋值**。本测试只钉
// "行为一致"，防止将来有人把别名换成**包装类型 / 深拷贝实现**：
// 注意那两个 `var back … = …` 是编译期闸，拦不住包装；真正拦住包装的是下面
// 的写回断言——包装/深拷贝会让写回只落在副本上，`m["k2"]` 读不到 "v2"。
func TestDomainTypesBehaveAsTheNeutralTypes(t *testing.T) {
	m := ModelMapping{}
	m["k"] = ModelMappingEntry{MappedModel: "v", Mode: ModelMappingModeExplicit}
	var back modelmap.ModelMapping = m // 无转换赋值（第二道编译期闸）
	back["k2"] = modelmap.ModelMappingEntry{MappedModel: "v2", Mode: modelmap.ModelMappingModeImplicit}
	require.Equal(t, "v2", m["k2"].MappedModel, "写回必须对本包侧可见：两侧是同一份数据，不是深拷贝")

	e := IdentityEnvelope{}
	e["installation_id"] = json.RawMessage(`"x"`)
	var eback identityenv.IdentityEnvelope = e // 无转换赋值
	require.Equal(t, "x", eback.InstallationID())
	require.Equal(t, "x", e.InstallationID(), "两侧读到的必须是同一份数据")
}
