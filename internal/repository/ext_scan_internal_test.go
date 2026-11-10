// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/ent/schema"
)

// accountExtTableAlias 投影列清单里的固定表别名（清单是 ext 与父行 JOIN 的
// SELECT 片段，列名都带 `e.` 前缀）。
const accountExtTableAlias = "e"

// accountExtProjectionPrimaryKey 主键列：不参与凭据投影（与父行的关联走
// account_id），故对照 ent schema 时要排除。
const accountExtProjectionPrimaryKey = "id"

// singleRowScanner entsql.Rows 所需的最小 ColumnScanner：一行值按列序给出，
// Scan 逐槽赋值（NULL → 目的类型零值，与 pgx 一致）。
//
// 为什么不走 pgxmock：这些用例要观测的是"某一列的字面内容 → 哪个领域字段"，
// 不是驱动行为（驱动层由仓库的 pgxmock 用例覆盖）。直接喂 ColumnScanner 可以
// 把列序/字面量的影响面缩到扫描器本身。
type singleRowScanner struct {
	cols []string
	row  []any
	done bool
}

func (s *singleRowScanner) Close() error { return nil }

func (s *singleRowScanner) ColumnTypes() ([]*sql.ColumnType, error) { return nil, nil }

func (s *singleRowScanner) Columns() ([]string, error) { return s.cols, nil }

func (s *singleRowScanner) Err() error { return nil }

func (s *singleRowScanner) Next() bool {
	if s.done {
		return false
	}
	s.done = true
	return true
}

func (s *singleRowScanner) NextResultSet() bool { return false }

func (s *singleRowScanner) Scan(dest ...any) error {
	if len(dest) != len(s.row) {
		return fmt.Errorf("scan %d destinations for %d columns", len(dest), len(s.row))
	}
	for i := range dest {
		slot := reflect.ValueOf(dest[i])
		if slot.Kind() != reflect.Pointer || slot.IsNil() {
			return fmt.Errorf("column %q: destination must be a non-nil pointer", s.cols[i])
		}
		target := slot.Elem()
		if s.row[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		value := reflect.ValueOf(s.row[i])
		switch {
		case value.Type().AssignableTo(target.Type()):
			target.Set(value)
		case value.Type().ConvertibleTo(target.Type()):
			target.Set(value.Convert(target.Type()))
		default:
			return fmt.Errorf("column %q: cannot assign %T to %s", s.cols[i], s.row[i], target.Type())
		}
	}
	return nil
}

// accountExtProjectionColumnNames 把 accountExtProjectionColumns 拆成**保留声明
// 顺序**的列名序列（剥掉固定表别名前缀）：清单是逗号分隔的 SELECT 片段，缩进与
// 换行不承载语义。
func accountExtProjectionColumnNames(t *testing.T) []string {
	t.Helper()
	parts := strings.Split(accountExtProjectionColumns, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.TrimSpace(part)
		require.NotEmptyf(t, column, "投影列清单里有空列：%q", accountExtProjectionColumns)
		require.Truef(t, strings.HasPrefix(column, accountExtTableAlias+"."),
			"投影列 %q 缺表别名 %q 前缀（清单是 e JOIN a 的片段）", column, accountExtTableAlias)
		names = append(names, strings.TrimPrefix(column, accountExtTableAlias+"."))
	}
	require.NotEmpty(t, names, "投影列清单为空")
	return names
}

// accountExtProjectionRow 按**清单声明顺序**把"列名 → 值"摊平成一行，并在末尾
// 追加父行 identity_revision。这行的列序就是调用方承诺的 SQL 列序。
func accountExtProjectionRow(t *testing.T, values map[string]any, identityRevision int64) *singleRowScanner {
	t.Helper()
	columns := accountExtProjectionColumnNames(t)
	row := make([]any, 0, len(columns)+1)
	for _, column := range columns {
		value, ok := values[column]
		require.Truef(t, ok, "投影列 %q 没有测试值（清单新增了列，用例需同步）", column)
		row = append(row, value)
	}
	row = append(row, identityRevision)
	cols := make([]string, 0, len(columns)+1)
	cols = append(cols, columns...)
	cols = append(cols, "identity_revision")
	return &singleRowScanner{cols: cols, row: row}
}

// accountExtScanFixture 一行的常规形态（除 codex_identity 外的凭据/标识列都填
// 可识别的值，空列显式给 NULL），identity 由用例决定。
func accountExtScanFixture(t *testing.T, identity any) *singleRowScanner {
	t.Helper()
	return accountExtProjectionRow(t, map[string]any{
		"account_id":                int64(7),
		"credential_type":           string(credential.TypeCodexOAuth),
		"codex_identity":            identity,
		"codex_oauth_token":         sql.NullString{String: "access-sentinel", Valid: true},
		"codex_oauth_refresh_token": sql.NullString{String: "refresh-sentinel", Valid: true},
		"codex_oauth_expires_at":    sql.NullTime{},
		"codex_pat_key":             sql.NullString{},
		"codex_email":               sql.NullString{},
		"codex_account_id":          sql.NullString{},
	}, 29)
}

// TestScanAccountExtRevisionJSONNullIsNil codex_identity 是 jsonb：**SQL NULL**
// 与 **JSON 字面量 `null`** 在 ent 生成层扫描器里都产出 nil 身份。裸 SQL 投影
// 若只判 `len(identity) > 0`，列里存 `null` 时会解出一个"非 nil 的空身份对象"
// ——与 ent 路径语义不等价，且没有任何报错。
//
// 判据按字面量：`null` → nil；`{}` → 非 nil（"配置了空身份"，与"没有身份"不同，
// 所以不能用"字段全空即 nil"的启发式替代）；正常 JSON → 解析出字段。
func TestScanAccountExtRevisionJSONNullIsNil(t *testing.T) {
	cases := []struct {
		name        string
		identity    any
		wantNil     bool
		wantInstall string
	}{
		{"SQL NULL", nil, true, ""},
		{"JSON 字面量 null", []byte("null"), true, ""},
		{"JSON null 带空白", []byte(" null\n"), true, ""},
		{"空对象不是 null", []byte(`{}`), false, ""},
		{"正常身份", []byte(`{"installation_id":"inst-1"}`), false, "inst-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, identityRevision, err := scanAccountExtRevision(
				&entsql.Rows{ColumnScanner: accountExtScanFixture(t, tc.identity)})
			require.NoError(t, err)
			require.Equal(t, int64(29), identityRevision, "父行 K 紧随投影列")
			require.Equal(t, int64(7), ext.AccountID)
			if tc.wantNil {
				require.Nil(t, ext.CodexIdentity,
					"codex_identity 的 SQL NULL 与 JSON null 同义（ent 扫描器语义）：必须是 nil，不能是空身份对象")
				return
			}
			require.NotNil(t, ext.CodexIdentity,
				"非 null 的 JSON 必须解析成身份对象（{} 也算\"配置了空身份\"，不得按字段全空折叠成 nil）")
			require.Equal(t, tc.wantInstall, ext.CodexIdentity.InstallationID)
		})
	}
}

