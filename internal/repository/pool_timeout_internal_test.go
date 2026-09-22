// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// appendPoolTimeouts DSN 补丁形态单元测试：分隔符按 DSN 形态裁定
// （URL ?/&、keyword/value 空格），用户已显式配置同名参数时尊重不覆盖。
// 真实生效断言（会话级 GUC SHOW）见 pg_pool_timeout_test.go。

import (
	"strings"
	"testing"
)

func TestAppendPoolTimeouts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"url with existing query",
			"postgres://postgres:c3api@localhost:15432/c3api?sslmode=disable",
			"postgres://postgres:c3api@localhost:15432/c3api?sslmode=disable&lock_timeout=5s",
		},
		{
			"url without query",
			"postgres://postgres:c3api@localhost:15432/c3api",
			"postgres://postgres:c3api@localhost:15432/c3api?lock_timeout=5s",
		},
		{
			"keyword form",
			"host=localhost dbname=c3api user=postgres",
			"host=localhost dbname=c3api user=postgres lock_timeout=5s",
		},
		{
			"user configured lock respected",
			"postgres://u:p@h:5432/db?lock_timeout=2s",
			"postgres://u:p@h:5432/db?lock_timeout=2s",
		},
		{
			"statement_timeout never appended (degraded form)",
			"postgres://u:p@h:5432/db?statement_timeout=3s",
			"postgres://u:p@h:5432/db?statement_timeout=3s&lock_timeout=5s",
		},
		{
			"keyword form user configured lock only",
			"host=h dbname=d lock_timeout=2s",
			"host=h dbname=d lock_timeout=2s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendPoolTimeouts(tc.in)
			if got != tc.want {
				t.Fatalf("appendPoolTimeouts(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !strings.Contains(got, "lock_timeout=") {
				t.Fatalf("result %q missing lock_timeout", got)
			}
		})
	}
}
