// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package migrate

// request_patch_pg_test.go 是 0004 的 adopt→up 面证据（spec 2026-11-26-custom-request-patch
// §4.2 评审 oracle m6 / §11.13）：
//
//	adopt 0001 或 0003 快照（账本丢失的存量库）→ up 到 head → template_exts.request_patch
//	存在且可写，版本门从 readonly 翻回 serve。
//
// adopt 是运维在账本丢失时唯一的出路，所以「收养出来的旧库仍能继续升级」必须是真 PG
// 证据，而不是注释：0004 是 expand，旧二进制不读新列；收养的库在 up 之前 head<4，
// RequiredVersion=4 的启动门必须先把它判成 readonly（fail-fast），而不是等第一次
// 读 template_ext 时 42703。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
)

// TestAdoptSnapshotThenUpAddsRequestPatch 覆盖两条收养入口：0001（老库基线）与
// 0003（0004 之前的历史 head）。
func TestAdoptSnapshotThenUpAddsRequestPatch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		snapshot string
		upTo     int64
	}{
		{snapshot: "0001", upTo: 1},
		{snapshot: "0003", upTo: 3},
	}
	for _, tc := range cases {
		t.Run("adopt "+tc.snapshot, func(t *testing.T) {
			db := newEmptyDB(t)

			_, err := Up(ctx, db, UpOptions{Target: tc.upTo})
			require.NoError(t, err)
			require.Equal(t, versionRange(1, tc.upTo), appliedVersions(t, db))

			// 抹掉账本 ⇒ 「有 schema、无账本」的存量库形态；adopt 是唯一出路。
			_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
			require.NoError(t, err)
			adopted, err := Adopt(ctx, db, AdoptOptions{Snapshot: tc.snapshot})
			require.NoError(t, err, "adopt %s 必须接受该快照形状", tc.snapshot)
			require.Len(t, adopted.Adopted, int(tc.upTo))
			require.Equal(t, OriginAdopted, adopted.Adopted[0].Origin)

			// 收养出来的库仍是旧 head：request_patch 还没补上，启动门必须拦成 readonly。
			require.False(t, relationColumnExists(t, db, "template_exts", "request_patch"),
				"%s 形状里没有 request_patch（它属 0004）", tc.snapshot)
			verdict, err := Verify(ctx, db)
			require.NoError(t, err)
			require.Equal(t, tc.upTo, verdict.DBHead)
			require.Equal(t, ModeReadonly, Decide(verdict).Mode,
				"head %d < RequiredVersion %d 必须 readonly（不是运行期 42703）", tc.upTo, RequiredVersion)

			// 升到 head：0004 落地（0001 收养还顺带补 0002/0003）。
			res, err := Up(ctx, db, UpOptions{})
			require.NoError(t, err)
			require.Equal(t, int64(4), res.To)
			require.Equal(t, versionRange(1, 4), appliedVersions(t, db))

			verdict, err = Verify(ctx, db)
			require.NoError(t, err)
			require.True(t, verdict.Compatible(), strings.Join(verdict.Problems, "; "))
			require.Equal(t, ModeServe, Decide(verdict).Mode)

			// template_exts.request_patch 存在、可写、可读（custom 模板的 ext 行）。
			require.True(t, relationColumnExists(t, db, "template_exts", "request_patch"),
				"up 到 head 后 0004 的列必须存在")
			execScript(t, db, `
INSERT INTO public.templates (name, base_url, credential_type, supported_formats, models, format_models, model_mapping, updated_at, created_at)
VALUES ('patch-tpl', 'https://upstream.example.com/v1', 'custom', '["openai-chat"]'::jsonb, '[]'::jsonb, '{}'::jsonb, '{}'::jsonb, now(), now());
INSERT INTO public.template_exts (template_id, credential_type, request_patch)
VALUES (1, 'custom', '{"headers":{"x-a":"b"}}'::jsonb);
`)
			var equal bool
			require.NoError(t, db.QueryRowContext(ctx, `SELECT request_patch = '{"headers":{"x-a":"b"}}'::jsonb
				FROM public.template_exts WHERE template_id = 1`).Scan(&equal))
			require.True(t, equal, "request_patch 必须可写可读（jsonb round-trip）")
		})
	}
}

// versionRange 返回 [from, to] 的连续版本号（升序）。
func versionRange(from, to int64) []int64 {
	out := make([]int64, 0, to-from+1)
	for version := from; version <= to; version++ {
		out = append(out, version)
	}
	return out
}

// relationColumnExists 判断 public.<table> 是否有 <column>（information_schema 口径）。
func relationColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                  WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists))
	return exists
}
