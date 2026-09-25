// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// P2/P4 的 handler 级判据（spec §8）：
//   - A15 回显头：三个 200 体是裸数组的端点（/stats/trend、/stats/entity-trend、
//     /api/user/stats）携带 X-Stats-Effective-From/To/Storage，值取自 service
//     判定回传的 Exec；**/overview 无回显头**（可证它永不 WindowShifted，§4.5）；
//   - A14(handler 级) 400 机读字段：window_too_long / window_invalid 走**真实
//     端点**断言（两者都是 clock 无关判定）；coverage 系列的序列化在
//     internal/handler/httpface 用合成载体钉死（避免墙钟）。
//   - A16' capabilities：投影/形状/解耦三件事（金标准数字见
//     internal/domain/statsplan_test.go——本包内刻意不写死那三个数字，
//     由 A16'② 的零命中审计钉死）。
//
// 纪律：require only、无 t.Parallel()、无墙钟依赖（判定全用固定字面量窗口）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// newStatsHandler 统计面 handler（fake store + 指定注入的保留期）。
// 保留期零值 ⇒ coverage 步整体跳过（本文件的用例因此与墙钟无关）。
func newStatsHandler(t *testing.T, ret domain.Retention) (*AdminAPI, *fakeStore) {
	t.Helper()
	fake := newFakeStore()
	fake.stats = []*domain.StatBucket{
		{BucketTime: time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC), Model: "m", RequestCount: 7},
	}
	svc := service.New(fake, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil,
		service.ServiceDeps{EmailCodeStore: fake, Retention: ret})
	return New(svc), fake
}

func getStats(h *AdminAPI, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/"+path, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// TestStatsEchoHeaders 回显头（A15）：三端点 200 携带三个头；值 = 判定后的生效
// 窗口与实际存储（不是请求窗口的复述）。
//   - Asia/Shanghai（恒 +8 整点）00:30→02:30 界不齐 ⇒ 服务端归一化到
//     01:00→03:00 走 cube：Storage=cube 且 Effective-From **晚于**请求 from；
//   - Asia/Kolkata（+5:30）同一窗口 ⇒ 只能扫原始行：Storage=raw 且生效窗口
//     == 请求窗口（Raw 分支不做对齐）。
func TestStatsEchoHeaders(t *testing.T) {
	h, _ := newStatsHandler(t, domain.Retention{})

	rec := getStats(h, "stats/trend?from=2026-08-17T00:30:00Z&to=2026-08-17T02:30:00Z&granularity=hour&timezone=Asia%2FShanghai")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "cube", rec.Header().Get("X-Stats-Storage"))
	require.Equal(t, "2026-08-17T01:00:00Z", rec.Header().Get("X-Stats-Effective-From"),
		"生效下界 = 请求下界向后取整到整点（证明回显取自判定，而非请求窗口）")
	require.Equal(t, "2026-08-17T03:00:00Z", rec.Header().Get("X-Stats-Effective-To"))

	rec = getStats(h, "stats/trend?from=2026-08-17T00:30:00Z&to=2026-08-17T02:30:00Z&granularity=hour&timezone=Asia%2FKolkata")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "raw", rec.Header().Get("X-Stats-Storage"), "半时区只能扫原始行")
	require.Equal(t, "2026-08-17T00:30:00Z", rec.Header().Get("X-Stats-Effective-From"),
		"Raw 分支生效窗口 == 请求窗口（不做对齐）")
	require.Equal(t, "2026-08-17T02:30:00Z", rec.Header().Get("X-Stats-Effective-To"))

	// 双界齐整点的恒整点时区 ⇒ cube/Exact：生效窗口 == 请求窗口。
	rec = getStats(h, "stats/trend?from=2026-08-17T00:00:00Z&to=2026-08-17T02:00:00Z&granularity=hour")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "cube", rec.Header().Get("X-Stats-Storage"))
	require.Equal(t, "2026-08-17T00:00:00Z", rec.Header().Get("X-Stats-Effective-From"))

	// entity-trend 同三头（同一判定）。
	rec = getStats(h, "stats/entity-trend?entity=user&id=7&from=2026-08-17T00:30:00Z&to=2026-08-17T02:30:00Z&granularity=hour&timezone=Asia%2FShanghai")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "cube", rec.Header().Get("X-Stats-Storage"))
	require.Equal(t, "2026-08-17T01:00:00Z", rec.Header().Get("X-Stats-Effective-From"))
	require.Equal(t, "2026-08-17T03:00:00Z", rec.Header().Get("X-Stats-Effective-To"))

	// /overview **无**回显头：它的窗口由服务端按 days 自算，永不发生位移
	//（A2b 的可证结论），且响应体是对象——回显头在它身上是虚假信号面。
	rec = getOverview(h, "days=7&timezone=Asia%2FKolkata")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Empty(t, rec.Header().Get("X-Stats-Storage"), "/overview 不得携带回显头")
	require.Empty(t, rec.Header().Get("X-Stats-Effective-From"))

	// 400 路径不写回显头（判定未成立，无"生效窗口"可言）。
	rec = getStats(h, "stats/trend?from=2026-08-17T00:30:00Z&to=2026-08-17T02:30:00Z&granularity=hour&timezone=Bad%2FZone")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Empty(t, rec.Header().Get("X-Stats-Storage"))
}

