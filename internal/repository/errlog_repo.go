// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// err_logs 错误明细审计（用户裁决分表设计：独立于 usage_logs 的瘦表——拒绝/
// 异常路径的 status_code/error_message 等审计字段由本表承载）。写入来源：
// usage.ErrLogWorker（有界队列 + 背压采样丢弃；recordRejected 本地拒绝 + 401
// 鉴权失败 + record/finish 错误行半异常双轨）。分区表 DDL 由 partition.go
// 的 EnsureErrLogPartitioned 独占管理。

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/errlog"
)

// ErrLogQuery err_logs 查询过滤面（/errlogs API 同构 /usages：分页 + error_type/
// status_code/时间范围/model/group 等——完整错误面，status_code/error_type 全值）。
type ErrLogQuery struct {
	GroupID    int64 // 0 = 不过滤
	AccountID  int64
	UserID     int64 // 0 = 不过滤（/api/user/errlogs 强制 = 自己）
	KeyID      int64
	Model      string
	Format     string // 空 = 不过滤（无效值自然查空——与 model 同语义，契约不校验值域）
	StatusCode int // 0 = 不过滤
	ErrorType  string
	From       *time.Time
	To         *time.Time
	Cursor     int64 // keyset 游标（上页最后一条 id；<=0 = 首页无 id 谓词）
	Limit      int
}

type ErrLogRepo struct{ client *ent.Client }

// InsertBatch 批量插入错误明细（err_logs 瘦列：审计字段全量，无 token/价格列；
// 计费路径字段 cost 等不落——0/NULL 语义由建表 DEFAULT 承载）。
func (r *ErrLogRepo) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	builders := make([]*ent.ErrLogCreate, 0, len(logs))
	for _, l := range logs {
		builders = append(builders, buildErrLogCreate(r.client, l))
	}
	_, err := r.client.ErrLog.CreateBulk(builders...).Save(ctx)
	return err
}

// buildErrLogCreate 构建单条 err_logs 插入构建器（域内 UsageLog 瞬态审计字段
// 投影：request_id/归属四元组/model/format/status_code/error_type/error_message/
// latency_ms/billing_tier/created_at——无 token/价格列）。归属 0 = NULL 落库
// （SQL 不写该列）；ErrorMessage nil = NULL；BillingTier 空 = NULL。
func buildErrLogCreate(client *ent.Client, l *domain.UsageLog) *ent.ErrLogCreate {
	c := client.ErrLog.Create().
		SetRequestID(l.RequestID).
		SetModel(l.Model).
		SetFormat(errlog.Format(l.Format)).
		SetStatusCode(l.StatusCode).
		SetErrorType(string(l.ErrorType)).
		SetLatencyMs(l.LatencyMS).
		SetCreatedAt(l.CreatedAt)
	// client_ip（S-E 2026-08-17）：非空才 Set（ent 只落被 Set 的列——拒绝行
	// 恒带；空 = NULL 不写该列）。
	if l.ClientIP != "" {
		c = c.SetClientIP(l.ClientIP)
	}
	if l.GroupID > 0 {
		c = c.SetGroupID(l.GroupID)
	}
	if l.AccountID > 0 {
		c = c.SetAccountID(l.AccountID)
	}
	if l.TemplateID > 0 {
		c = c.SetTemplateID(l.TemplateID)
	}
	if l.UserID > 0 {
		c = c.SetUserID(l.UserID)
	}
	if l.KeyID > 0 {
		c = c.SetKeyID(l.KeyID)
	}
	if l.ErrorMessage != nil {
		c = c.SetErrorMessage(*l.ErrorMessage)
	}
	if l.BillingTier != "" {
		c = c.SetBillingTier(l.BillingTier)
	}
	return c
}

// QueryErrLogs err_logs keyset 游标分页查询（与 QueryUsages 同构：id 主键
// 游标 + from/to 必填 + LIMIT limit+1 探测下一页；去 Count——Total 已从契约移除）。
func (r *ErrLogRepo) QueryErrLogs(ctx context.Context, q ErrLogQuery) ([]*domain.UsageLog, error) {
	pred := r.client.ErrLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(errlog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(errlog.AccountIDEQ(q.AccountID))
	}
	if q.UserID > 0 {
		pred = pred.Where(errlog.UserIDEQ(q.UserID))
	}
	if q.KeyID > 0 {
		pred = pred.Where(errlog.KeyIDEQ(q.KeyID))
	}
	if q.Model != "" {
		pred = pred.Where(errlog.ModelEQ(q.Model))
	}
	if q.Format != "" {
		pred = pred.Where(errlog.FormatEQ(errlog.Format(q.Format)))
	}
	if q.StatusCode > 0 {
		pred = pred.Where(errlog.StatusCodeEQ(q.StatusCode))
	}
	if q.ErrorType != "" {
		pred = pred.Where(errlog.ErrorTypeEQ(q.ErrorType))
	}
	if q.From != nil {
		pred = pred.Where(errlog.CreatedAtGTE(*q.From))
	}
	if q.To != nil {
		pred = pred.Where(errlog.CreatedAtLTE(*q.To))
	}
	if q.Cursor > 0 {
		pred = pred.Where(errlog.IDLT(q.Cursor))
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := pred.Order(ent.Desc(errlog.FieldID)).Limit(q.Limit + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, row := range rows {
		l := &domain.UsageLog{
			ID: row.ID, RequestID: row.RequestID,
			Model: row.Model, Format: domain.RequestFormat(row.Format),
			StatusCode: row.StatusCode, ErrorType: domain.ErrorType(row.ErrorType),
			ErrorMessage: row.ErrorMessage,
			LatencyMS:    row.LatencyMs,
			CreatedAt:    row.CreatedAt,
		}
		if row.ClientIP != nil {
			l.ClientIP = *row.ClientIP
		}
		if row.GroupID != nil {
			l.GroupID = *row.GroupID
		}
		if row.AccountID != nil {
			l.AccountID = *row.AccountID
		}
		if row.TemplateID != nil {
			l.TemplateID = *row.TemplateID
		}
		if row.UserID != nil {
			l.UserID = *row.UserID
		}
		if row.KeyID != nil {
			l.KeyID = *row.KeyID
		}
		if row.BillingTier != nil {
			l.BillingTier = *row.BillingTier
		}
		out = append(out, l)
	}
	return out, nil
}
