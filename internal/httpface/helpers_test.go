// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type decodePayload struct {
	X string `json:"x"`
}

// probeJSONBody 构造 `{"x":"<n 字节>"}`（总长 = n+8）。
func probeJSONBody(n int) string {
	return `{"x":"` + strings.Repeat("a", n) + `"}`
}

// TestDecodeLimitBoundaries 证明探测 8 MiB 与普通端点 1 MiB 的分界，且
// Decode/DecodeLimit 共享同一严格语义。
func TestDecodeLimitBoundaries(t *testing.T) {
	// 恰好 1 MiB 的 JSON 在 Decode 下通过。
	exact1MiB := probeJSONBody(1<<20 - 8)
	req := httptest.NewRequest("POST", "/", strings.NewReader(exact1MiB))
	var out decodePayload
	require.NoError(t, Decode(req, &out))
	require.Len(t, out.X, 1<<20-8)

	// 1 MiB + 1 字节：Decode 拒绝，DecodeLimit(8 MiB) 接受。
	over1MiB := probeJSONBody(1<<20 - 7)
	req = httptest.NewRequest("POST", "/", strings.NewReader(over1MiB))
	require.Error(t, Decode(req, &out), "普通端点必须保持 1 MiB 上限")
	req = httptest.NewRequest("POST", "/", strings.NewReader(over1MiB))
	require.NoError(t, DecodeLimit(req, &out, domain.ProbeMaxRequestBytes))

	// 恰好 8 MiB 接受；8 MiB + 1 拒绝。
	exact8MiB := probeJSONBody(8<<20 - 8)
	req = httptest.NewRequest("POST", "/", strings.NewReader(exact8MiB))
	require.NoError(t, DecodeLimit(req, &out, domain.ProbeMaxRequestBytes))
	require.Len(t, out.X, 8<<20-8)

	over8MiB := probeJSONBody(8<<20 - 7)
	req = httptest.NewRequest("POST", "/", strings.NewReader(over8MiB))
	require.Error(t, DecodeLimit(req, &out, domain.ProbeMaxRequestBytes))

	// 2 MiB 体：Decode 拒绝、DecodeLimit 接受（同一端点的两种上限）。
	twoMiB := probeJSONBody(2<<20 - 8)
	req = httptest.NewRequest("POST", "/", strings.NewReader(twoMiB))
	require.Error(t, Decode(req, &out))
	req = httptest.NewRequest("POST", "/", strings.NewReader(twoMiB))
	require.NoError(t, DecodeLimit(req, &out, domain.ProbeMaxRequestBytes))
}

// TestDecodeLimitStrictness 未知字段与 trailing data 在两种上限下都必须拒绝
// （DecodeLimit 与 Decode 同一实现，仅上限可配）。
func TestDecodeLimitStrictness(t *testing.T) {
	var out decodePayload
	unknownBody := `{"x":"a","y":1}`
	for _, limit := range []int64{1 << 20, domain.ProbeMaxRequestBytes} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(unknownBody))
		err := DecodeLimit(req, &out, limit)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown field")

		// `null` 第二次 Decode 成功 → trailing-data 检查必须显式拒绝。
		req = httptest.NewRequest("POST", "/", strings.NewReader(`{"x":"a"} null`))
		err = DecodeLimit(req, &out, limit)
		require.Error(t, err)
		require.Contains(t, err.Error(), "trailing")

		// 任意尾随 JSON 值同样拒绝。
		req = httptest.NewRequest("POST", "/", strings.NewReader(`{"x":"a"} {"x":"b"}`))
		require.Error(t, DecodeLimit(req, &out, limit))
		req = httptest.NewRequest("POST", "/", strings.NewReader(`{"x":"a"} []`))
		require.Error(t, DecodeLimit(req, &out, limit))
	}
}
