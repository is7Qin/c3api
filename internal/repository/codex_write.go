// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/template"
)

const templateWriteLockNamespace int64 = 0x4333415049540000

// AccountFieldValues 是账号行参与**按值比较**的字段快照。它承载 ChangedFields
// 需要的全部旧值原料：字段集由 domain 的声明表给出，表中每一行都必须在这里有
// 对应项，否则该字段的"是否真的变了"不可判定。写入事务内由
// lockAccountsForUpdate 经 FOR UPDATE 读出，故它是判据且无 lost-update 窗口。
type AccountFieldValues struct {
	ID                       int64
	Name                     string
	TemplateID               int64
	BaseURL                  *string
	UpstreamKey              string
	MaxConcurrency           int
	Enabled                  bool
	CacheDomain              *string
	UpstreamCostMultiplierBp int
}

// sameNullableString 可空字符串按值相等（nil 与 nil 相等；nil 与 &"" 不等）。
// 身份类字段推进 K 的判据：只有**值真的变了**才推进（幂等重写不推进）。
func sameNullableString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// patchNullable 把补丁的可空字符串编码归一成落库值：nil = 未提供，
// 空串 = 清空（落 NULL），其余 = 落值。归一后与旧值按值比较。
func patchNullable(v *string) *string {
	if v == nil || *v == "" {
		return nil
	}
	return v
}

// ChangedFields 由补丁与旧值快照算出**真实变更集**：仅当补丁提供了该字段且值
// 确实不同才置位（幂等重写不算变更）。它是字段比较的唯一落点——写入路径与测试
// 替身都调用它，不得各自重写一份比较；字段类别（身份/配置）与后果只由 domain
// 的声明表决定。
// TestChangedFieldsCoversEveryDeclaredField 机械断言本函数对表中每一行都有判定
// 分支——新增字段而漏加分支会失败，不会静默退化成"永不推进 K"。
func ChangedFields(p AccountPatch, old AccountFieldValues) domain.FieldSet {
	var s domain.FieldSet
	if p.Name != nil && *p.Name != old.Name {
		s = s.With(domain.FieldName)
	}
	if p.TemplateID != nil && *p.TemplateID != old.TemplateID {
		s = s.With(domain.FieldTemplateID)
	}
	if p.BaseURL != nil && !sameNullableString(old.BaseURL, patchNullable(p.BaseURL)) {
		s = s.With(domain.FieldBaseURL)
	}
	if p.UpstreamKey != nil && *p.UpstreamKey != old.UpstreamKey {
		s = s.With(domain.FieldUpstreamKey)
	}
	if p.MaxConcurrency != nil && *p.MaxConcurrency != old.MaxConcurrency {
		s = s.With(domain.FieldMaxConcurrency)
	}
	if p.GroupIDs != nil {
		// 集合字段：旧集合在 join 表、不在旧值快照内，故按"补丁替换了集合"置位。
		// group_ids 是配置类（不推进 K）；组失效由调用方按旧∪新并集执行，不依赖
		// 本位的相等性。
		s = s.With(domain.FieldGroupIDs)
	}
	if p.Enabled != nil && *p.Enabled != old.Enabled {
		s = s.With(domain.FieldEnabled)
	}
	if p.CacheDomain != nil && !sameNullableString(old.CacheDomain, patchNullable(p.CacheDomain)) {
		s = s.With(domain.FieldCacheDomain)
	}
	if p.UpstreamCostMultiplierBp != nil && *p.UpstreamCostMultiplierBp != old.UpstreamCostMultiplierBp {
		s = s.With(domain.FieldUpstreamCostMultiplier)
	}
	return s
}

func withWriteTx(ctx context.Context, driver dialect.Driver, fn func(*ent.Client, dialect.Driver) error) error {
	tx, err := driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 后回滚仅返回事务已关闭。
	txDriver := &txDriver{tx: tx, drv: driver}
	if err := fn(ent.NewClient(ent.Driver(txDriver)), txDriver); err != nil {
		return err
	}
	return tx.Commit()
}

