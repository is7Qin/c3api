// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"embed"
	"io/fs"
)

// webDist 前端构建产物（web 构建脚本输出 → cmd/server/dist）。
// 未构建时仅含 .gitkeep 占位（仓库内已提交），服务端正常启动（开发期无 UI 也能跑）。
//
//go:embed all:dist
var webDist embed.FS

func webUI() fs.FS {
	sub, err := fs.Sub(webDist, "dist")
	if err != nil {
		return fs.FS(emptyFS{})
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }
