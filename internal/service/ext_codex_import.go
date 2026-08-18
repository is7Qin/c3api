// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// —— codex 凭据批量导入（Task B：batch-import-codex-oauth / batch-import-codex-pat
// 共享 upsert 核心） ——
//
// 解耦的是 API/校验面（oauth/pat 各端点类型特定校验），底层单实现
// importCodexAccounts：逐行 键查重（组合键 codex_email + codex_account_id）→
//   - 不存在 → imported：**单行事务**（txRepo.CreateAccount + txRepo.
//     UpsertAccountExt（NewCodexIdentity 身份）+ txRepo.SetAccountGroups 可选——
//     任一步失败整体回滚，无孤儿）；
//   - 存在且类型匹配 → updated：**只更新凭据列**（oauth → WriteOAuthRotation
//     三列 / pat → WritePATKey——identity/并发/权重/归属零触碰；全量
//     UpsertAccountExt 的 ClearX 清 NULL 面禁用）；
//   - 存在但类型不匹配 → 行级 failed（不跨类型混写）。
//
// 不调 service.CreateAccount（带 invalidate/publish 副作用面——事务回滚时副作用
// 已发）——直调 tx 版 repo 原始方法；invalidate/publish 事务提交后批末一次。
// 批内同键重复行逐行顺序应用后者胜；跨请求并发同键 → (codex_email,
// codex_account_id) 唯一索引兜底（后到者行级 failed）。

// codexImportRow 共享核心行形态（oauth/pat 两端点归一——类型特定校验后仅凭据
// 字段差异；index = items 原始下标——行级 failed 定位契约）。
type codexImportRow struct {
	index        int
	email        string
	accountID    string
	oauthToken   string
	oauthRT      string
	oauthExpires *time.Time
	patKey       string
	maxConc      int // 显式归一（缺省 25——导入面裁决覆盖账号表默认 8）
	weight       int // 显式归一（缺省 100）
}

// ImportCodexOAuthAccounts 批量导入 codex-oauth 凭据（ServerInterface 依赖面）：
// 模板顶层校验（缺/不存在 → 400/404；**credential_type 必须 == codex-oauth——
// 错配 → 400 整批拒绝**，防违反 ext 类型 == 模板类型的硬不变量 ext_codex.go:238）；
// 逐行类型特定校验（必填/成对/expires RFC3339/email 格式——失败 → 行级 failed
// 收集继续）；共享核心落库。
func (s *Service) ImportCodexOAuthAccounts(ctx context.Context, items []domain.CodexOAuthImportItem, tplID, groupID *int64) (*domain.ImportResult, error) {
	if err := s.checkCodexImportTemplate(ctx, tplID, credential.TypeCodexOAuth); err != nil {
		return nil, err
	}
	res := &domain.ImportResult{}
	rows := make([]codexImportRow, 0, len(items))
	for i, it := range items {
		row, err := codexOAuthRow(i, it)
		if err != nil {
			res.Failed = append(res.Failed, domain.ImportFailedItem{Index: i, Error: err.Error()})
			continue
		}
		rows = append(rows, row)
	}
	s.importCodexAccounts(ctx, rows, *tplID, groupID, credential.TypeCodexOAuth, res)
	return res, nil
}

// ImportCodexPATAccounts 批量导入 codex-pat 凭据（结构同 oauth 端点；模板
// credential_type 必须 == codex-pat）。
func (s *Service) ImportCodexPATAccounts(ctx context.Context, items []domain.CodexPATImportItem, tplID, groupID *int64) (*domain.ImportResult, error) {
	if err := s.checkCodexImportTemplate(ctx, tplID, credential.TypeCodexPAT); err != nil {
		return nil, err
	}
	res := &domain.ImportResult{}
	rows := make([]codexImportRow, 0, len(items))
	for i, it := range items {
		row, err := codexPATRow(i, it)
		if err != nil {
			res.Failed = append(res.Failed, domain.ImportFailedItem{Index: i, Error: err.Error()})
			continue
		}
		rows = append(rows, row)
	}
	s.importCodexAccounts(ctx, rows, *tplID, groupID, credential.TypeCodexPAT, res)
	return res, nil
}

