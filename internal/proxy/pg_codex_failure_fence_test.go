// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// 真实 PG 集成：SDK 失效链必须走**围栏路径**（先按 (指纹, K) 上锁存 → 以身份代际
// K 为 guard 的 CAS 落库 → 组级 NOTIFY → 释放锁存）。装配侧一旦漏掉 Latch 或
// Publisher，失效链会静默退化为"只写 failed_at"的简化路径：判决不再与"身份是否
// 被授权变更"对齐，且对端只能靠周期兜底收敛——本用例以可观测指纹钉住该装配。
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestCodexFatalChainUsesFencedPathPG -v

const codexFailureFencePGTestSchema = "proxy_codex_failure_fence_test"

// latchAcquire 一次锁存获取的可观测记录。
type latchAcquire struct {
	accountID        int64
	fingerprint      string
	identityRevision int64
}

// recordingLatch 包装真实 LatchStore 并记录调用：既要断言"确实上过锁存"，又要
// 保留真实谓词语义（不用自实现替身，避免替身与生产分歧掩盖缺陷）。
type recordingLatch struct {
	*latch.LatchStore
	mu       sync.Mutex
	acquired []latchAcquire
	cleared  []int64
}

func (l *recordingLatch) TryAcquire(accountID int64, fingerprint string, identityRevision int64) bool {
	l.mu.Lock()
	l.acquired = append(l.acquired, latchAcquire{accountID, fingerprint, identityRevision})
	l.mu.Unlock()
	return l.LatchStore.TryAcquire(accountID, fingerprint, identityRevision)
}

func (l *recordingLatch) Clear(accountID int64) {
	l.mu.Lock()
	l.cleared = append(l.cleared, accountID)
	l.mu.Unlock()
	l.LatchStore.Clear(accountID)
}

func (l *recordingLatch) snapshot() ([]latchAcquire, []int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]latchAcquire(nil), l.acquired...), append([]int64(nil), l.cleared...)
}

// recordingGroupPublisher 记录组级 NOTIFY（围栏路径独有的一步）。
type recordingGroupPublisher struct {
	mu   sync.Mutex
	gids [][]int64
}

func (p *recordingGroupPublisher) PublishGroups(_ context.Context, gids []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gids = append(p.gids, append([]int64(nil), gids...))
}

func (p *recordingGroupPublisher) published() [][]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]int64(nil), p.gids...)
}

func TestCodexFatalChainUsesFencedPathPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + codexFailureFencePGTestSchema
	} else {
		dsn += "?search_path=" + codexFailureFencePGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+codexFailureFencePGTestSchema+` CASCADE; CREATE SCHEMA `+codexFailureFencePGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))

	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-fence-tpl", BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-fence", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "codex-fence-acc", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 4, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{
			InstallationID: "11111111-2222-3333-4444-555555555555", SessionID: "s", ThreadID: "t", WindowID: "t:0",
		},
		CodexOAuthToken: strPtrPG("at-1"), CodexOAuthRefreshToken: strPtrPG("rt-1"),
	})
	require.NoError(t, err)
	before, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)

	up, _ := newCodexImageUpstream(t, codexUpStep{status: 401, body: `{"error":{"code":"token_expired"}}`})
	codexRefreshMock(t, 401, `{"error":"invalid_grant"}`)

	re := rule.New(rule.Config{}, repos.Rules, nil, nil, nil)
	require.NoError(t, re.Reload(ctx))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, repos.Groups, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)
	sctx, scancel := context.WithCancel(ctx)
	require.NoError(t, sched.Start(sctx, nil))
	t.Cleanup(scancel)

	// 与生产装配同形：Latch + Publisher 都必须在位。
	ls := &recordingLatch{LatchStore: latch.NewLatchStore()}
	pub := &recordingGroupPublisher{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{
		Store: repos.Accounts, Failer: sched, Latch: ls, Publisher: pub,
	})
	adapter := sdkbridge.NewCodex(failure, newProxyOfficialRewriteTransportWithAssert(t, up.URL), sdkbridge.RotationDeps{})

	ext, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	cred := domain.CredentialFromExt(ext)
	_, err = adapter.GenerateImage(ctx, &cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh 被拒绝")

	// ① 围栏路径的判据：failure_source 只由 FailAccountCAS 写入（简化路径不写），
	//    且 CAS 会推进 C。
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FailedAt, "failed_at 落库")
	require.NotNil(t, got.FailureSource, "failure_source 必须落库 —— 只有围栏路径的 CAS 会写它")
	require.Equal(t, "sdk", *got.FailureSource)
	require.Equal(t, before.LifecycleRevision+1, got.LifecycleRevision, "围栏 CAS 推进配置代际 C")
	require.Equal(t, before.IdentityRevision, got.IdentityRevision, "失效写入不是身份写入，K 不变")

	// ② 锁存按 (指纹, K) 上锁并在成功后释放。
	acquired, cleared := ls.snapshot()
	require.Len(t, acquired, 1, "围栏路径必须先上锁存")
	require.Equal(t, acc.ID, acquired[0].accountID)
	require.NotEmpty(t, acquired[0].fingerprint, "锁存身份含候选指纹")
	require.Equal(t, before.IdentityRevision, acquired[0].identityRevision, "锁存的第三分量是身份代际 K")
	require.Equal(t, []int64{acc.ID}, cleared, "CAS 成功后释放锁存（否则账号永久不可选）")
	require.False(t, ls.IsLatched(acc.ID, acquired[0].fingerprint, before.IdentityRevision), "释放后不再锁存")

	// ③ 组级 NOTIFY：只有围栏路径发布。
	published := pub.published()
	require.Len(t, published, 1, "围栏路径必须发布一次组级 NOTIFY")
	require.Equal(t, []int64{g.ID}, published[0])

	// ④ 调度摘除（两条路径都做，此处确认端到端仍成立）。
	ri, ok := sched.Runtime(acc.ID)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestProductionWiresFailureFenceDeps 是**装配门**：上一条用例证明"接了 Latch +
// Publisher 就会走围栏路径"，但 main 是 package main、无法从测试里导入断言，故按
// 源码扫描钉住生产装配。漏掉任一项即退化为简化路径（判决不与身份对齐、对端只能
// 靠周期兜底），且退化是静默的——故必须有机械门。
func TestProductionWiresFailureFenceDeps(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "cmd", "server", "main.go"))
	require.NoError(t, err)
	// 折叠空白后匹配：gofmt 会随字段增删重新对齐，装配门不该因此假失败。
	normalized := regexp.MustCompile(`\s+`).ReplaceAllString(string(src), " ")
	// 用 True + 自定义消息（而非 Contains）：断言失败时不要把整个 main.go 打出来。
	require.True(t, strings.Contains(normalized, "Latch: latchStore,"),
		"生产必须把锁存装配进 sdkbridge.FailureDeps —— 缺失则失效链不走围栏路径")
	require.True(t, strings.Contains(normalized, "Publisher: schedGroupPub{pub},"),
		"生产必须把组级发布面装配进 sdkbridge.FailureDeps —— 缺失则失效不对端广播")
}
