// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestPGEmailCodeUpsertOverwrite(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	email := "alice@example.com"
	purpose := string(domain.EmailCodeRegister)
	sha1 := sha256Hex("111111")
	sha2 := sha256Hex("222222")
	exp1 := time.Now().Add(10 * time.Minute)
	exp2 := time.Now().Add(20 * time.Minute)

	row1, err := repos.UpsertEmailCode(ctx, email, purpose, sha1, exp1)
	require.NoError(t, err)
	require.Equal(t, sha1, row1.CodeSHA256)
	require.Equal(t, 0, row1.Attempts)

	// simulate attempts increment then second upsert should reset
	_, err = repos.IncrementEmailCodeAttempts(ctx, email, purpose)
	require.NoError(t, err)
	_, err = repos.IncrementEmailCodeAttempts(ctx, email, purpose)
	require.NoError(t, err)
	got, err := repos.GetEmailCode(ctx, email, purpose)
	require.NoError(t, err)
	require.Equal(t, 2, got.Attempts)

	// second upsert — overwrites sha, resets attempts, extends expires
	row2, err := repos.UpsertEmailCode(ctx, email, purpose, sha2, exp2)
	require.NoError(t, err)
	require.Equal(t, sha2, row2.CodeSHA256, "second upsert overwrites sha")
	require.Equal(t, 0, row2.Attempts, "attempts reset to 0")
	require.WithinDuration(t, exp2, row2.ExpiresAt, time.Second)

	// same email different purpose should be independent
	rowOther, err := repos.UpsertEmailCode(ctx, email, string(domain.EmailCodeReset), sha1, exp1)
	require.NoError(t, err)
	require.Equal(t, domain.EmailCodePurpose("reset"), rowOther.Purpose)
	got2, err := repos.GetEmailCode(ctx, email, purpose)
	require.NoError(t, err)
	require.Equal(t, sha2, got2.CodeSHA256, "other purpose does not affect register")
}

func TestPGEmailCodeExpiryAndAttempts(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	email := "bob@example.com"

	t.Run("expiry stored and retrievable", func(t *testing.T) {
		sha := sha256Hex("333333")
		past := time.Now().Add(-5 * time.Minute) // already expired
		_, err := repos.UpsertEmailCode(ctx, email, string(domain.EmailCodeRegister), sha, past)
		require.NoError(t, err)
		row, err := repos.GetEmailCode(ctx, email, string(domain.EmailCodeRegister))
		require.NoError(t, err)
		require.NotNil(t, row)
		require.True(t, time.Now().After(row.ExpiresAt), "row is expired")
	})

	t.Run("future expiry not expired", func(t *testing.T) {
		sha := sha256Hex("444444")
		future := time.Now().Add(10 * time.Minute)
		_, err := repos.UpsertEmailCode(ctx, "fresh@example.com", string(domain.EmailCodeRegister), sha, future)
		require.NoError(t, err)
		row, err := repos.GetEmailCode(ctx, "fresh@example.com", string(domain.EmailCodeRegister))
		require.NoError(t, err)
		require.True(t, row.ExpiresAt.After(time.Now()))
	})

	t.Run("attempts increment cap", func(t *testing.T) {
		em := "attempts@example.com"
		sha := sha256Hex("555555")
		_, err := repos.UpsertEmailCode(ctx, em, string(domain.EmailCodeRegister), sha, time.Now().Add(10*time.Minute))
		require.NoError(t, err)
		for i := 1; i <= 5; i++ {
			n, err := repos.IncrementEmailCodeAttempts(ctx, em, string(domain.EmailCodeRegister))
			require.NoError(t, err)
			require.Equal(t, i, n)
		}
		// one more increment (beyond cap) still increments DB, but service caps at 5
		n, err := repos.IncrementEmailCodeAttempts(ctx, em, string(domain.EmailCodeRegister))
		require.NoError(t, err)
		require.Equal(t, 6, n, "repo does not cap, service caps")

		// delete and verify not found
		require.NoError(t, repos.DeleteEmailCode(ctx, em, string(domain.EmailCodeRegister)))
		got, err := repos.GetEmailCode(ctx, em, string(domain.EmailCodeRegister))
		require.NoError(t, err)
		require.Nil(t, got)
		// delete missing → ErrNotFound
		err = repos.DeleteEmailCode(ctx, em, string(domain.EmailCodeRegister))
		require.ErrorIs(t, err, repository.ErrNotFound)
	})
}

func TestPGEmailCodeGetNotFound(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	got, err := repos.GetEmailCode(ctx, "none@example.com", string(domain.EmailCodeRegister))
	require.NoError(t, err)
	require.Nil(t, got)
	_, err = repos.IncrementEmailCodeAttempts(ctx, "none@example.com", string(domain.EmailCodeRegister))
	require.Error(t, err)
}
