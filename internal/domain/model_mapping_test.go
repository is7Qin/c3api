// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestModelMappingModeValid(t *testing.T) {
	require.True(t, ModelMappingModeExplicit.Valid())
	require.True(t, ModelMappingModeImplicit.Valid())
	require.False(t, ModelMappingModeInvalid.Valid())
	require.False(t, ModelMappingMode(99).Valid())
}

func TestModelMappingModeJSONRoundTrip(t *testing.T) {
	data, err := json.Marshal(ModelMappingModeExplicit)
	require.NoError(t, err)
	require.Equal(t, `"explicit"`, string(data))
	data, err = json.Marshal(ModelMappingModeImplicit)
	require.NoError(t, err)
	require.Equal(t, `"implicit"`, string(data))
	// MarshalJSON 的拒绝分支（types.go:256）：值渲染是 Mode.String()，即
	// `ModelMappingMode(%d)`——实测 Mode(99) → "ModelMappingMode(99)"。
	// 用后缀而非 EqualError：encoding/json 会在前面套 "json: error calling
	// MarshalJSON for type ..."，那层前缀是 stdlib 的实现细节，不属于本包契约。
	_, err = json.Marshal(ModelMappingModeInvalid)
	require.True(t, err != nil && strings.HasSuffix(err.Error(), `invalid ModelMappingMode "ModelMappingMode(0)"`), "实际 %v", err)
	_, err = json.Marshal(ModelMappingMode(99))
	require.True(t, err != nil && strings.HasSuffix(err.Error(), `invalid ModelMappingMode "ModelMappingMode(99)"`), "实际 %v", err)

	var m ModelMappingMode
	require.NoError(t, json.Unmarshal([]byte(`"explicit"`), &m))
	require.Equal(t, ModelMappingModeExplicit, m)
	require.NoError(t, json.Unmarshal([]byte(`"implicit"`), &m))
	require.Equal(t, ModelMappingModeImplicit, m)
	err = json.Unmarshal([]byte(`"unknown"`), &m)
	require.True(t, err != nil && strings.HasPrefix(err.Error(), "invalid mode "), "实际 %v", err)
	require.EqualError(t, json.Unmarshal([]byte(`null`), &m), "mode must not be null")
	require.EqualError(t, json.Unmarshal([]byte(`""`), &m), `invalid mode ""`)
}

// TestModelMappingRejectsTopLevelNull 钉的是**解码面**约束：`UnmarshalJSON` 碰到
// null 自己返回 error。它与 internal/handler 的 raw-JSON 探测**文案相同、语义不同**
// （那边是 HTTP 400 形状契约，根本不经过 UnmarshalJSON），故两处各自钉、不得合并。
func TestModelMappingRejectsTopLevelNull(t *testing.T) {
	for _, input := range []string{`null`, " \nnull\t"} {
		var m ModelMapping
		err := json.Unmarshal([]byte(input), &m)
		require.EqualError(t, err, "model_mapping must not be null", "input %q", input)
	}
}

func TestModelMappingRejectsInvalidMode(t *testing.T) {
	entry := ModelMappingEntry{MappedModel: "upstream", Mode: ModelMappingModeInvalid}
	_, err := json.Marshal(entry)
	require.Error(t, err)
	m := ModelMapping{"alias": entry}
	require.Error(t, ValidateModelMapping(m))
}

func TestModelMappingJSONRoundTrip(t *testing.T) {
	orig := ModelMapping{
		"alias-a": {MappedModel: "upstream-a", Mode: ModelMappingModeExplicit},
		"alias-b": {MappedModel: "upstream-b", Mode: ModelMappingModeImplicit},
		"same":    {MappedModel: "same", Mode: ModelMappingModeImplicit},
	}
	data, err := json.Marshal(orig)
	require.NoError(t, err)
	var decoded ModelMapping
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, orig, decoded)
}

