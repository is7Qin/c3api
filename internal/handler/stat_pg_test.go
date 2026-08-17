// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// /admin/stats + /user/stats 真实 PG 测试（rewrite spec 2026-08-14 测试节）：
//   - 桶数组响应含全部新字段（TTFT 六指标插值数值断言——已知分布 + TTFTCount；
//     call_count；Cost USD 口径——毫分 /1e5）
//   - granularity=day：Go map 合并 24 小时直方图 + 插值断言（TTFT 四字段）
//   - template_id 过滤断言（两端点——前置清单①接线验证）
//   - /user/stats 越权回归（非本人 user_id 过滤参数无效）
//   - 无样本（空区间）：TTFT 全 0、Cost 0、CallCount 0
//   - 顶桶回落（>12800ms 样本 → pN = 12800）
//
// 基座复用 overview_pg_test.go：overviewPGTestDB（独立 schema 重建）/
// overviewSeedBuckets（AggregateRange 覆盖落盘）——同小时多维度行必须单次
// AggregateRange 调用内一并种子（DELETE [bt,bt+1h) 覆盖语义）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// statBucketWithTTFT 统计桶测试种子（TTFT 直方图 10 档；cost 毫分）。
func statBucketWithTTFT(bt time.Time, groupID, userID int64, req, call, cost int64,
	ttftTotal, ttftCount, ttftMax int64, hist []int64) *domain.StatBucket {
	return &domain.StatBucket{
		BucketTime: bt, GroupID: groupID, AccountID: 42, UserID: userID,
		Model: "gpt-4o", RequestCount: req, TotalTokens: req * 3,
		Cost: cost, CallCount: call,
		TTFTTotalMS: ttftTotal, TTFTCount: ttftCount, TTFTMaxMS: ttftMax, TTFTHist: hist,
	}
}

// TestPGStatsEndpointBucketFields /admin/stats 桶数组全字段（hour 粒度不合并）：
// 同小时两维度行各自携带已知 TTFT 分布（hist {6,4} N=5）——p50/p90/p95/p99
// 桶内线性插值数值断言 + TTFTCount + call_count + Cost USD（毫分 /1e5）。
func TestPGStatsEndpointBucketFields(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	hist := []int64{6, 4, 0, 0, 0, 0, 0, 0, 0, 0}
	// 同小时两维度行（group 1/2）单次 AggregateRange 种子
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{
			statBucketWithTTFT(bucket, 1, 42, 5, 3, 100_000, 300, 5, 90, hist),
			statBucketWithTTFT(bucket, 2, 42, 5, 2, 50_000, 180, 5, 60, hist),
		}))

	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})
	w := router("GET", fmt.Sprintf("/admin/stats?granularity=hour&from=%s&to=%s",
		bucket.Format(time.RFC3339), bucket.Add(time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w.Code, "stats: %s", w.Body.String())
	var out []StatBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 2, "hour 粒度两维度行原样返回")

	byGroup := map[int64]StatBucket{}
	for _, b := range out {
		byGroup[*b.GroupID] = b
	}
	// 行 1：hist {6,4} N=5 → p50: rank3 → 0+3/6×50=25；p90/p95/p99: rank5 → 41；
	// avg=300/5=60；Cost 100000 毫分 → USD 1.0
	b1 := byGroup[1]
	require.Equal(t, int64(5), *b1.TTFTCount)
	require.Equal(t, int64(3), *b1.CallCount, "call_count 随桶返回")
	require.InDelta(t, 1.0, *b1.Cost, 1e-9, "Cost 毫分 /1e5 → USD")
	require.InDelta(t, 60.0, *b1.TTFTAvgMS, 1e-9, "avg = sum/count")
	require.Equal(t, int64(90), *b1.TTFTMaxMS)
	require.Equal(t, int64(25), *b1.TTFTP50MS)
	require.Equal(t, int64(41), *b1.TTFTP90MS)
	require.Equal(t, int64(41), *b1.TTFTP95MS)
	require.Equal(t, int64(41), *b1.TTFTP99MS)
	// 行 2：avg=180/5=36；Cost → USD 0.5
	b2 := byGroup[2]
	require.InDelta(t, 36.0, *b2.TTFTAvgMS, 1e-9)
	require.InDelta(t, 0.5, *b2.Cost, 1e-9)
	require.Equal(t, int64(60), *b2.TTFTMaxMS)
}

