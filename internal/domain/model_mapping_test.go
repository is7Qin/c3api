// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"encoding/json"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestModelMappingModeValid(t *testing.T) {
	require.True(t, ModelMappingModeExplicit.Valid())
	require.True(t, ModelMappingModeImplicit.Valid())
	require.False(t, ModelMappingModeInvalid.Valid())
	require.False(t, ModelMappingMode(0).Valid())
	require.False(t, ModelMappingMode(99).Valid())
}

func TestModelMappingModeJSONStrings(t *testing.T) {
	data, err := json.Marshal(ModelMappingModeExplicit)
	require.NoError(t, err)
	require.Equal(t, `"explicit"`, string(data))
	data, err = json.Marshal(ModelMappingModeImplicit)
	require.NoError(t, err)
	require.Equal(t, `"implicit"`, string(data))
	_, err = json.Marshal(ModelMappingModeInvalid)
	require.Error(t, err)
	_, err = json.Marshal(ModelMappingMode(99))
	require.Error(t, err)

	var m ModelMappingMode
	require.NoError(t, json.Unmarshal([]byte(`"explicit"`), &m))
	require.Equal(t, ModelMappingModeExplicit, m)
	require.NoError(t, json.Unmarshal([]byte(`"implicit"`), &m))
	require.Equal(t, ModelMappingModeImplicit, m)
	require.Error(t, json.Unmarshal([]byte(`"unknown"`), &m))
	require.Error(t, json.Unmarshal([]byte(`null`), &m))
	require.Error(t, json.Unmarshal([]byte(`""`), &m))
}

func TestModelMappingTopLevelNullRejected(t *testing.T) {
	var m ModelMapping
	err := json.Unmarshal([]byte(`null`), &m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be null")
}

func TestModelMappingZeroEnumRejected(t *testing.T) {
	entry := ModelMappingEntry{MappedModel: "upstream", Mode: ModelMappingModeInvalid}
	_, err := json.Marshal(entry)
	require.Error(t, err)
	m := ModelMapping{"alias": entry}
	require.Error(t, ValidateModelMapping(m))
	e2 := ModelMappingEntry{MappedModel: "x", Mode: 0}
	require.False(t, e2.Mode.Valid())
}

func TestModelMappingEntryJSONRoundTrip(t *testing.T) {
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
	require.Contains(t, string(data), `"mapped_model":"upstream-a"`)
	require.Contains(t, string(data), `"mode":"explicit"`)
	require.Contains(t, string(data), `"mode":"implicit"`)
}

func TestModelMappingEntryRejectsInvalid(t *testing.T) {
	cases := []string{
		`{"alias": "legacy-string"}`,
		`{"alias": {"mapped_model": "upstream"}}`,
		`{"alias": {"mode": "explicit"}}`,
		`{"alias": {"mapped_model": "upstream", "mode": "unknown"}}`,
		`{"alias": {"mapped_model": "", "mode": "explicit"}}`,
		`{"alias": {"mapped_model": "  ", "mode": "explicit"}}`,
		`{" alias": {"mapped_model": "upstream", "mode": "explicit"}}`,
		`{"alias ": {"mapped_model": "upstream", "mode": "explicit"}}`,
		`{"alias": {"mapped_model": " upstream", "mode": "explicit"}}`,
		`{"alias": {"mapped_model": "upstream ", "mode": "explicit"}}`,
		`{"alias": null}`,
		`{"alias": {"mapped_model": "upstream", "mode": null}}`,
	}
	for _, c := range cases {
		var m ModelMapping
		err := json.Unmarshal([]byte(c), &m)
		if err != nil {
			continue
		}
		require.Error(t, ValidateModelMapping(m), "should reject %s", c)
	}
}

func TestModelMappingCompactSize(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("size assertion only on amd64")
	}
	sz := unsafe.Sizeof(ModelMappingEntry{})
	require.Equal(t, uintptr(24), sz, "ModelMappingEntry should be 24B (string 16 + uint8 + 7 padding)")
}

func TestTemplateServesWithNewMapping(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		ModelMapping: ModelMapping{"claude-sonnet": {MappedModel: "claude-4", Mode: ModelMappingModeExplicit}},
	}
	require.True(t, tpl.Serves("claude-sonnet"))
	require.False(t, tpl.Serves("nope"))
	require.True(t, tpl.HasModelSpace())
	empty := &Template{}
	require.False(t, empty.HasModelSpace())
	empty2 := &Template{ModelMapping: ModelMapping{}}
	require.False(t, empty2.HasModelSpace())
}
