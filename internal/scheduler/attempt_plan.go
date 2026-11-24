// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"strconv"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
)

// MaxAttemptPlanAccounts 是跨账号 attempted-ID 去重槽位数与数组界，来源唯一：
// domain.MaxFailoverAttempts（配置校验的同一硬上限）。
const MaxAttemptPlanAccounts = domain.MaxFailoverAttempts

// 一次派发的**值契约**的 canonical 定义在 internal/api（P3c II）：Attempt /
// AttemptPlanIdentity / AttemptLane 的定义体见 api/attempt.go，RouteRef 见
// api/route_ref.go。本包只留别名与三条常量值别名，理由是迁移面——Attempt 在数据面
// 有 26 处纯传值引用、AttemptPlanIdentity 有 137 行测试引用，别名让它们不必一次性
// 改名（旧拼写仍编译、仍是同一类型）。
//
// **这里是兼容层，不是第二份定义**：本包不得再出现这四个名字的 StructType 或方法
// 声明（Validate / Valid 只许在 api）。判据是机械的，见
// internal/api/type_uniqueness_gate_test.go（AST 逐文件扫 + lane 常量逐名逐值相等）；
// 靠"两边都登记代际门禁"兜不住——那条门禁只认 *ast.StructType，给别名登记会直接红。
type AttemptPlanIdentity = api.AttemptPlanIdentity

type AttemptLane = api.AttemptLane

// 三条 lane 常量**必须**逐条指向 api 的同名常量：写成 `AttemptLane = "primary"`
// 这类字面量也是合法 Go，但常量集合会裂成两套——build 绿、现有门禁绿，运行时却被
// api.Attempt.Validate 以 "Lane must be valid" 判死。故值一律取别名而非复制。
const (
	AttemptLanePrimary  AttemptLane = api.AttemptLanePrimary
	AttemptLaneExplore  AttemptLane = api.AttemptLaneExplore
	AttemptLaneDegraded AttemptLane = api.AttemptLaneDegraded
)

type Attempt = api.Attempt

type AttemptReservation func(accountID int64) bool

// fnvOffset64 / fnvPrime64 are the standard FNV-1a 64-bit offset basis and
// prime. Single source of truth for every FNV site in this package（测试基准除外）.
const fnvOffset64 = 14695981039346656037
const fnvPrime64 = 1099511628211

func ExploreHash(label, requestID string, userID int64, routeClassID string, generation uint64, ordinal uint8) uint64 {
	h := uint64(fnvOffset64)
	writeString := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= fnvPrime64
		}
		h *= fnvPrime64
	}
	writeString(label)
	writeString(requestID)
	writeString(strconv.FormatInt(userID, 10))
	writeString(routeClassID)
	writeString(strconv.FormatUint(generation, 10))
	writeString(strconv.FormatUint(uint64(ordinal), 10))
	return h
}

func exploreHashForPlan(identity AttemptPlanIdentity, ordinal uint8) uint64 {
	return ExploreHash("explore", identity.RequestID, identity.UserID, identity.RouteClassID, identity.RoutingGeneration, ordinal)
}

// laneHashForPlan draws the per-request lane decision (charter
// hash(lane)%10000 selects the lane). The "lane" label keeps it an
// independent draw from the "explore" sample ticket: lane pick and sample
// pick never couple. Zero-alloc FNV-1a64 over the canonical identity tuple,
// so all instances decide identically with no seed and no coordination.
func laneHashForPlan(identity AttemptPlanIdentity) uint64 {
	return ExploreHash("lane", identity.RequestID, identity.UserID, identity.RouteClassID, identity.RoutingGeneration, 0)
}
