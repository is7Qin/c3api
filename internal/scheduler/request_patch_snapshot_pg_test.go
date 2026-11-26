// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// request_patch_snapshot_pg_test.go — P2 快照面的真实 PG 证据
// （spec custom-request-patch §6/§10 P2/§17.2）。
//
// 为什么必须在 scheduler 包内跑：digestTemplateUpstream 是包私有；本条走的是
// 「真 repository 载入器 → mapping 的 custom 门（Paths + Patch 同源同门）→
// domain.Template.Upstream → 唯一构造函数 digestTemplateUpstream」的生产链路，
// 不是手搓 TemplateUpstream。
//
// 取证要点（§17.2）：digest 必须用**读回后重新 Parse** 的 Patch.CanonicalBytes，
// 不能用写入前 bytes——jsonb 会把 1e18 规范化为十进制；同一 DB 值两次读回摘要
// 必须相同（无抖动），改配置则摘要必须变。

import (
	"context"
	"encoding/json"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// requestPatchSnapshotWire 覆盖 P1 遗留裁决要求的三类值：指数写法 1e18、
// >2^53 整数字面量 9007199254740993、{api_key} 占位符；再带嵌套对象与 delete。
const requestPatchSnapshotWire = `{
  "headers": {"Authorization": "Bearer {api_key}", "X-Call-Source": "vibe_code"},
  "formats": {"openai-chat": {
    "headers": {"User-Agent": "vibe-code/1.0"},
    "set": {"big": 9007199254740993, "e18": 1e18,
            "nested": {"n": 9007199254740993, "a": [1, 2]}},
    "delete": ["metadata.client_field"]
  }}
}`

type requestPatchPGFixture struct {
	repos *repository.Repository
	pool  *pgxpool.Pool
	tpl   *domain.Template
	group *domain.Group
	acc   *domain.Account
}

func newRequestPatchPGFixture(t *testing.T) *requestPatchPGFixture {
	t.Helper()
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "t-rp-snapshot", BaseURL: "http://upstream.example.com",
		CredentialType:   credential.TypeCustom,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m-1"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.CreateGroup(ctx, &domain.Group{
		Name: "g-rp-snapshot", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	acc, err := repos.CreateAccount(ctx, &domain.Account{
		Name: "a-rp-snapshot", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		MaxConcurrency: 4, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	return &requestPatchPGFixture{repos: repos, pool: pool, tpl: tpl, group: g, acc: acc}
}

// writePatch 写一行 template_ext（守卫写恒放行：本用例测快照读回，不是降级判定）。
func (f *requestPatchPGFixture) writePatch(t *testing.T, wire string, paths map[string]string) *domain.RequestPatch {
	t.Helper()
	patch, err := domain.ParseRequestPatch(json.RawMessage(wire))
	require.NoError(t, err)
	require.NotNil(t, patch)
	_, _, err = f.repos.TemplateExts.UpsertTemplateExt(context.Background(), &domain.TemplateExt{
		TemplateID: f.tpl.ID, CredentialType: credential.TypeCustom,
		UpstreamPaths: paths, RequestPatch: patch,
	}, func(*domain.TemplateExt) (bool, error) { return false, nil })
	require.NoError(t, err)
	return patch
}

// loadTemplate 走生产载入器：repository.LoadGroupAccounts → mapping 的 custom 门。
func (f *requestPatchPGFixture) loadTemplate(t *testing.T) *domain.Template {
	t.Helper()
	accs, err := f.repos.LoadGroupAccounts(context.Background(), f.group.ID)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].Template)
	return accs[0].Template
}

// TestPGTemplateUpstreamReadBackDigestStable 是 §17.2 的端到端半边：真实 PG 写
// request_patch（1e18 / 9007199254740993 / {api_key}）→ 读回 mapping → digest。
func TestPGTemplateUpstreamReadBackDigestStable(t *testing.T) {
	f := newRequestPatchPGFixture(t)
	ctx := context.Background()

	paths := map[string]string{
		string(domain.UpstreamPathChat): "v1/custom/chat?api-version=2024-10-21",
	}
	writePatch := f.writePatch(t, requestPatchSnapshotWire, paths)

	// 1) 真实载入器读回：custom 门把 Paths 与 Parse 后的 Patch 装进同一结构体。
	first := f.loadTemplate(t)
	require.NotNil(t, first.Upstream, "custom + ext 行 ⇒ Template.Upstream 必须非 nil")
	require.Equal(t, paths, first.Upstream.Paths, "paths 与 patch 同源同门")
	require.NotNil(t, first.Upstream.Patch, "request_patch 读回必须解析成 Patch")

	readBack := first.Upstream.Patch
	readBytes := string(readBack.CanonicalBytes())
	writeBytes := string(writePatch.CanonicalBytes())

	// 2) 读回值已被 PG 规范化（1e18 → 十进制），与写入前 bytes 不同——这正是
	//    §17.2 要求 digest 用读回值的原因。
	require.Contains(t, readBytes, "1000000000000000000", "jsonb 读回必须把 1e18 展开为十进制")
	require.NotContains(t, readBytes, "1e18")
	require.Contains(t, readBytes, "9007199254740993", ">2^53 整数必须逐字保留")
	require.Contains(t, readBytes, "{api_key}", "占位符读回原样（不替换/不掩码）")
	require.Contains(t, writeBytes, "1e18", "写入前 canonical 保留指数字面量（UseNumber 深归一）")
	require.NotEqual(t, writeBytes, readBytes,
		"写入前 bytes 与读回 bytes 必须不同（本用例的前提；相同则 §17.2 无从谈起）")

	// 3) digest 用**读回值**：同一 DB 值两次读回摘要必须逐字节相同。
	digest1 := digestTemplateUpstream(first.Upstream)
	second := f.loadTemplate(t)
	digest2 := digestTemplateUpstream(second.Upstream)
	require.Equal(t, digest1, digest2,
		"同一 DB 值两次读回的 digest 必须相同（读回值已被 PG 规范化，不存在抖动）")
	require.Equal(t, digestTemplateUpstream(&domain.TemplateUpstream{Paths: paths, Patch: readBack}), digest1,
		"digest 必须等于「读回 Patch」的规范摘要——即实现用的是读回值")
	require.NotEqual(t, digestTemplateUpstream(&domain.TemplateUpstream{Paths: paths, Patch: writePatch}), digest1,
		"用写入前 bytes 会得到不同摘要 ⇒ 写后/快照期换叶抖动；本断言锁住错误实现")

	// 4) 摘要真的进键：唯一构造函数把 Template.Upstream 折叠进 upstreamPathsKey。
	av := newSnapshotStatic(*f.acc, first, []int64{f.group.ID}, directEgress(), nil)
	require.Equal(t, digest1, av.key.upstreamPathsKey,
		"planKey 的 upstreamPathsKey 必须等于 digestTemplateUpstream(Template.Upstream)")

	// 5) 平台侧原始列已经是 PG 规范化后的值：digest 的输入确实来自读回。
	var raw string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT request_patch::text FROM template_exts WHERE template_id = $1`, f.tpl.ID).Scan(&raw))
	require.Contains(t, raw, "1000000000000000000", "PG jsonb 列必须已把 1e18 展开成十进制")
	require.NotContains(t, raw, "1e18")
	require.Contains(t, raw, "9007199254740993")
	require.Contains(t, raw, "{api_key}")

	// 6) 配置变化 ⇒ 摘要变 + planKey 变（否则复用旧叶，旧 patch 继续生效）。
	f.writePatch(t, `{"headers":{"X-Call-Source":"changed"}}`, paths)
	third := f.loadTemplate(t)
	digest3 := digestTemplateUpstream(third.Upstream)
	require.NotEqual(t, digest1, digest3, "改 request_patch 必须换新摘要")
	av3 := newSnapshotStatic(*f.acc, third, []int64{f.group.ID}, directEgress(), nil)
	require.NotEqual(t, av.key, av3.key, "改 request_patch 必须换新 planKey（否则旧叶被复用）")
}

// TestPGTemplateUpstreamWithoutPatchIsPathsOnly 钉住「空层归一」与 P1 golden：
// 只有路径时摘要必须与 P1 的 digestUpstreamPaths 逐字节一致；两者皆空 ⇒ Upstream
// 收敛为 nil（未配置只有一种快照形态）。
func TestPGTemplateUpstreamWithoutPatchIsPathsOnly(t *testing.T) {
	f := newRequestPatchPGFixture(t)
	ctx := context.Background()
	paths := map[string]string{string(domain.UpstreamPathChat): "v1/chat/completions"}

	_, _, err := f.repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: f.tpl.ID, CredentialType: credential.TypeCustom,
		UpstreamPaths: paths, RequestPatch: nil,
	}, func(*domain.TemplateExt) (bool, error) { return false, nil })
	require.NoError(t, err)

	got := f.loadTemplate(t)
	require.NotNil(t, got.Upstream, "有路径 ⇒ Upstream 非 nil")
	require.Nil(t, got.Upstream.Patch, "无 request_patch ⇒ Patch nil")
	require.Equal(t, wantPathsOnlyDigest, hex32(digestTemplateUpstream(got.Upstream)),
		"paths-only 摘要必须与 P1 digestUpstreamPaths 逐字节一致（既有 golden 不动）")

	// 全空 ⇒ nil 归一（paths 与 patch 同门清除）。
	_, _, err = f.repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: f.tpl.ID, CredentialType: credential.TypeCustom,
		UpstreamPaths: nil, RequestPatch: nil,
	}, func(*domain.TemplateExt) (bool, error) { return false, nil })
	require.NoError(t, err)
	empty := f.loadTemplate(t)
	require.Nil(t, empty.Upstream, "无路径无 patch ⇒ Upstream 必须收敛为 nil")
}

// TestPGTemplateUpstreamCrossTypeResidualInactive 是 §5.3/§11.14 的真实 PG 证据：
// 模板类型从 custom 改成 api_key 后陈旧 ext 行不会被删除；custom 门必须让
// Paths 与 Patch **一起**失效（Template.Upstream = nil），不得静默作用于其它类型。
func TestPGTemplateUpstreamCrossTypeResidualInactive(t *testing.T) {
	f := newRequestPatchPGFixture(t)
	ctx := context.Background()
	paths := map[string]string{string(domain.UpstreamPathChat): "v1/custom/chat"}
	f.writePatch(t, requestPatchSnapshotWire, paths)

	before := f.loadTemplate(t)
	require.NotNil(t, before.Upstream, "precondition：custom 下 Paths/Patch 都生效")

	// 类型切换（模拟管理面把模板从 custom 改成 api_key；ext 行保留 = 残留行）。
	_, err := f.pool.Exec(ctx, `UPDATE templates SET credential_type = $1 WHERE id = $2`,
		string(credential.TypeAPIKey), f.tpl.ID)
	require.NoError(t, err)

	// 残留行仍然存在（证明是 custom 门在兜底，不是行被删了）。
	ext, err := f.repos.TemplateExts.GetTemplateExt(ctx, f.tpl.ID)
	require.NoError(t, err, "类型切换不会删除 template_ext 行（残留窗口真实存在）")
	require.NotNil(t, ext.UpstreamPaths)
	require.NotNil(t, ext.RequestPatch)

	after := f.loadTemplate(t)
	require.Equal(t, credential.TypeAPIKey, after.CredentialType)
	require.Nil(t, after.Upstream,
		"非 custom ⇒ Template.Upstream 必须为 nil：残留路径与残留 patch 一起失效（§11.14）")
	require.Equal(t, wantEmptyUpstreamDigest, hex32(digestTemplateUpstream(after.Upstream)),
		"非 custom 的 upstreamPathsKey 必须是空集摘要（P1 golden）")
}
