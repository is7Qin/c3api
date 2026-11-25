// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package storeerr 定义**存储实现层**的错误哨兵（单一真相）。
//
// 为什么单独成包（R1-a）：哨兵是**调用方用来分类的控制流**，不是实现细节。
// internal/rule 与 internal/sdkbridge 需要 errors.Is 判定，却不该为此 import 整个
// 存储实现层（会把 ent、pgx、事务门面全部拉进依赖闭包）。方向固定：实现 → 契约。
//
// 为什么叫 storeerr 而不并进 internal/loadport：loadport 的 charter 是"控制面向存储
// 索要**账号快照**的装载端口"（见其包注释），错误分类与仓储接口与"装载"无语义关系。
// 本仓已有同形先例 internal/service/errors（serviceerr）：错误按**轴**单独成叶子包。
// k8s 亦然：apimachinery/pkg/api/errors 与 client-go 的存储接口不在同一个包。
//
// 与 serviceerr 的关系：两者是**不同的错误轴**。serviceerr 的 "service: " 前缀面向
// API 结果语义；本包的 "repository: " 前缀面向存储 CAS 语义。**不要合并。**
//
// 边界：只依赖 stdlib。**不**依赖 repository（否则环回来）。
package storeerr

import (
	"errors"
	"fmt"
)

// 哨兵文案是日志/用户面契约，**一个字节都不能改**（实测基线文案见 spec §1.4）。
var (
	ErrNotFound = errors.New("repository: not found")
	ErrConflict = errors.New("repository: conflict")

	// 下面两枚是 ErrConflict 的 %w 派生 ⇒ errors.Is(stale, ErrConflict) 成立。
	// 搬动时必须与 ErrConflict 同包，否则语义链断裂。
	ErrStaleRevision         = fmt.Errorf("%w: stale lifecycle_revision", ErrConflict)
	ErrStaleIdentityRevision = fmt.Errorf("%w: stale identity_revision", ErrConflict)
)