func lockTemplateWrites(ctx context.Context, driver dialect.Driver, ids []int64) error {
	for _, id := range sortedUniqueIDs(ids) {
		var result sql.Result
		if err := driver.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{templateWriteLockNamespace ^ id}, &result); err != nil {
			return fmt.Errorf("lock template %d: %w", id, err)
		}
	}
	return nil
}

func lockAccountsForUpdate(ctx context.Context, driver dialect.Driver, ids []int64) ([]AccountFieldValues, error) {
	sortedIDs := sortedUniqueIDs(ids)
	args := make([]any, len(sortedIDs))
	placeholders := make([]string, len(sortedIDs))
	for index, id := range sortedIDs {
		args[index] = id
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	query := `SELECT id, template_id, base_url, upstream_key, name, max_concurrency, enabled, cache_domain, upstream_cost_multiplier_bp FROM accounts WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id FOR UPDATE`
	rows := &entsql.Rows{}
	if err := driver.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	locked := make([]AccountFieldValues, 0, len(sortedIDs))
	for rows.Next() {
		var row AccountFieldValues
		var baseURL, cacheDomain sql.NullString
		if err := rows.Scan(&row.ID, &row.TemplateID, &baseURL, &row.UpstreamKey, &row.Name, &row.MaxConcurrency, &row.Enabled, &cacheDomain, &row.UpstreamCostMultiplierBp); err != nil {
			return nil, err
		}
		if baseURL.Valid {
			row.BaseURL = &baseURL.String
		}
		if cacheDomain.Valid {
			row.CacheDomain = &cacheDomain.String
		}
		locked = append(locked, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	existing := make([]int64, 0, len(locked))
	for _, row := range locked {
		existing = append(existing, row.ID)
	}
	if err := diffMissing(existing, sortedIDs); err != nil {
		return nil, err
	}
	return locked, nil
}

func loadTemplates(ctx context.Context, client *ent.Client, ids []int64) (map[int64]*domain.Template, error) {
	uniqueIDs := sortedUniqueIDs(ids)
	rows, err := client.Template.Query().Where(template.IDIn(uniqueIDs...)).All(ctx)
	if err != nil {
		return nil, err
	}
	gotIDs := make([]int64, 0, len(rows))
	result := make(map[int64]*domain.Template, len(rows))
	for _, row := range rows {
		gotIDs = append(gotIDs, row.ID)
		result[row.ID] = toDomainTemplate(row)
	}
	if err := diffMissing(gotIDs, uniqueIDs); err != nil {
		return nil, err
	}
	return result, nil
}

func validateCodexAccountBaseURL(tpl *domain.Template, baseURL *string) error {
	if isCodexType(tpl.CredentialType) && baseURL != nil && *baseURL != "" {
		return fmt.Errorf("%w: codex account base_url must be empty", ErrInvalidInput)
	}
	return nil
}

func validateCodexTemplateUpdate(ctx context.Context, client *ent.Client, tpl *domain.Template) error {
	if !isCodexType(tpl.CredentialType) {
		return nil
	}
	if tpl.BaseURL != "" {
		return fmt.Errorf("%w: codex template base_url must be empty", ErrInvalidInput)
	}
	hasOverride, err := client.Account.Query().Where(
		account.TemplateIDEQ(tpl.ID),
		account.BaseURLNotNil(),
		account.BaseURLNEQ(""),
	).Exist(ctx)
	if err != nil {
		return err
	}
	if hasOverride {
		return fmt.Errorf("%w: referenced codex account base_url must be empty", ErrInvalidInput)
	}
	return nil
}

func isCodexType(typ credential.Type) bool {
	return typ == credential.TypeCodexOAuth || typ == credential.TypeCodexPAT
}

func sortedUniqueIDs(ids []int64) []int64 {
	result := slices.Clone(ids)
	slices.Sort(result)
	return slices.Compact(result)
}
