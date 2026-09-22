// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/logx"
)

// RuleStore 规则存储接口（repository.RuleStore 子集，Service 门面注入用）。
type RuleStore interface {
	ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) // nil = 全部；priority 升序
	CreateRule(ctx context.Context, r domain.Rule) (int64, error)
	UpdateRule(ctx context.Context, r domain.Rule) error
	DeleteRule(ctx context.Context, id int64) error
	DeleteRulesBatch(ctx context.Context, ids []int64) error
	CountRules(ctx context.Context) (int64, error)
}

// RuleInput 规则创建入参（when/then 为契约自由对象，service 层负责
// DisallowUnknownFields 反序列化与语义校验）。
type RuleInput struct {
	Name     string
	Enabled  bool
	Priority int
	When     map[string]any
	Then     map[string]any
}

// RulePatch 规则部分更新（nil 字段 = 不修改）。
type RulePatch struct {
	Name     *string
	Enabled  *bool
	Priority *int
	When     map[string]any // nil = 不修改；显式 {} = 清空 when
	Then     map[string]any
}

// CreateRule 创建规则：name 必填 → when/then 反序列化（未知键拒绝）→ 语义校验 →
// 写入 → 规则引擎 Reload。priority/name 唯一冲突 → ErrConflict（409）。
func (s *Service) CreateRule(ctx context.Context, in RuleInput) (*domain.Rule, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	w, err := ruleWhenFromRaw(in.When)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	t, err := ruleThenFromRaw(in.Then)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := rule.ValidateWhen(w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := rule.ValidateThen(t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	id, err := s.store.CreateRule(ctx, domain.Rule{
		Name: in.Name, Enabled: in.Enabled, Priority: in.Priority, When: w, Then: t,
	})
	if err != nil {
		return nil, mapRuleRepoErr(err)
	}
	s.reloadRules(ctx)
	s.publish(ctx, notify.Change{Rules: true})
	if s.log != nil {
		s.log.Info("rule created", logx.Int64("id", id), logx.String("name", in.Name))
	}
	return s.getRule(ctx, id)
}

// UpdateRule 部分更新规则：未提供字段保持原值；校验合并后的完整 when/then；
// 不存在 → ErrNotFound（消息含 id）。成功后规则引擎 Reload。
func (s *Service) UpdateRule(ctx context.Context, id int64, p RulePatch) (*domain.Rule, error) {
	cur, err := s.getRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Name != nil {
		cur.Name = *p.Name
	}
	if p.Enabled != nil {
		cur.Enabled = *p.Enabled
	}
	if p.Priority != nil {
		cur.Priority = *p.Priority
	}
	if p.When != nil {
		w, err := ruleWhenFromRaw(p.When)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		cur.When = w
	}
	if p.Then != nil {
		t, err := ruleThenFromRaw(p.Then)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		cur.Then = t
	}
	if strings.TrimSpace(cur.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if err := rule.ValidateWhen(cur.When); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := rule.ValidateThen(cur.Then); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.store.UpdateRule(ctx, *cur); err != nil {
		return nil, mapRuleRepoErr(err)
	}
	s.reloadRules(ctx)
	s.publish(ctx, notify.Change{Rules: true})
	return s.getRule(ctx, id)
}

// DeleteRule 删除规则；不存在 → ErrNotFound（消息含 id）。成功后规则引擎 Reload。
func (s *Service) DeleteRule(ctx context.Context, id int64) error {
	if err := mapRuleRepoErr(s.store.DeleteRule(ctx, id)); err != nil {
		return err
	}
	s.reloadRules(ctx)
	s.publish(ctx, notify.Change{Rules: true})
	return nil
}

// DeleteRulesBatch 批量删除规则（事务，全成或全败）；ids 1–100 去重；
// 缺 id → ErrNotFound（消息含缺失 id）。成功后规则引擎 Reload。
func (s *Service) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := mapRepoErr(s.store.DeleteRulesBatch(ctx, ids)); err != nil {
		return err
	}
	s.reloadRules(ctx)
	s.publish(ctx, notify.Change{Rules: true})
	return nil
}

// ListRules 规则列表（enabled 过滤 + priority 升序 + {total, rows}；无分页）。
func (s *Service) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, int64, error) {
	rows, err := s.store.ListRules(ctx, enabled)
	if err != nil {
		return nil, 0, err
	}
	return rows, int64(len(rows)), nil
}

// reloadRules 规则引擎全量重载（规则 CRUD 后触发）。Reload 失败记日志——
// 管理端操作已成功，重载失败由下一次 CRUD/启动重试兜底。
// 脱离请求 ctx：请求 ctx 取消（客户端断开）→ 重载中止 → 引擎按
// 旧规则跑；context.WithoutCancel 剥离取消/超时信号仅继承值（与 publish 同纪律
// service.go:320）。invalidate Rules 分支（reloadAll 已 Background）为双保险。
func (s *Service) reloadRules(ctx context.Context) {
	if s.ruleReload == nil {
		return
	}
	if err := s.ruleReload.Reload(context.WithoutCancel(ctx)); err != nil && s.log != nil {
		s.log.Warn("rule engine reload failed", logx.Error(err))
	}
}

// getRule 按 id 查规则（列表全量扫描，规则表极小）；不存在 → ErrNotFound 含 id。
func (s *Service) getRule(ctx context.Context, id int64) (*domain.Rule, error) {
	rows, err := s.store.ListRules(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
}

// ruleWhenFromRaw 契约 when 自由对象 → 领域 RuleWhen：未知键拒绝
// （DisallowUnknownFields，400 语义；记债：白名单拒绝而非 round-trip 保留）。
func ruleWhenFromRaw(raw map[string]any) (domain.RuleWhen, error) {
	if len(raw) == 0 {
		return domain.RuleWhen{}, nil
	}
	var w domain.RuleWhen
	if err := decodeStrict(raw, &w); err != nil {
		return domain.RuleWhen{}, fmt.Errorf("when: %v", err)
	}
	return w, nil
}

// ruleThenFromRaw 契约 then 自由对象 → 领域 RuleThen（同 when 的严格反序列化）。
func ruleThenFromRaw(raw map[string]any) (domain.RuleThen, error) {
	if len(raw) == 0 {
		return domain.RuleThen{}, nil
	}
	var t domain.RuleThen
	if err := decodeStrict(raw, &t); err != nil {
		return domain.RuleThen{}, fmt.Errorf("then: %v", err)
	}
	return t, nil
}

// decodeStrict map → 结构体：未知键拒绝（与契约字段白名单一致）。
func decodeStrict(raw map[string]any, v any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// mapRuleRepoErr 规则存储错误映射：唯一约束冲突 → ErrConflict（409，保留冲突
// 详情 "priority=10 or name=\"x\""——与 mapRepoErr 对齐，两函数唯一差异即此
// 分支，规则 409 响应体带详情）；缺失 id → ErrNotFound（保留 "id=5 missing"
// 详情，404 响应带 id）。
func mapRuleRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrConflict):
		detail := strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": ")
		return fmt.Errorf("%w: %s", ErrConflict, detail)
	case errors.Is(err, repository.ErrNotFound):
		detail := strings.TrimPrefix(err.Error(), repository.ErrNotFound.Error()+": ")
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	}
	return err
}