// TestModelMappingRejectsInvalidEntries 的**文案**是用户面契约（无效映射会原样进 400
// 响应体），所以每一条都钉住文案——`require.Error` 挡不住整句改写。
//
// 分两类，口径不同的理由：
//
//   - 无 `%q` 的文案是**稳定契约**：整句逐字钉（`require.EqualError`）。改一个字就该红。
//   - 带 `%q` 的文案拼的是**被拒绝的输入原值**：钉"固定模板 + 值渲染"，不裸钉前缀。
//     裸前缀（只到 `alias `）对**后缀**改写完全失明——这是实测过一次的假绿：把
//     `… without leading/trailing whitespace` 删掉，`HasPrefix("alias ")` 照样通过。
func TestModelMappingRejectsInvalidEntries(t *testing.T) {
	// A. 无 %q：钉整句。
	exact := []struct{ body, want string }{
		{`{"alias": null}`, "model mapping entry must not be null"},
		{`{"alias": {"mode": "explicit"}}`, "mapped_model is required"},
		{`{"alias": {"mapped_model": "upstream"}}`, "mode is required"},
		{`{"alias": {"mapped_model": "upstream", "mode": "explicit", "extra": 1}}`, "additional properties not allowed"},
		{`{"alias": {"mapped_model": "", "mode": "explicit"}}`, "mapped_model must be non-empty without leading/trailing whitespace"},
		{`{"alias": {"mapped_model": "  ", "mode": "explicit"}}`, "mapped_model must be non-empty without leading/trailing whitespace"},
	}
	for _, tc := range exact {
		var m ModelMapping
		err := json.Unmarshal([]byte(tc.body), &m)
		if err == nil {
			err = ValidateModelMapping(m)
		}
		require.EqualError(t, err, tc.want, "input %s", tc.body)
	}

	// B. 带 %q：钉固定模板 + 值渲染（前缀不足以挡住后缀改写）。
	//
	// 两条到达路径必须分开，因为同一个"空白"输入会先撞上解码面：
	//   - `{"alias": {"mapped_model": " upstream"}}` 由 ModelMappingEntry.UnmarshalJSON
	//     的无 %q 文案拦下（见上表），永远走不到 ValidateModelMapping；
	//   - 只有形状合法、内容非法的映射才进验证面，那里才有 %q 文案。
	type prefixCase struct {
		name    string
		mapping ModelMapping
		body    string // 非空 = 走 json.Unmarshal；空 = 直接构造走 ValidateModelMapping
		want    string // 固定模板 + 被拒绝原值的渲染形态
	}
	for _, tc := range []prefixCase{
		// 别名空白只有验证面能验：解码面先以 "additional properties not allowed" 拦下同
		// 一个输入（`{"alias ": ...}` 在 map 里是额外键之外的另一个键，形状不合法），
		// 于是上面那条无 %q 文案会永远先命中——用 body 写过这些用例就是假绿。
		{
			name:    "别名前导空白（验证面）",
			mapping: ModelMapping{" alias": {MappedModel: "upstream", Mode: ModelMappingModeExplicit}},
			want:    `alias " alias" must be non-empty without leading/trailing whitespace`,
		},
		{
			name:    "别名尾随空白（验证面）",
			mapping: ModelMapping{"alias ": {MappedModel: "upstream", Mode: ModelMappingModeExplicit}},
			want:    `alias "alias " must be non-empty without leading/trailing whitespace`,
		},
		{name: "未知 mode", body: `{"alias": {"mapped_model": "upstream", "mode": "unknown"}}`, want: `invalid mode "unknown"`},
		{
			name:    "映射值前导空白（验证面）",
			mapping: ModelMapping{"alias": {MappedModel: " upstream", Mode: ModelMappingModeExplicit}},
			want:    `mapped_model " upstream" must be non-empty without leading/trailing whitespace`,
		},
		{
			name:    "映射值尾随空白（验证面）",
			mapping: ModelMapping{"alias": {MappedModel: "upstream ", Mode: ModelMappingModeExplicit}},
			want:    `mapped_model "upstream " must be non-empty without leading/trailing whitespace`,
		},
		{
			// 第三条并列校验（types.go:340）：值渲染是 `Mode.String()`，即
			// `ModelMappingMode(%d)`——实测 Mode(99) → "ModelMappingMode(99)"。
			// 这一条同样是**验证面**：解码面先以 "invalid mode %q" 拦下非法 mode
			// （见上表），只有直接构造的映射才走到这里。
			name:    "mode 非法值（验证面）",
			mapping: ModelMapping{"alias": {MappedModel: "upstream", Mode: ModelMappingMode(99)}},
			want:    `mode "ModelMappingMode(99)" must be explicit or implicit`,
		},
	} {
		var err error
		if tc.body != "" {
			var m ModelMapping
			err = json.Unmarshal([]byte(tc.body), &m)
			if err == nil {
				err = ValidateModelMapping(m)
			}
		} else {
			err = ValidateModelMapping(tc.mapping)
		}
		require.Error(t, err, "%s: should reject", tc.name)
		require.EqualError(t, err, tc.want, "%s: 文案模板 + 值渲染必须逐字稳定", tc.name)
	}
}

func TestModelMappingEntrySizeAMD64(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("size assertion only on amd64")
	}
	require.Equal(t, uintptr(24), unsafe.Sizeof(ModelMappingEntry{}))
}
