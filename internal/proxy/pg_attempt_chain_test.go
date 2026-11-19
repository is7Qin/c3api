// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 请求链的端到端验收（spec 2026-11-18 retry-observability §6 S3 **9**）：
// 数据必须来自**真实写路径**（HandleChat → failover 循环 → attempt_logs 写点 /
// err_logs 写点 → worker → PG），然后经 repository 三源读 + service 合并成链。
//
// 断言的三件事：
//  1. 同一 attempt 同时存在于 attempt_logs（ordinal=2）与 err_logs（耗尽行，
//     attempt_ordinal=2）⇒ 链里只出现一次，且取的是 **attempt 行**（lane 非空、
//     status_code 来自 attempt 腿——err 行没有 lane，回填会立刻暴露）；
//  2. err_logs 的 NULL ordinal 行（本地 401 拒绝）照常出现并标 ordinal_unknown；
//  3. 成功请求的 usage 终态行：恒在最后、source=usage、ordinal=null，但
//     **ordinal_unknown=false**——「按设计没有序号」不等于「序号未知」。
//
// 禁止手搓 err_logs 行（§4「这条列必须真的有人写」的验收纪律）：本用例里每一行
// 都是 chatOnce 跑出来的；用例只从库里**读回** request_id 来定位链。

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// attemptChainEmailCodes service.New 要求非 nil 的验证码存储；本用例不碰验证码，
// 四个方法返回零值即可（构造期 panic 的唯一触发条件是 nil）。
type attemptChainEmailCodes struct{}

func (attemptChainEmailCodes) GetEmailCode(context.Context, string, string) (*domain.EmailCode, error) {
	return nil, nil
}

func (attemptChainEmailCodes) UpsertEmailCode(context.Context, string, string, string, time.Time) (*domain.EmailCode, error) {
	return nil, nil
}

func (attemptChainEmailCodes) IncrementEmailCodeAttempts(context.Context, string, string) (int, error) {
	return 0, nil
}

func (attemptChainEmailCodes) DeleteEmailCode(context.Context, string, string) error { return nil }

