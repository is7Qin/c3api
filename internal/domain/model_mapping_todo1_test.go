// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelMappingModeValid(t *testing.T) {
	require.True(t, ModelMappingModeExplicit.Valid())
	require.True(t, ModelMappingModeImplicit.Valid())
	require.False(t, ModelMappingMode("unknown").Valid())
	require.False(t, ModelMappingMode("").Valid())
}

func TestModelMappingEntryJSONRoundTrip(t *testing.T) {
	orig := map[string]ModelMappingEntry{
		"alias-a": {MappedModel: "upstream-a", Mode: ModelMappingModeExplicit},
		"alias-b": {MappedModel: "upstream-b", Mode: ModelMappingModeImplicit},
		"same":    {MappedModel: "same", Mode: ModelMappingModeImplicit},
	}
	data, err := json.Marshal(orig)
	require.NoError(t, err)
	var decoded map[string]ModelMappingEntry
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, orig, decoded)
	// exact JSON shape
	require.Contains(t, string(data), `"mapped_model":"upstream-a"`)
	require.Contains(t, string(data), `"mode":"explicit"`)
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
		var m map[string]ModelMappingEntry
		err := json.Unmarshal([]byte(c), &m)
		if err != nil {
			continue
		}
		require.Error(t, ValidateModelMapping(m), "should reject %s", c)
	}
}

func TestTemplateServesWithNewMapping(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		ModelMapping: map[string]ModelMappingEntry{"claude-sonnet": {MappedModel: "claude-4", Mode: ModelMappingModeExplicit}},
	}
	require.True(t, tpl.Serves("claude-sonnet"))
	require.False(t, tpl.Serves("nope"))
	require.True(t, tpl.HasModelSpace())
	empty := &Template{}
	require.False(t, empty.HasModelSpace())
	empty2 := &Template{ModelMapping: map[string]ModelMappingEntry{}}
	require.False(t, empty2.HasModelSpace())
}