// TestPGStatsEndpointDayGranularity granularity=day：Go map 合并（service
// QueryStats）——同维度（group/account/template/user/model/isError 一致）两小时
// 桶按 BucketTime 合并后 24h 直方图 {12,8} N=10 插值：
// avg=(300+180)/10=48、max=90、p50: rank5 → 0+5/12×50≈20、p90: rank9 → 37、
// p95/p99: rank10 → 41（与 TestPGStatsSummarizeTTFT 合并数学一致）。
func TestPGStatsEndpointDayGranularity(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	h0 := now.Truncate(time.Hour)
	hist := []int64{6, 4, 0, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, repos.Stats.AggregateRange(ctx, h0, h0.Add(2*time.Hour), h0.Add(30*time.Minute),
		[]*domain.StatBucket{
			statBucketWithTTFT(h0, 1, 42, 5, 3, 100_000, 300, 5, 90, hist),
			statBucketWithTTFT(h0.Add(time.Hour), 1, 42, 5, 2, 50_000, 180, 5, 60, hist),
		}))

	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})
	w := router("GET", fmt.Sprintf("/admin/stats?granularity=day&from=%s&to=%s",
		h0.Format(time.RFC3339), h0.Add(2*time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w.Code, "stats: %s", w.Body.String())
	var out []StatBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1, "day 粒度跨维度行按 BucketTime 合并")
	b := out[0]
	require.Equal(t, int64(10), *b.RequestCount)
	require.Equal(t, int64(5), *b.CallCount, "call_count 日合并求和")
	require.InDelta(t, 1.5, *b.Cost, 1e-9, "(100000+50000)/1e5")
	require.Equal(t, int64(10), *b.TTFTCount, "TTFTCount 日合并")
	require.InDelta(t, 48.0, *b.TTFTAvgMS, 1e-9, "(300+180)/10")
	require.Equal(t, int64(90), *b.TTFTMaxMS, "max 取大")
	require.Equal(t, int64(20), *b.TTFTP50MS)
	require.Equal(t, int64(37), *b.TTFTP90MS)
	require.Equal(t, int64(41), *b.TTFTP95MS)
	require.Equal(t, int64(41), *b.TTFTP99MS)
}

// TestPGStatsTemplateFilterBothEndpoints template_id 过滤接线验证（前置清单①）：
// 同小时 TemplateID 0 与 5 两行种子 → /admin/stats 与 /user/stats 的
// template_id 参数均只回命中行。
func TestPGStatsTemplateFilterBothEndpoints(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	// 用户先建（usage_stats 无外键，但 /user/stats 强制 user_id = 本人——桶的
	// user_id 必须等于真实用户 ID 才能命中）
	user, err := repos.Users.CreateUser(ctx, &domain.User{Email: "u42@x.test", PasswordHash: "h",
		Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{
			statBucketWithTTFT(bucket, 1, user.ID, 1, 0, 0, 0, 0, 0, make([]int64, 10)),
			{BucketTime: bucket, GroupID: 1, AccountID: 42, UserID: user.ID, Model: "gpt-4o",
				TemplateID: 5, RequestCount: 7, TTFTHist: make([]int64, 10)},
		}))

	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})
	w := router("GET", fmt.Sprintf("/admin/stats?granularity=hour&template_id=5&from=%s&to=%s",
		bucket.Format(time.RFC3339), bucket.Add(time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w.Code)
	var out []StatBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1, "template_id=5 只回命中行")
	require.Equal(t, int64(5), *out[0].TemplateID)
	require.Equal(t, int64(7), *out[0].RequestCount)

	// /user/stats 同参数形态（真实用户 token）
	iss := auth.NewIssuer("test-secret")
	token, err := iss.Issue(user.ID, user.Email, string(user.Role))
	require.NoError(t, err)
	svc := service.New(repos, stubSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	ur := userapi.Router(svc, iss, pgUserStatus{repos: repos}, nil)
	ureq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/user/stats?granularity=hour&template_id=5&from=%s&to=%s",
		bucket.Format(time.RFC3339), bucket.Add(time.Hour).Format(time.RFC3339)), nil)
	ureq.Header.Set("Authorization", "Bearer "+token)
	urec := httptest.NewRecorder()
	ur.ServeHTTP(urec, ureq)
	require.Equal(t, http.StatusOK, urec.Code, "user stats: %s", urec.Body.String())
	var uout []userapi.StatBucket
	require.NoError(t, json.Unmarshal(urec.Body.Bytes(), &uout))
	require.Len(t, uout, 1, "/user/stats template_id=5 只回命中行")
	require.Equal(t, int64(5), *uout[0].TemplateID)
}

// pgUserStatus 真实 PG 用户快照 provider（RequireJWT 快照校验；fail-closed；
// status+role 单次查找）。
type pgUserStatus struct{ repos *repository.Repository }

func (p pgUserStatus) UserSnapshot(userID int64) (domain.UserSnapshot, bool) {
	u, err := p.repos.Users.GetUser(context.Background(), userID)
	if err != nil {
		return domain.UserSnapshot{}, false
	}
	return domain.UserSnapshot{Status: u.Status, Role: u.Role}, true
}

