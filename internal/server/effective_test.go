// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
)

func TestEffectiveMaxInflight(t *testing.T) {
	v, err := config.EffectiveMaxInflight(0)
	require.NoError(t, err)
	require.Equal(t, int64(50000), v)

	s := NewServer(Options{MaxInflight: v})
	require.NotNil(t, s)
	require.Equal(t, int64(50000), s.opts.MaxInflight)

	v2, err := config.EffectiveMaxInflight(12345)
	require.NoError(t, err)
	require.Equal(t, int64(12345), v2)
	s2 := NewServer(Options{MaxInflight: v2})
	require.Equal(t, int64(12345), s2.opts.MaxInflight)

	_, err = config.EffectiveMaxInflight(-1)
	require.Error(t, err)
	require.ErrorContains(t, err, "proxy.max_inflight")
}

func TestServerAndRecorderSameEffective(t *testing.T) {
	raw := int64(0)
	eff, err := config.EffectiveMaxInflight(raw)
	require.NoError(t, err)
	require.Equal(t, int64(50000), eff)
	s := NewServer(Options{MaxInflight: eff})
	require.Equal(t, eff, s.opts.MaxInflight)
}
