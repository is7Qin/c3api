// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// fakeReloader 记录规则引擎 Reload 调用（invalidate 钩子断言用）。
type fakeReloader struct{ calls int }

func (f *fakeReloader) Reload(ctx context.Context) error {
	f.calls++
	return nil
}

func newRuleSvc() (*Service, *fakeStore, *fakeReloader) {
	fs := newFakeStore()
	rl := &fakeReloader{}
	return &Service{store: fs, ruleReload: rl}, fs, rl
}

// fakeRuleReloader 函数式 RuleReloader（B4-4 断言注入）。
type fakeRuleReloader func(ctx context.Context) error

func (f fakeRuleReloader) Reload(ctx context.Context) error { return f(ctx) }

// TestReloadRulesSurvivesRequestCancel B4-4（p2-12）：请求 ctx 已取消（客户端
// 断开）→ 规则重载仍必须执行完成——reloadRules 用 context.WithoutCancel 剥离
// 取消信号（与 publish 同纪律 service.go:320）；且重载拿到的 ctx 不可取消。
func TestReloadRulesSurvivesRequestCancel(t *testing.T) {
	called := make(chan struct{})
	svc := &Service{ruleReload: fakeRuleReloader(func(rc context.Context) error {
		require.NoError(t, rc.Err(), "WithoutCancel 后重载 ctx 不可取消（B4-4）")
		close(called)
		return nil
	})}
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel() // 请求 ctx 已取消（客户端断开）
	svc.reloadRules(reqCtx)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("请求 ctx 取消后规则重载必须仍执行完成（B4-4）")
	}
}

func validWhen() map[string]any { return map[string]any{"kind": "5xx"} }
func validThen() map[string]any { return map[string]any{"status": "unhealthy", "cooldown": "5s"} }

func TestCreateRule(t *testing.T) {
	svc, _, rl := newRuleSvc()
	got, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "r1", Enabled: true, Priority: 10, When: validWhen(), Then: validThen(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), got.ID)
	require.Equal(t, "r1", got.Name)
	require.True(t, got.Enabled)
	require.Equal(t, "5xx", *got.When.Kind)
	require.Equal(t, domain.StatusUnhealthy, *got.Then.Status)
	require.Equal(t, "5s", *got.Then.Cooldown)
	require.Equal(t, 1, rl.calls, "规则创建后必须触发引擎 Reload")

	// response_code/custom_message 契约 round-trip（指针意图，seed-400 nil/nil 形态直插，普通规则需显式 ResponseCode/CustomMessage）
	got2, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "tx", Priority: 20, When: map[string]any{"kind": "4xx", "http_status": 400},
		Then: map[string]any{"response_code": 400, "custom_message": "bad request passthrough"},
	})
	require.NoError(t, err, "ResponseCode/CustomMessage 规则通过 ValidateThen")
	require.NotNil(t, got2.Then.ResponseCode)
	require.Equal(t, 400, *got2.Then.ResponseCode)
	require.NotNil(t, got2.Then.CustomMessage)
	require.Equal(t, "bad request passthrough", *got2.Then.CustomMessage)
	require.Nil(t, got2.Then.Status)
}

func TestCreateRuleRejectsUnknownWhenKey(t *testing.T) {
	svc, _, rl := newRuleSvc()
	_, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "r1", Priority: 10,
		When: map[string]any{"kind": "5xx", "bogus_key": 1},
		Then: validThen(),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "when 未知键 → 400 语义")
	require.Zero(t, rl.calls, "校验失败不触发 Reload")

	_, err = svc.CreateRule(context.Background(), RuleInput{
		Name: "r2", Priority: 11, When: validWhen(),
		Then: map[string]any{"status": "unhealthy", "bogus": true},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "then 未知键 → 400 语义")
}

