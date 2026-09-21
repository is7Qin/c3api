// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/server"
)

// 收尾：真实 embed FS（webUI）上 /assets/ 必须 404——目录请求被 webFSNoDirs
// 包装拒绝，不渲染 HTML 目录列表（go:embed all:dist 内容不可枚举）。若未来
// dist 下出现 assets/ 目录且包装被移除，此测试立即捕获目录列表暴露。
func TestEmbedAssetsNoDirectoryListing(t *testing.T) {
	s := server.NewServer(server.Options{AdminToken: "tok", WebFS: webUI()})

	req := httptest.NewRequest(http.MethodGet, "/assets/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "embed FS 目录请求 404")
	require.NotContains(t, rec.Body.String(), "<html", "不得渲染目录列表 HTML")
}
