// SPDX-License-Identifier: AGPL-3.0-or-later
package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEffectiveMaxInflight(t *testing.T) {
	v, err := EffectiveMaxInflight(0)
	require.NoError(t, err)
	require.Equal(t, int64(50000), v)

	v, err = EffectiveMaxInflight(1)
	require.NoError(t, err)
	require.Equal(t, int64(1), v)

	v, err = EffectiveMaxInflight(50000)
	require.NoError(t, err)
	require.Equal(t, int64(50000), v)

	v, err = EffectiveMaxInflight(100000)
	require.NoError(t, err)
	require.Equal(t, int64(100000), v)

	_, err = EffectiveMaxInflight(-1)
	require.Error(t, err)
	require.ErrorContains(t, err, "proxy.max_inflight")

	_, err = EffectiveMaxInflight(-100)
	require.Error(t, err)
}

func TestEffectiveMaxInflightViaLoad(t *testing.T) {
	setenvRequired(t)
	_, err := Load(writeConfig(t, `proxy = { max_inflight = -1 }`))
	require.Error(t, err)
	require.ErrorContains(t, err, "proxy.max_inflight")

	c, err := Load(writeConfig(t, `proxy = { max_inflight = 0 }`))
	require.NoError(t, err)
	v, err := EffectiveMaxInflight(c.Proxy.MaxInflight)
	require.NoError(t, err)
	require.Equal(t, int64(50000), v)
}