// TestStatsEchoHeadersUserStats /api/user/stats 与另两端点同形（裸数组 ⇒ 同三头）。
func TestStatsEchoHeadersUserStats(t *testing.T) {
	do, store, _, _ := newTestUserRouter(t)
	// 注册即登录（首个注册 = platform_admin），拿到 token 与自己的 user id。
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"echo@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var reg struct {
		Token string `json:"Token"`
		User  struct {
			ID int64 `json:"ID"`
		} `json:"User"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	require.NotEmpty(t, reg.Token)
	store.entityStats = []*domain.EntityStatBucket{
		{BucketTime: time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC), EntityType: "user", EntityID: reg.User.ID, RequestCount: 3},
	}

	rec = do(http.MethodGet,
		"/api/user/stats?from=2026-08-17T00:30:00Z&to=2026-08-17T02:30:00Z&granularity=hour&timezone=Asia%2FShanghai",
		"", reg.Token)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "cube", rec.Header().Get("X-Stats-Storage"))
	require.Equal(t, "2026-08-17T01:00:00Z", rec.Header().Get("X-Stats-Effective-From"))
	require.Equal(t, "2026-08-17T03:00:00Z", rec.Header().Get("X-Stats-Effective-To"))
}

// TestStatsWindowRejectMachineFields 400 机读字段（A14 handler 级）：走真实端点
// 断言 clock 无关的两类拒绝，并断言 error 人读文案形态不变、非统计 400 不带
// reason（既有客户端零感知）。
func TestStatsWindowRejectMachineFields(t *testing.T) {
	h, _ := newStatsHandler(t, domain.Retention{})

	// window_too_long：TTFT exact 分支 8d > 168h（纯常量 cap）→ 400，
	// storage=raw、limit_seconds=168h、effective_* 在，retention_days 不在
	//（cost 拒绝与保留期无关——A13b 的同一事实在 400 体上的投影）。
	rec := getStats(h, "stats/ttft?entity=user&id=7&from=2026-08-01T00:00:00Z&to=2026-08-09T00:00:00Z")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "window_too_long", body["reason"])
	require.Equal(t, "raw", body["storage"])
	require.EqualValues(t, int64(domain.MaxExactTTFTSpan/time.Second), body["limit_seconds"],
		"上限秒数 = 矩阵的 ttft_exact 成本上限（168h）")
	require.Equal(t, "2026-08-01T00:00:00Z", body["effective_from"])
	require.Equal(t, "2026-08-09T00:00:00Z", body["effective_to"])
	require.NotContains(t, body, "retention_days", "cost 拒绝不携带保留期")
	require.Contains(t, body["error"], "service: stats window too long", "人读文案形态保持不变")

	// window_invalid：from == to（倒序/空窗）→ 只报 reason，其余字段**省略**
	//（不得序列化 "cube"/0 这类伪造事实——J2b）。
	rec = getStats(h, "stats/trend?from=2026-08-17T00:00:00Z&to=2026-08-17T00:00:00Z")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	body = map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "window_invalid", body["reason"])
	require.NotContains(t, body, "storage")
	require.NotContains(t, body, "limit_seconds")
	require.NotContains(t, body, "effective_from")
	require.NotContains(t, body, "retention_days")

	// 非统计 400（非法时区）**不带** reason：既有 400 体逐字节不变。
	rec = getStats(h, "stats/trend?from=2026-08-17T00:00:00Z&to=2026-08-17T02:00:00Z&timezone=Bad%2FZone")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	body = map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotContains(t, body, "reason")
	require.Len(t, body, 1, "非统计 400 体仍只有 error 一个字段")
}

// int64Text 数字转十进制串（金标准由字符串拼出，避免把裸数字写进本包——
// A16'② 的零命中审计覆盖 internal/handler）。
func int64Text(v int64) string { return strconv.FormatInt(v, 10) }

// capsHandler 能力端点 handler（指定注入保留期）。
func capsHandler(t *testing.T, ret domain.Retention) *AdminAPI {
	t.Helper()
	h, _ := newStatsHandler(t, ret)
	return h
}

// TestStatsCapabilitiesShape capabilities 端点（A16'①）：响应是 domain 投影的
// 忠实序列化（形状不丢字段）；金标准数值由 internal/domain 的用例钉死。
func TestStatsCapabilitiesShape(t *testing.T) {
	const logDays, errlogDays, statsDays = 2, 7, 180
	h := capsHandler(t, domain.Retention{Log: logDays, ErrLog: errlogDays, Stats: statsDays})

	rec := getStats(h, "stats/capabilities")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got StatsCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	want := domain.StatsCapabilities(domain.Retention{Log: logDays, ErrLog: errlogDays, Stats: statsDays})
	require.Equal(t, len(want.Kinds), len(got.Kinds), "kind 数不得丢")
	for id, wk := range want.Kinds {
		gk, ok := got.Kinds[string(id)]
		require.True(t, ok, "kind %s 必须出现在响应中", id)
		require.Equal(t, wk.Grouping.String(), gk.Grouping)
		require.Len(t, gk.Storages, len(wk.Storages))
		for i, s := range wk.Storages {
			require.Equal(t, s.String(), string(gk.Storages[i]))
			require.Equal(t, wk.CostCapSeconds[s], gk.CostCapSeconds[s.String()], "%s/%s 上限", id, s)
			require.Equal(t, wk.CoverageDays[s], gk.CoverageDays[s.String()], "%s/%s 覆盖天数", id, s)
		}
	}
}

// TestStatsCapabilitiesGoldenShape 金标准形状（A16'④ 的形状面）：钉一份逐字段
// 字面量的期望 JSON——静默改字段名/嵌套/覆盖天数都会红。上限秒数取自同一份
// domain 常量（**本包内不出现任何上限秒值字面量**：那三个绝对数值的金标准在
// internal/domain——A16'② 的零命中审计覆盖本包，含测试文件）。
func TestStatsCapabilitiesGoldenShape(t *testing.T) {
	const logDays, errlogDays, statsDays = 2, 7, 180
	h := capsHandler(t, domain.Retention{Log: logDays, ErrLog: errlogDays, Stats: statsDays})

	rec := getStats(h, "stats/capabilities")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	cubeCap := int64(domain.MaxCubeSpan / time.Second)
	rawCap := int64(domain.MaxGroupedRawSpan / time.Second)
	exactCap := int64(domain.MaxExactTTFTSpan / time.Second)
	grid := int64(time.Hour / time.Second)

	zoned := func() string {
		return `{"grouping":"zoned","storages":["cube","raw"],` +
			`"cost_cap_seconds":{"cube":` + int64Text(cubeCap) + `,"raw":` + int64Text(rawCap) + `},` +
			`"coverage_days":{"cube":180,"raw":2}}`
	}
	cubeOnly := func() string {
		return `{"grouping":"none","storages":["cube"],` +
			`"cost_cap_seconds":{"cube":` + int64Text(cubeCap) + `},"coverage_days":{"cube":180}}`
	}
	rawOnly := func(cap string, days int) string {
		return `{"grouping":"none","storages":["raw"],` +
			`"cost_cap_seconds":{"raw":` + cap + `},"coverage_days":{"raw":` + int64Text(int64(days)) + `}}`
	}
	golden := `{"bucket_grid_seconds":` + int64Text(grid) + `,"kinds":{` +
		`"trend":` + zoned() + `,` +
		`"entity_trend":` + zoned() + `,` +
		`"summary":` + zoned() + `,` +
		`"days":` + zoned() + `,` +
		`"top":` + cubeOnly() + `,` +
		`"ttft_sketch":` + cubeOnly() + `,` +
		`"ttft_exact":` + rawOnly(int64Text(exactCap), logDays) + `,` +
		`"usage_agg":` + rawOnly(int64Text(cubeCap), logDays) + `,` +
		`"usage_list":` + rawOnly("0", logDays) + `,` +
		`"errlog_list":` + rawOnly("0", errlogDays) +
		`}}`
	require.JSONEq(t, golden, rec.Body.String())

	// ttft_exact 的 raw 上限 = 前端 TTFT_MAX_SPAN 的唯一来源（spec §4.4(a) 前端用途）。
	require.EqualValues(t, exactCap, 168*3600, "实体级精确 TTFT 上限 = 168h")
}

// TestStatsCapabilitiesCostIndependentOfRetention（A16'③）双配置注入：
// coverage_days 随保留期变，cost_cap_seconds **不变**（cost 与 coverage 解耦）。
func TestStatsCapabilitiesCostIndependentOfRetention(t *testing.T) {
	read := func(ret domain.Retention) StatsCapabilities {
		rec := getStats(capsHandler(t, ret), "stats/capabilities")
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var got StatsCapabilities
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}
	short := read(domain.Retention{Log: 2, ErrLog: 7, Stats: 180})
	long := read(domain.Retention{Log: 30, ErrLog: 7, Stats: 180})

	require.Equal(t, 2, short.Kinds["trend"].CoverageDays["raw"])
	require.Equal(t, 7, long.Kinds["trend"].CoverageDays["raw"], "保留期调长 → coverage 随动")
	// 统计面 raw 读 usage_logs + err_logs **两张表** ⇒ 取更保守者（min(30,7)=7）；
	// 而 errlog_list 只读 err_logs ⇒ 恒为 ErrLog（7）——"按读的表取，不取全局 min"
	// 在同一个响应里的可见证据（spec §4.4(a)）。
	require.Equal(t, 7, long.Kinds["errlog_list"].CoverageDays["raw"])
	require.Equal(t, 7, short.Kinds["errlog_list"].CoverageDays["raw"])
	require.Equal(t, short.Kinds["trend"].CostCapSeconds["raw"], long.Kinds["trend"].CostCapSeconds["raw"],
		"cost cap 是保留期无关的纯常量")
	require.NotZero(t, long.Kinds["trend"].CostCapSeconds["raw"])
	// 关闭保留期 ⇒ coverage_days 归零（守卫关闭 = 无覆盖下限），cost 仍不变。
	off := read(domain.Retention{})
	require.Zero(t, off.Kinds["trend"].CoverageDays["raw"])
	require.Equal(t, short.Kinds["trend"].CostCapSeconds["raw"], off.Kinds["trend"].CostCapSeconds["raw"])
}
