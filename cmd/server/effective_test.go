// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/server"
)

func TestEffectiveMaxInflight(t *testing.T) {
	v, err := config.EffectiveMaxInflight(0)
	require.NoError(t, err)
	require.Equal(t, int64(50000), v)
	s := server.NewServer(server.Options{MaxInflight: v})
	require.NotNil(t, s.Handler())

	r, err := quality.NewRecorder(v)
	require.NoError(t, err)
	require.Equal(t, int64(50000), r.EffectiveMaxInflight())

	_, err = config.EffectiveMaxInflight(-1)
	require.Error(t, err)

	_, err = quality.NewRecorder(-1)
	require.Error(t, err)
}