// TestAccountExtProjectionColumnsMatchSchema 投影列清单必须与 ent schema 的
// account_ext 字段集合**逐一对应**（主键除外）：漏一列 ⇒ 凭据面读缺该字段且
// 没有任何编译期报错；多一列或多一列不存在的列 ⇒ SQL 直接失败。
//
// 编译期无法把"列清单"与"扫描顺序"绑在一起，所以这条对照放在运行期。
func TestAccountExtProjectionColumnsMatchSchema(t *testing.T) {
	var want []string
	for _, field := range (schema.AccountExt{}).Fields() {
		desc := field.Descriptor()
		column := desc.Name
		if desc.StorageKey != "" {
			column = desc.StorageKey
		}
		if column == accountExtProjectionPrimaryKey {
			continue // 主键不进凭据投影（父行关联走 account_id）
		}
		want = append(want, column)
	}
	require.NotEmpty(t, want, "schema.AccountExt 的字段集合读成空：反射契约变了")
	require.Equal(t, want, accountExtProjectionColumnNames(t),
		"投影列清单必须等于 ent schema 的 account_ext 非主键字段集合且顺序一致（漏列会让凭据面静默读缺）")
}

// TestAccountExtProjectionOrderMatchesScanner 投影列清单的**顺序**必须与
// scanAccountExtRevision 的扫描顺序一致：扫描按位置赋值，清单换序（扫描器没换）
// 则凭据内容落进别的字段；codex_oauth_token 与 codex_oauth_refresh_token 这类
// 同为 *string 的相邻列互换连报错都不会有。
//
// 判据：行按清单顺序喂"各列不同的哨兵"，扫描后每个领域字段必须拿到**自己那一列**
// 的哨兵。清单被删一列时行也随之少一格 ⇒ 扫描的目的数量对不上 ⇒ 同样报红。
func TestAccountExtProjectionOrderMatchesScanner(t *testing.T) {
	expires := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	rows := accountExtProjectionRow(t, map[string]any{
		"account_id":                int64(7),
		"credential_type":           string(credential.TypeCodexOAuth),
		"codex_identity":            []byte(`{"installation_id":"inst-sentinel"}`),
		"codex_oauth_token":         sql.NullString{String: "access-sentinel", Valid: true},
		"codex_oauth_refresh_token": sql.NullString{String: "refresh-sentinel", Valid: true},
		"codex_oauth_expires_at":    sql.NullTime{Time: expires, Valid: true},
		"codex_pat_key":             sql.NullString{String: "pat-sentinel", Valid: true},
		"codex_email":               sql.NullString{String: "email-sentinel", Valid: true},
		"codex_account_id":          sql.NullString{String: "account-sentinel", Valid: true},
	}, 29)
	ext, identityRevision, err := scanAccountExtRevision(&entsql.Rows{ColumnScanner: rows})
	require.NoError(t, err)
	require.Equal(t, int64(29), identityRevision, "父行 K 必须来自紧随投影列的那一列")
	require.Equal(t, int64(7), ext.AccountID, "account_id 落错列会撞上同为 int64 的 K")
	require.Equal(t, credential.TypeCodexOAuth, ext.CredentialType)
	require.NotNil(t, ext.CodexIdentity)
	require.Equal(t, "inst-sentinel", ext.CodexIdentity.InstallationID)
	require.NotNil(t, ext.CodexOAuthToken)
	require.Equal(t, "access-sentinel", *ext.CodexOAuthToken, "token 与 refresh_token 相邻且同型：换序静默串列")
	require.NotNil(t, ext.CodexOAuthRefreshToken)
	require.Equal(t, "refresh-sentinel", *ext.CodexOAuthRefreshToken)
	require.NotNil(t, ext.CodexOAuthExpiresAt)
	require.True(t, expires.Equal(*ext.CodexOAuthExpiresAt))
	require.NotNil(t, ext.CodexPATKey)
	require.Equal(t, "pat-sentinel", *ext.CodexPATKey)
	require.NotNil(t, ext.CodexEmail)
	require.Equal(t, "email-sentinel", *ext.CodexEmail)
	require.NotNil(t, ext.CodexAccountID)
	require.Equal(t, "account-sentinel", *ext.CodexAccountID)
}
