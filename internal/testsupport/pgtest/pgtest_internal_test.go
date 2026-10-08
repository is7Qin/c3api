// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pgtest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureTemplateDropsOnMigrateFailure pins the "any failure after CREATE →
// DROP + error" contract (spec §5.7/A5): a template whose migration fails must
// not be left behind, or a later run would see the name and reuse a half-built
// database. It exercises ensureTemplate directly with a migrate function that
// always errors, driven through a dedicated C3API_TEST_TEMPLATE probe so the
// shared current-hash template — which parallel packages may be cloning — is
// never touched.
func TestEnsureTemplateDropsOnMigrateFailure(t *testing.T) {
	m, e := state(t)
	ctx := context.Background()

	const probe = "c3api_test_template_migrate_fail_probe"
	t.Setenv("C3API_TEST_TEMPLATE", probe)
	_, _ = m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(probe)+" WITH (FORCE)")
	t.Cleanup(func() {
		_, _ = m.conn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(probe)+" WITH (FORCE)")
	})

	sentinel := errors.New("boom: migration failed")
	failing := func(context.Context, string) error { return sentinel }

	_, err := ensureTemplate(t, m, e, failing)
	require.ErrorIs(t, err, sentinel)

	var exists bool
	require.NoError(t, m.conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, probe).Scan(&exists))
	require.False(t, exists, "a template whose migration failed must be dropped, not left behind")

	// A second attempt must also not find (and reuse) a leftover half-built
	// template: it rebuilds, fails again, and the probe is gone again.
	_, err = ensureTemplate(t, m, e, failing)
	require.ErrorIs(t, err, sentinel)
	require.NoError(t, m.conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, probe).Scan(&exists))
	require.False(t, exists, "the retry must also leave no half-built template")
}