// TestAttemptChainEndToEndPG 真实 failover 造数据 → 三源读 → 合并链。
func TestAttemptChainEndToEndPG(t *testing.T) {
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	now := time.Now()
	require.NoError(t, repos.EnsureAllTablePartitions(ctx, now, now.AddDate(0, 0, 1)))

	// 上游：前两次 500（耗尽），之后 200（成功请求 → usage 终态行）。
	// 脚本按命中次数推进，故 ① 耗尽（2 跳）、② 本地拒绝（0 跳，不打上游）、
	// ③ 成功（1 跳）三步是确定的。
	up := scriptedChatUpstreamDelayed(t, 0, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)

	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "chain-g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	for i := int64(1); i <= 2; i++ {
		suffix := fmt.Sprintf("%d", i)
		tpl, terr := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "chain-t" + suffix, BaseURL: up.URL,
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			Models:           []string{"gpt-4o"},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, terr)
		acc, aerr := repos.Accounts.CreateAccount(ctx, &domain.Account{
			Name: "chain-a" + suffix, TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
			MaxConcurrency: 4, Enabled: true,
		})
		require.NoError(t, aerr)
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	}

	re := rule.New(rule.Config{}, repos.Rules, nil, nil, nil)
	require.NoError(t, re.Reload(ctx))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, repos.Groups, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, repos.Usages, nil)
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, repos.ErrLogs, nil)
	attemptW, aerr := usage.NewAttemptLogWorker(usage.AttemptLogConfig{}, repos.AttemptLogs, nil)
	require.NoError(t, aerr)

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, g.ID)}},
		noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(ctx))
	px := New(Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture: true,
	}, sched, credential.New(), rec,
		aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport},
			aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second}),
		auth, nil, nil, errlogW, Deps{})
	px.SetAttemptLogs(attemptW)

	require.Equal(t, http.StatusBadGateway, chatOnce(px, "ck-1").Code, "两次 5xx ⇒ 耗尽")
	require.Equal(t, http.StatusUnauthorized, chatOnce(px, "ck-invalid").Code, "本地拒绝（无 attempt 语义）")
	require.Equal(t, http.StatusOK, chatOnce(px, "ck-1").Code, "第三次：上游正常 ⇒ 成功（产生 usage 终态行）")

	// 排空三条通道（真实 worker 的 Close 排空，不做 sleep）。
	require.NoError(t, attemptW.Close(ctx))
	require.NoError(t, errlogW.Close(ctx))
	require.NoError(t, rec.Close(ctx))

	// 从库里读回两个 request_id（**读**自己写出来的行，不造行）。
	var exhaustedID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT request_id FROM err_logs WHERE status_code = 500 AND attempt_ordinal = 2`).Scan(&exhaustedID))
	var rejectedID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT request_id FROM err_logs WHERE status_code = 401 AND attempt_ordinal IS NULL`).Scan(&rejectedID))
	require.NotEqual(t, exhaustedID, rejectedID, "两次调用是两个 request")

	// 成功那次调用的 request_id 也从库里读回（error_type='none' 的行只有它）。
	var successID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT request_id FROM usage_logs WHERE error_type = 'none'`).Scan(&successID))
	require.NotEqual(t, exhaustedID, successID)
	require.NotEqual(t, rejectedID, successID)

	svc := service.New(service.Deps{Store: repos, Invalidate: service.NopInvalidator{}, EmailCodeStore: attemptChainEmailCodes{}})

	// ① 耗尽请求：两跳 attempt（ordinal 1/2）+ usage 终态；err_logs 的 ordinal=2
	// 行被吸收（去重后只剩一跳 ordinal=2，且是 attempt 行）。
	chain, err := svc.QueryAttemptChain(ctx, service.AttemptChainQuery{RequestID: exhaustedID})
	require.NoError(t, err)
	require.Equal(t, exhaustedID, chain.RequestID)
	require.False(t, chain.Truncated)

	ordinals := make([]int, 0, len(chain.Hops))
	ordinalSources := map[int]string{}
	for _, hop := range chain.Hops {
		if hop.Ordinal == nil {
			continue
		}
		ordinals = append(ordinals, int(*hop.Ordinal))
		ordinalSources[int(*hop.Ordinal)] = hop.Source
	}
	require.Equal(t, []int{1, 2}, ordinals, "链上恰好两次尝试，且按 ordinal 升序（err 行已去重）")
	require.Equal(t, "attempt", ordinalSources[1])
	require.Equal(t, "attempt", ordinalSources[2], "同 ordinal 的 err 行必须让位给 attempt 行")

	for _, hop := range chain.Hops {
		if hop.Ordinal == nil || *hop.Ordinal != 2 {
			continue
		}
		// err 行没有 lane；lane 非空 + status_code=500 证明这一跳来自 attempt_logs。
		require.Equal(t, "attempt", hop.Source)
		require.NotEmpty(t, hop.Lane, "attempt 行有 lane；发现空 lane 说明链上留下的是 err 行")
		require.Equal(t, http.StatusInternalServerError, hop.StatusCode)
		require.Equal(t, "5xx", hop.Outcome, "outcome 只有 attempt_logs 有")
		require.False(t, hop.OrdinalUnknown)
	}
	// usage 终态恒在最后：一旦出现 usage 行，其后不得再有 attempt/err 行
	// （耗尽路径不落 usage 行——那是计费路径的事，故这里只钉顺序；真实 usage
	// 跳的断言在下一段用成功请求做）。
	seenUsage := false
	for _, hop := range chain.Hops {
		if hop.Source == "usage" {
			seenUsage = true
			continue
		}
		require.False(t, seenUsage, "usage 终态行之后不得再有 attempt/err 行")
	}

	// ② 本地拒绝请求：一条 err 行、ordinal NULL、ordinal_unknown=true。
	rejected, err := svc.QueryAttemptChain(ctx, service.AttemptChainQuery{RequestID: rejectedID})
	require.NoError(t, err)
	require.Len(t, rejected.Hops, 1)
	require.Equal(t, "err", rejected.Hops[0].Source)
	require.Nil(t, rejected.Hops[0].Ordinal)
	require.True(t, rejected.Hops[0].OrdinalUnknown)
	require.Equal(t, http.StatusUnauthorized, rejected.Hops[0].StatusCode)

	// ③ 成功请求：链上只有 usage 终态行。usage_logs 按设计**没有**序号
	// （ordinal=null），但 ordinal_unknown 只描述「序号未知」的 err 行——
	// 终态行必须显式 false（把 usage 行也标 unknown 这里就红）。
	succeeded, err := svc.QueryAttemptChain(ctx, service.AttemptChainQuery{RequestID: successID})
	require.NoError(t, err)
	require.Len(t, succeeded.Hops, 1, "成功请求只有一跳：usage 终态")
	usageHop := succeeded.Hops[0]
	require.Equal(t, "usage", usageHop.Source)
	require.Nil(t, usageHop.Ordinal, "usage_logs 没有序号列")
	require.False(t, usageHop.OrdinalUnknown, "usage 终态行没有序号，但不是 unknown")

	// ④ 窗口强制（spec §6 S3-10）：窗口外的 request_id ⇒ 空链 + 不报错。
	outside := now.AddDate(0, 0, -8)
	stale, err := svc.QueryAttemptChain(ctx, service.AttemptChainQuery{
		RequestID: exhaustedID, From: &outside, To: ptrTime(outside.Add(24 * time.Hour)),
	})
	require.NoError(t, err)
	require.NotNil(t, stale.Hops, "空链必须是 [] 而不是 nil")
	require.Empty(t, stale.Hops, "8 天前的窗口查不到（保留期 7 天）——空链而不是错误")
}

// ptrTime 取时间指针（窗口参数是 *time.Time）。
func ptrTime(v time.Time) *time.Time { return &v }