// TestPGUserStatsForcedOwnUser 越权回归：/user/stats 强制 user_id = 当前用户——
// query 里带他人 user_id 被忽略（oapi-codegen 未声明参数不解析），响应只含
// 本人行。
func TestPGUserStatsForcedOwnUser(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	ua, err := repos.Users.CreateUser(ctx, &domain.User{Email: "ua@x.test", PasswordHash: "h",
		Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	ub, err := repos.Users.CreateUser(ctx, &domain.User{Email: "ub@x.test", PasswordHash: "h",
		Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{
			statBucketWithTTFT(bucket, 1, ua.ID, 3, 1, 10_000, 0, 0, 0, make([]int64, 10)),
			statBucketWithTTFT(bucket, 2, ub.ID, 9, 2, 20_000, 0, 0, 0, make([]int64, 10)),
		}))

	iss := auth.NewIssuer("test-secret")
	svc := service.New(repos, stubSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	ur := userapi.Router(svc, iss, pgUserStatus{repos: repos}, nil)
	tokenA, err := iss.Issue(ua.ID, ua.Email, string(ua.Role))
	require.NoError(t, err)
	tokenB, err := iss.Issue(ub.ID, ub.Email, string(ub.Role))
	require.NoError(t, err)

	do := func(token string, qs string) []userapi.StatBucket {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/user/stats?granularity=hour&from="+bucket.Format(time.RFC3339)+
			"&to="+bucket.Add(time.Hour).Format(time.RFC3339)+qs, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		ur.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "user stats: %s", rec.Body.String())
		var out []userapi.StatBucket
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	// A 的请求带 user_id=<B> → 参数无效，只回 A 的行
	out := do(tokenA, "&user_id="+fmt.Sprint(ub.ID))
	require.Len(t, out, 1, "非本人 user_id 过滤参数无效")
	require.Equal(t, ua.ID, *out[0].UserID)
	require.Equal(t, int64(3), *out[0].RequestCount)
	// B 同样只见自己的行（正向）
	out = do(tokenB, "")
	require.Len(t, out, 1)
	require.Equal(t, ub.ID, *out[0].UserID)
	require.Equal(t, int64(9), *out[0].RequestCount)
}

// TestPGStatsEndpointNoSample 无样本语义：仅有请求量无 TTFT 样本/费用/调用量
// 的桶 → TTFT 全 0、Cost 0、CallCount 0；空区间 → 200 空数组。
func TestPGStatsEndpointNoSample(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{statBucketWithTTFT(bucket, 1, 42, 3, 0, 0, 0, 0, 0, make([]int64, 10))}))

	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})
	w := router("GET", fmt.Sprintf("/admin/stats?granularity=hour&from=%s&to=%s",
		bucket.Format(time.RFC3339), bucket.Add(time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w.Code)
	var out []StatBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	b := out[0]
	require.Equal(t, int64(3), *b.RequestCount)
	require.Equal(t, int64(0), *b.CallCount, "无按次调用 → 0")
	require.InDelta(t, 0.0, *b.Cost, 1e-9, "无费用 → USD 0")
	require.Equal(t, int64(0), *b.TTFTCount, "无样本 → TTFTCount 0")
	require.InDelta(t, 0.0, *b.TTFTAvgMS, 1e-9, "无样本 → avg 0")
	require.Equal(t, int64(0), *b.TTFTMaxMS, "无样本 → max 0")
	require.Equal(t, int64(0), *b.TTFTP50MS, "无样本 → pN 0")
	require.Equal(t, int64(0), *b.TTFTP99MS, "无样本 → pN 0")

	// 空区间：200 空数组（无 42703/扫描错误）
	w2 := router("GET", fmt.Sprintf("/admin/stats?granularity=hour&from=%s&to=%s",
		bucket.Add(2*time.Hour).Format(time.RFC3339), bucket.Add(3*time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w2.Code, "empty range: %s", w2.Body.String())
	require.JSONEq(t, "[]", w2.Body.String(), "空区间 → 空数组")
}

// TestPGStatsEndpointTopBucketFallback 顶桶回落：样本全落顶桶（12800+）→
// pN = 12800（顶桶无上界不可插值，保守下限口径）。
func TestPGStatsEndpointTopBucketFallback(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC(), time.Now().UTC())
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	hist := []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 5}
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{statBucketWithTTFT(bucket, 1, 42, 5, 0, 0, 65_000, 5, 20_000, hist)}))

	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})
	w := router("GET", fmt.Sprintf("/admin/stats?granularity=hour&from=%s&to=%s",
		bucket.Format(time.RFC3339), bucket.Add(time.Hour).Format(time.RFC3339)))
	require.Equal(t, http.StatusOK, w.Code)
	var out []StatBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	b := out[0]
	require.Equal(t, int64(12800), *b.TTFTP50MS, "顶桶回落 12800")
	require.Equal(t, int64(12800), *b.TTFTP90MS)
	require.Equal(t, int64(12800), *b.TTFTP95MS)
	require.Equal(t, int64(12800), *b.TTFTP99MS)
	require.InDelta(t, 13_000.0, *b.TTFTAvgMS, 1e-9, "avg 不受顶桶回落影响（65000/5）")
	require.Equal(t, int64(20_000), *b.TTFTMaxMS, "max 原值")
}