func TestCreateRuleInvalidThen(t *testing.T) {
	svc, _, _ := newRuleSvc()
	// Then{} 空=纯透传合法（R-4）
	got, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "r1", Priority: 10, When: validWhen(), Then: map[string]any{},
	})
	require.NoError(t, err, "Then{} 纯透传合法")
	require.Nil(t, got.Then.Status)
	require.Nil(t, got.Then.ResponseCode)
	require.Nil(t, got.Then.CustomMessage)
	_, err = svc.CreateRule(context.Background(), RuleInput{
		Name: "r2", Priority: 11, When: validWhen(), Then: map[string]any{"cooldown": "0s"},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "cooldown ≤ 0 → 400 语义")
	_, err = svc.CreateRule(context.Background(), RuleInput{
		Name: "r3", Priority: 12, When: map[string]any{"ratio_429_ge": 0.5}, Then: validThen(),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "比例缺 count_total_ge → 400 语义")
}

func TestCreateRuleNameRequired(t *testing.T) {
	svc, _, rl := newRuleSvc()
	_, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "", Priority: 10, When: validWhen(), Then: validThen(),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "name 空 → 400 语义")
	_, err = svc.CreateRule(context.Background(), RuleInput{
		Name: "   ", Priority: 11, When: validWhen(), Then: validThen(),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "name 全空白 → 400 语义")
	require.Zero(t, rl.calls, "校验失败不触发 Reload")
}

func TestCreateRulePriorityConflict(t *testing.T) {
	svc, _, rl := newRuleSvc()
	_, err := svc.CreateRule(context.Background(), RuleInput{Name: "r1", Priority: 10, When: validWhen(), Then: validThen()})
	require.NoError(t, err)
	_, err = svc.CreateRule(context.Background(), RuleInput{Name: "r2", Priority: 10, When: validWhen(), Then: validThen()})
	require.ErrorIs(t, err, ErrConflict, "priority 唯一冲突 → ErrConflict（409 语义）")
	require.Contains(t, err.Error(), `priority=10 or name="r2"`, "409 消息含冲突详情（G1-2）")
	_, err = svc.CreateRule(context.Background(), RuleInput{Name: "r1", Priority: 20, When: validWhen(), Then: validThen()})
	require.ErrorIs(t, err, ErrConflict, "name 唯一冲突同样映射 ErrConflict")
	require.Contains(t, err.Error(), `name="r1"`, "409 消息含冲突详情（G1-2）")
	require.Equal(t, 1, rl.calls, "冲突失败不触发 Reload")
}

func TestUpdateRuleMerge(t *testing.T) {
	svc, _, rl := newRuleSvc()
	created, err := svc.CreateRule(context.Background(), RuleInput{
		Name: "r1", Enabled: false, Priority: 10, When: validWhen(), Then: validThen(),
	})
	require.NoError(t, err)

	// 部分更新：未提供字段保持原值
	name := "r1-renamed"
	updated, err := svc.UpdateRule(context.Background(), created.ID, RulePatch{Name: &name})
	require.NoError(t, err)
	require.Equal(t, "r1-renamed", updated.Name)
	require.False(t, updated.Enabled, "未提供字段保持原值")
	require.Equal(t, 10, updated.Priority)
	require.Equal(t, "5xx", *updated.When.Kind, "when 未提供保持原值")
	require.Equal(t, 2, rl.calls, "规则更新后必须触发引擎 Reload")

	// when 更新：未知键拒绝
	_, err = svc.UpdateRule(context.Background(), created.ID, RulePatch{When: map[string]any{"bogus": 1}})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Equal(t, 2, rl.calls, "校验失败不触发 Reload")

	// 显式 {} 清空 when（D-M2）：非 nil 空 map = 整体替换为空 when（匹配一切）
	cleared, err := svc.UpdateRule(context.Background(), created.ID, RulePatch{When: map[string]any{}})
	require.NoError(t, err)
	require.Nil(t, cleared.When.Kind, "显式 {} 清空 when")
	require.Equal(t, "r1-renamed", cleared.Name, "其他字段不受影响")
	require.Equal(t, 3, rl.calls, "清空 when 同样触发 Reload")

	// 404 含 id
	_, err = svc.UpdateRule(context.Background(), 999, RulePatch{})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")

	// name 更新为空 → 400 语义
	emptyName := ""
	_, err = svc.UpdateRule(context.Background(), created.ID, RulePatch{Name: &emptyName})
	require.ErrorIs(t, err, ErrInvalidInput, "name 更新为空 → 400 语义")

	// priority 冲突 → ErrConflict
	_, err = svc.CreateRule(context.Background(), RuleInput{Name: "r2", Priority: 20, When: validWhen(), Then: validThen()})
	require.NoError(t, err)
	p := 20
	_, err = svc.UpdateRule(context.Background(), created.ID, RulePatch{Priority: &p})
	require.ErrorIs(t, err, ErrConflict)
	require.Contains(t, err.Error(), `priority=20`, "更新路径 409 消息同样含冲突详情（G1-2）")
	require.Equal(t, 4, rl.calls, "冲突失败不触发 Reload")
}

// TestMapRuleRepoErrConflict mapRuleRepoErr 的 ErrConflict 分支：repository.ErrConflict →
// service.ErrConflict（保留冲突详情，handler 409 响应带详情——G1-2 对齐 mapRepoErr）；
// 非冲突错误原样透传。
func TestMapRuleRepoErrConflict(t *testing.T) {
	err := fmt.Errorf("%w: priority=%d or name=%q", repository.ErrConflict, 10, "r2")
	mapped := mapRuleRepoErr(err)
	require.ErrorIs(t, mapped, ErrConflict, "repository.ErrConflict → service.ErrConflict")
	require.Contains(t, mapped.Error(), `priority=10 or name="r2"`, "409 消息保留冲突详情")

	raw := errors.New("boom")
	require.Same(t, raw, mapRuleRepoErr(raw), "非映射错误原样透传")
}

func TestDeleteRule(t *testing.T) {
	svc, _, rl := newRuleSvc()
	created, err := svc.CreateRule(context.Background(), RuleInput{Name: "r1", Priority: 10, When: validWhen(), Then: validThen()})
	require.NoError(t, err)
	require.NoError(t, svc.DeleteRule(context.Background(), created.ID))
	require.Equal(t, 2, rl.calls, "删除后必须触发引擎 Reload")

	err = svc.DeleteRule(context.Background(), 999)
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing", "404 消息含 id")
	require.Equal(t, 2, rl.calls, "404 不触发 Reload")
}

func TestListRules(t *testing.T) {
	svc, _, _ := newRuleSvc()
	for _, p := range []int{30, 10, 20} {
		_, err := svc.CreateRule(context.Background(), RuleInput{
			Name: fmt.Sprintf("r%d", p), Enabled: p != 20, Priority: p, When: validWhen(), Then: validThen(),
		})
		require.NoError(t, err)
	}
	rows, total, err := svc.ListRules(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Equal(t, []int{10, 20, 30}, []int{rows[0].Priority, rows[1].Priority, rows[2].Priority},
		"priority 升序")

	enabled := true
	rows, total, err = svc.ListRules(context.Background(), &enabled)
	require.NoError(t, err)
	require.Equal(t, int64(2), total, "enabled 过滤")
	require.Equal(t, []int{10, 30}, []int{rows[0].Priority, rows[1].Priority})

	disabled := false
	rows, total, err = svc.ListRules(context.Background(), &disabled)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, 20, rows[0].Priority)
}