// checkCodexImportTemplate 导入模板顶层校验（task review Important 1）：
// 缺/非法 id → 400；不存在 → 404；**credential_type 必须 == 端点类型**
// （oauth 端点要求 codex-oauth 模板、pat 端点要求 codex-pat）——错配 → 400
// 整批拒绝（"模板类型不匹配"）——否则 ext 行类型与父模板类型不一致，违反
// service.UpsertAccountExt 的硬不变量（ext_codex.go:238 不一致 → 400）。
func (s *Service) checkCodexImportTemplate(ctx context.Context, tplID *int64, endpointType credential.Type) error {
	if tplID == nil || *tplID <= 0 {
		return ErrInvalidInput
	}
	tpl, err := s.store.GetTemplate(ctx, *tplID)
	if err != nil {
		return mapRepoErr(err) // 模板缺 id → 404
	}
	if tpl.CredentialType != endpointType {
		return fmt.Errorf("%w: 模板类型不匹配：%s 端点要求 %s 模板（实际 %s）", ErrInvalidInput, endpointType, endpointType, tpl.CredentialType)
	}
	return nil
}

// codexOAuthRow oauth 行类型特定校验 → 共享行形态（失败返回行级错误文案）。
func codexOAuthRow(index int, it domain.CodexOAuthImportItem) (codexImportRow, error) {
	if !validEmail(it.CodexEmail) {
		return codexImportRow{}, errors.New("codex_email 必填且须为合法邮箱")
	}
	if it.CodexAccountID == "" {
		return codexImportRow{}, errors.New("codex_account_id 必填")
	}
	if it.CodexOAuthToken == "" || it.CodexOAuthRefreshToken == "" {
		return codexImportRow{}, errors.New("codex_oauth_token 与 codex_oauth_refresh_token 必须成对提供（同空同非空）")
	}
	var expires *time.Time
	if it.CodexOAuthExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *it.CodexOAuthExpiresAt)
		if err != nil {
			return codexImportRow{}, errors.New("codex_oauth_expires_at 必须为 RFC3339 格式")
		}
		expires = &t
	}
	row := codexImportRow{
		index: index, email: it.CodexEmail, accountID: it.CodexAccountID,
		oauthToken: it.CodexOAuthToken, oauthRT: it.CodexOAuthRefreshToken, oauthExpires: expires,
		maxConc: 25, weight: 100,
	}
	if err := applyCodexImportConfig(&row, it.MaxConcurrency, it.Weight); err != nil {
		return codexImportRow{}, err
	}
	return row, nil
}

// codexPATRow pat 行类型特定校验 → 共享行形态（失败返回行级错误文案）。
func codexPATRow(index int, it domain.CodexPATImportItem) (codexImportRow, error) {
	if !validEmail(it.CodexEmail) {
		return codexImportRow{}, errors.New("codex_email 必填且须为合法邮箱")
	}
	if it.CodexAccountID == "" {
		return codexImportRow{}, errors.New("codex_account_id 必填")
	}
	if it.CodexPATKey == "" {
		return codexImportRow{}, errors.New("codex_pat_key 必填")
	}
	row := codexImportRow{
		index: index, email: it.CodexEmail, accountID: it.CodexAccountID, patKey: it.CodexPATKey,
		maxConc: 25, weight: 100,
	}
	if err := applyCodexImportConfig(&row, it.MaxConcurrency, it.Weight); err != nil {
		return codexImportRow{}, err
	}
	return row, nil
}

// applyCodexImportConfig 配置面缺省/范围归一（对齐 validateAccount 既有边界：
// weight < 0 → 行级失败；max_concurrency < 1 → 归一缺省——导入面 25 覆盖账号
// 表默认 8；weight 0 合法保留）。nil = 未提供 → 缺省。
func applyCodexImportConfig(row *codexImportRow, maxConc, weight *int) error {
	if maxConc != nil && *maxConc >= 1 {
		row.maxConc = *maxConc
	}
	if weight != nil {
		if *weight < 0 {
			return errors.New("weight 不能为负")
		}
		row.weight = *weight
	}
	return nil
}

// importCodexAccounts 共享落库核心：逐行 查重 → imported（单行事务）/ updated
// （仅凭据列）/ 跨类型行级 failed；失败行收集进 res 继续下一行（单行失败不毁
// 整批——整批不原子，响应恒 200 语义由 handler 组装）。批末一次 invalidate +
// publish（gids = imported 行归组 ∪ updated 行既有分组——凭据变更须重载调度器
// 组快照，新凭据经 AccountExt 快照生效）。
func (s *Service) importCodexAccounts(ctx context.Context, rows []codexImportRow, tplID int64, groupID *int64, credType credential.Type, res *domain.ImportResult) {
	var gids []int64
	for _, row := range rows {
		imported, accountID, err := s.importCodexRow(ctx, row, tplID, groupID, credType)
		if err != nil {
			res.Failed = append(res.Failed, domain.ImportFailedItem{Index: row.index, Error: err.Error()})
			continue
		}
		if imported {
			res.Imported++
			if groupID != nil {
				gids = append(gids, *groupID)
			}
		} else {
			res.Updated++
			gs, gErr := s.store.GetAccountGroups(ctx, accountID)
			if gErr != nil {
				if s.log != nil {
					s.log.Warn("codex import: account groups query failed", logx.Int64("account_id", accountID), logx.Error(gErr))
				}
				continue
			}
			gids = append(gids, gs...)
		}
	}
	if len(gids) > 0 {
		s.inv.Accounts(gids, false)
		s.publish(ctx, notify.Change{Groups: gids})
	}
}

