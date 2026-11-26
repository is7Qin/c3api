// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package modelmap 持有模板 model_mapping 列的**值类型与校验**：别名 → 上游模型
// 的映射表、映射模式与它的 JSON 线缆形态。
//
// 它是**中立叶子包**：只 import stdlib（bytes / encoding/json / fmt / strings），
// 绝不反向 import 任何 internal/* 或 pkg/*。allow 矩阵里本包的规则是显式的
// `to: []string{}`——将来有人想往里加依赖，会得到一条明确的拒绝，而不是被
// leafRule 顺手放行。
//
// 为什么单独成包（而不是留在 internal/domain）：ent 的生成模型要这个字段类型
// （Template.ModelMapping），ent 又不能反向依赖 domain 值类型层。把这类"纯
// 序列化契约"下沉成叶子包之后，生成代码只依赖契约，账本里那条
// `internal/ent → internal/domain` 的边就消失了。domain 侧保留同名别名
// （`type ModelMapping = modelmap.ModelMapping`），所以消费方零改动。
package modelmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type ModelMappingMode uint8

const (
	ModelMappingModeInvalid  ModelMappingMode = 0
	ModelMappingModeExplicit ModelMappingMode = 1
	ModelMappingModeImplicit ModelMappingMode = 2
)

func (m ModelMappingMode) Valid() bool {
	switch m {
	case ModelMappingModeExplicit, ModelMappingModeImplicit:
		return true
	}
	return false
}

func (m ModelMappingMode) String() string {
	switch m {
	case ModelMappingModeExplicit:
		return "explicit"
	case ModelMappingModeImplicit:
		return "implicit"
	default:
		return fmt.Sprintf("ModelMappingMode(%d)", uint8(m))
	}
}

func (m ModelMappingMode) MarshalJSON() ([]byte, error) {
	switch m {
	case ModelMappingModeExplicit:
		return json.Marshal("explicit")
	case ModelMappingModeImplicit:
		return json.Marshal("implicit")
	default:
		return nil, fmt.Errorf("invalid ModelMappingMode %q", m.String())
	}
}

func (m *ModelMappingMode) UnmarshalJSON(data []byte) error {
	var s *string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("mode must not be null")
	}
	switch *s {
	case "explicit":
		*m = ModelMappingModeExplicit
	case "implicit":
		*m = ModelMappingModeImplicit
	default:
		return fmt.Errorf("invalid mode %q", *s)
	}
	return nil
}

type ModelMappingEntry struct {
	MappedModel string           `json:"mapped_model"`
	Mode        ModelMappingMode `json:"mode"`
}

func (e *ModelMappingEntry) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("model mapping entry must not be null")
	}
	var raw map[string]*json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["mapped_model"]; !ok {
		return fmt.Errorf("mapped_model is required")
	}
	if _, ok := raw["mode"]; !ok {
		return fmt.Errorf("mode is required")
	}
	if len(raw) != 2 {
		return fmt.Errorf("additional properties not allowed")
	}
	type alias ModelMappingEntry
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	if strings.TrimSpace(tmp.MappedModel) == "" || tmp.MappedModel != strings.TrimSpace(tmp.MappedModel) {
		return fmt.Errorf("mapped_model must be non-empty without leading/trailing whitespace")
	}
	*e = ModelMappingEntry(tmp)
	return nil
}

type ModelMapping map[string]ModelMappingEntry

func (m *ModelMapping) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("model_mapping must not be null")
	}
	type raw ModelMapping
	var tmp raw
	if err := json.Unmarshal(data, (*map[string]ModelMappingEntry)(&tmp)); err != nil {
		return err
	}
	if tmp == nil {
		tmp = make(raw)
	}
	*m = ModelMapping(tmp)
	return nil
}

func ValidateModelMapping(m ModelMapping) error {
	for alias, entry := range m {
		if strings.TrimSpace(alias) == "" || alias != strings.TrimSpace(alias) {
			return fmt.Errorf("alias %q must be non-empty without leading/trailing whitespace", alias)
		}
		if strings.TrimSpace(entry.MappedModel) == "" || entry.MappedModel != strings.TrimSpace(entry.MappedModel) {
			return fmt.Errorf("mapped_model %q must be non-empty without leading/trailing whitespace", entry.MappedModel)
		}
		if !entry.Mode.Valid() {
			return fmt.Errorf("mode %q must be explicit or implicit", entry.Mode.String())
		}
	}
	return nil
}
