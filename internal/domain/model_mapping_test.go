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
	require.False(t, ModelMappingMode(99).Valid())
}

func TestModelMappingModeJSONRoundTrip(t *testing.T) {
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

func TestModelMappingRejectsTopLevelNull(t *testing.T) {
	for _, input := range []string{`null`, " \nnull\t"} {
		var m ModelMapping
		err := json.Unmarshal([]byte(input), &m)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not be null")
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

func TestModelMappingRejectsInvalidEntries(t *testing.T) {
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
		if err == nil {
			err = ValidateModelMapping(m)
		}
		require.Error(t, err, "should reject %s", c)
	}
}

func TestModelMappingEntrySizeAMD64(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("size assertion only on amd64")
	}
	require.Equal(t, uintptr(24), unsafe.Sizeof(ModelMappingEntry{}))
}