// importCodexRow 单行导入（组合键查重 → imported/updated/跨类型行级 failed）。
// 返回 imported=true 新建 / false 凭据更新（accountID 仅 updated 路径有效——
// invalidate 组并集用）；错误 = 行级失败原因。
func (s *Service) importCodexRow(ctx context.Context, row codexImportRow, tplID int64, groupID *int64, credType credential.Type) (imported bool, accountID int64, err error) {
	cur, err := s.store.FindAccountExtByCodexKey(ctx, row.email, row.accountID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return false, 0, err
	}
	if cur != nil {
		// 存在：类型匹配 → updated（只更新凭据列）；不匹配 → 行级 failed
		// （凭据列组不同，混写是脏——不跨类型 updated）
		if cur.CredentialType != credType {
			return false, 0, fmt.Errorf("凭据类型不匹配：已存在 %s 账号（codex_email=%q codex_account_id=%q）", cur.CredentialType, row.email, row.accountID)
		}
		// 软删账号不自动复活（task review Minor 1——删除意图权威，凭据更新
		// 无意义）：updated 只对存活账号；命中已软删 → 行级 failed（管理面
		// 恢复后重新导入）。
		acc, err := s.store.GetAccount(ctx, cur.AccountID)
		if err != nil {
			return false, 0, err
		}
		if acc.DeletedAt != nil {
			return false, 0, fmt.Errorf("账号已删除（codex_email=%q codex_account_id=%q）——管理面恢复后重新导入", row.email, row.accountID)
		}
		if err := s.updateCodexCredentials(ctx, cur.AccountID, row, credType); err != nil {
			return false, 0, err
		}
		return false, cur.AccountID, nil
	}
	// 不存在 → imported：单行事务。直调 tx 版 repo 原始方法——不调
	// service.CreateAccount（invalidate/publish 副作用面在事务回滚时已发）；
	// 副作用批末一次（importCodexAccounts）。
	identity := NewCodexIdentity()
	err = s.store.WithTx(ctx, func(tx repository.TxStore) error {
		acc, err := tx.CreateAccount(ctx, &domain.Account{
			Name: row.email, TemplateID: tplID,
			Weight: row.weight, MaxConcurrency: row.maxConc, // 显式写缺省（25/100）——不依赖表默认 8
		})
		if err != nil {
			return err
		}
		ext := &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credType, CodexIdentity: &identity,
			CodexEmail: &row.email, CodexAccountID: &row.accountID,
		}
		if credType == credential.TypeCodexOAuth {
			ext.CodexOAuthToken = &row.oauthToken
			ext.CodexOAuthRefreshToken = &row.oauthRT
			ext.CodexOAuthExpiresAt = row.oauthExpires
		} else {
			ext.CodexPATKey = &row.patKey
		}
		if _, err := tx.UpsertAccountExt(ctx, ext); err != nil {
			return err
		}
		if groupID != nil {
			if err := tx.SetAccountGroups(ctx, acc.ID, []int64{*groupID}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// 并发同键兜底（查重 check-then-act 窗口——跨请求并发导入同键）：事务内
		// UpsertAccountExt 撞 (codex_email, codex_account_id) 唯一索引 → 整体回滚
		// → 行级 failed（后到者失败，无孤儿）
		return false, 0, importKeyConflictErr(err)
	}
	return true, 0, nil
}

// updateCodexCredentials updated 路径凭据列部分更新（oauth 三列 / pat 单列——
// identity/email/并发/权重/归属零触碰）。
func (s *Service) updateCodexCredentials(ctx context.Context, accountID int64, row codexImportRow, credType credential.Type) error {
	if credType == credential.TypeCodexOAuth {
		return s.store.WriteOAuthRotation(ctx, accountID, row.oauthToken, row.oauthRT, row.oauthExpires)
	}
	return s.store.WritePATKey(ctx, accountID, row.patKey)
}

// importKeyConflictErr 唯一索引冲突 → 行级 failed 文案（并发同键兜底面：
// ent 透传 *pgconn.PgError Code=23505）。
func importKeyConflictErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errors.New("键冲突：同 (codex_email, codex_account_id) 账号并发导入，本行未写入")
	}
	return err
}
