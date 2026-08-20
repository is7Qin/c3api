// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package handler 实现 /api/admin/* 的 HTTP 处理：OpenAPI 契约层（oapi-codegen
// 生成的 ServerInterface + chi 路由），JSON in/out。
package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// AdminAPI 实现生成的 ServerInterface（契约层唯一实现）。
// 注：生成代码自带包级 func Handler(...) 路由助手，故实现结构体命名
// AdminAPI 以避免与生成的 Handler 函数重名。
type AdminAPI struct {
	svc *service.Service
	ops OpsOptions // 运维观测装配（GetOpsWorkers 用；变参注入，零值 = 端点返回空）
	// overview/users-top 聚合面缓存（spec 2026-08-14 TTL：30s/2s——dashboard
	// 轮询频率下无陈旧感；键含参数与 UTC 日界；无 singleflight）。
	overviewCache *ttlCache
	usersTopCache *ttlCache
	// now 可注入时钟（默认 time.Now；测试注入断言缓存键日界滚转）。
	now func() time.Time
}

// New 构造契约处理器（路由由 HandlerWithOptions 生成）。
// ops 变参：GetOpsWorkers 的 worker 引用在组合根（cmd/server/main.go）持有，
// 由装配面注入；缺省零值 = 端点返回空（测试/无运维装配场景零改动）。
func New(svc *service.Service, ops ...OpsOptions) *AdminAPI {
	var o OpsOptions
	if len(ops) > 0 {
		o = ops[0]
	}
	return &AdminAPI{
		svc:           svc,
		ops:           o,
		overviewCache: newTTLCache(30 * time.Second),
		usersTopCache: newTTLCache(2 * time.Second),
		now:           time.Now,
	}
}

// Router 返回带 /api/admin 前缀的 chi 路由（替代原 Routes/RoutesMux）。
// ErrorHandlerFunc 覆盖生成的默认 http.Error 纯文本 400：参数绑定失败
// （InvalidParamFormatError 等）统一输出契约 ErrorResponse（{"error": ...}）。
func (h *AdminAPI) Router() http.Handler {
	return HandlerWithOptions(h, ChiServerOptions{
		BaseURL: "/api/admin",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		},
	})
}

// RoutesMux 兼容保留：cmd/server/main.go 仍以 Handle("/api/admin/*") 挂载，
// 后续任务改接 Router 后可删除。
func (h *AdminAPI) RoutesMux() http.Handler {
	return h.Router()
}

// decode 严格解码（管理面全部 JSON 入参共用）：未知字段 → 错误（拼错字段名
// 从 200 静默不生效变 400 显式，与 config ErrorUnused fail-fast 哲学一致——
// spec 2026-08-17 边界收敛）；二次 Decode 拒尾随数据（io.EOF 才算完）。
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after JSON body")
		}
		return err
	}
	return nil
}

// deref 返回指针指向的值；nil 时返回零值。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// ptr 返回指向 v 的指针（响应契约字段赋值用）。
func ptr[T any](v T) *T { return &v }

// normalizeIDs 校验批量 ids 1–100 条且去重（返回去重后列表，条数按去重后计）。
func normalizeIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, errors.New("ids must contain 1-100 entries")
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
