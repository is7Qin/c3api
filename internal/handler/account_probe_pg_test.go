// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	// Registers pgtest's repository-dependent hooks.
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// probePGBase 打开测试私有 clone（含四张分区表 bootstrap），返回仓库与池。
func probePGBase(t *testing.T) (*repository.Repository, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), false, pool)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	return repos, pool
}

func probePGTemplate(t *testing.T, repos *repository.Repository) *domain.Template {
	t.Helper()
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name:             "probe-tpl-" + t.Name(),
		BaseURL:          "https://upstream.example",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m1"},
		// ent JSON 列不接受 NULL：nil map 会让 CreateTemplate 在回读时
		// unmarshal 失败（model_mapping must not be null）。
		FormatModels: map[domain.RequestFormat][]string{},
		ModelMapping: domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

func probePGAccount(t *testing.T, repos *repository.Repository, tplID, ownerUID int64, name string) *domain.Account {
	t.Helper()
	acc, err := repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: tplID, UpstreamKey: "sk-pg", MaxConcurrency: 8,
		Enabled: true, SupplierUserID: ownerUID,
	})
	require.NoError(t, err)
	return acc
}

func probePGUser(t *testing.T, repos *repository.Repository, email string) *domain.User {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), &domain.User{
		Email: email, PasswordHash: "x", Role: domain.RoleSupplier, Status: domain.UserStatusActive,
		MaxConcurrency: 4,
	})
	require.NoError(t, err)
	return u
}

func probePGService(t *testing.T, repos *repository.Repository, prober service.AccountProber) *service.Service {
	t.Helper()
	return service.New(service.Deps{
		Store: repos, Invalidate: service.NopInvalidator{}, EmailCodeStore: testEmailCodes, Prober: prober,
	})
}

// TestProbePGNoWrites A8（PG 侧）：探测前后 usage_logs/err_logs 计数与账号行快照
// （revision/failed_at/updated_at/last_error）不变——只读诊断观测的落库证据。
func TestProbePGNoWrites(t *testing.T) {
	repos, pool := probePGBase(t)
	ctx := context.Background()
	tpl := probePGTemplate(t, repos)
	acc := probePGAccount(t, repos, tpl.ID, 0, "probe-no-write")

	type snap struct {
		revision  int64
		failedAt  *time.Time
		updatedAt time.Time
		lastError *string
	}
	readSnap := func() snap {
		var s snap
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT lifecycle_revision, failed_at, updated_at, last_error FROM accounts WHERE id = $1`, acc.ID,
		).Scan(&s.revision, &s.failedAt, &s.updatedAt, &s.lastError))
		return s
	}
	count := func(table string) int64 {
		var n int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		return n
	}

	beforeAcc := readSnap()
	beforeUsage, beforeErr := count("usage_logs"), count("err_logs")

	prober := &probeStub{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	svc := probePGService(t, repos, prober)
	res, err := svc.ProbeAccount(ctx, acc.ID, service.AccountProbeRequest{Model: "m1", Format: domain.FormatOpenAIChat})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 1, prober.calls)

	require.Equal(t, beforeAcc, readSnap(), "账号行快照必须不变（revision/failed_at/updated_at/last_error）")
	require.Equal(t, beforeUsage, count("usage_logs"), "探测不得写 usage_logs")
	require.Equal(t, beforeErr, count("err_logs"), "探测不得写 err_logs")
}

// TestProbeSupplierScope A9（PG 侧）：供应商可测本人账号；越域 ⇒ 404（仓储
// owner 谓词，不泄漏存在性）。
func TestProbeSupplierScope(t *testing.T) {
	repos, _ := probePGBase(t)
	tpl := probePGTemplate(t, repos)
	owner := probePGUser(t, repos, "probe-owner@example.com")
	other := probePGUser(t, repos, "probe-other@example.com")
	ownAcc := probePGAccount(t, repos, tpl.ID, owner.ID, "probe-own")
	foreignAcc := probePGAccount(t, repos, tpl.ID, other.ID, "probe-foreign")

	prober := &probeStub{result: domain.ProbeResult{Success: true, StatusCode: 200}}
	api := New(probePGService(t, repos, prober))
	surface := supplierSurface{api: api}
	body := `{"model":"m1","format":"openai-chat"}`

	do := func(accountID int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/user/supplier/accounts/x/test", strings.NewReader(body))
		req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(owner.ID)))
		rec := httptest.NewRecorder()
		surface.PostSupplierAccountsIdTest(rec, req, accountID)
		return rec
	}

	rec := do(ownAcc.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got AccountTestResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.True(t, got.Success)
	require.Equal(t, 1, prober.calls)

	rec = do(foreignAcc.ID)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, 1, prober.calls, "越域请求不得触达探测执行")
}
