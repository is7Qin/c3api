package scheduler

import (
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 带路径的模板 base_url（如 https://opencode.ai/zen、https://opencode.ai/zen/go）
// 必须仍能路由：base_url 的路径是上游协议前缀，不是"源"。指纹计算若把带路径的
// base_url 判为非法，会让候选指纹为空，reserve 的空指纹门把全部候选刷掉，
// 表现为 429 "no available account"——而账号本身完全健康。
func TestSelect_TemplateBaseURLWithPathStillRoutes(t *testing.T) {
	tpl := &domain.Template{ID: 2, Name: "opencode-zen", BaseURL: "https://opencode.ai/zen",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
		Models:           []string{"space-bunny-free"}}
	acc := &domain.Account{ID: 1, Name: "a", Enabled: true, TemplateID: tpl.ID, Template: tpl,
		IdentityRevision: 1, MaxConcurrency: 8, UpstreamKey: "sk-test"}

	groups, byID := buildSnapshots(map[int64][]*domain.Account{1: {acc}}, nil)
	sv := &StaticView{groups: groups, byID: byID, facts: attachCompilerFacts(byID)}
	dec, err := NewRoutingCompiler().Compile(CompilerInputs{Static: sv})
	require.NoError(t, err)

	sched := &Scheduler{timeNow: func() time.Time { return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC) }}
	sched.publisher = &routingPublisher{sched: sched}
	sched.view.Store(&RoutingView{generation: 1, static: sv, decision: dec})

	sel, err := sched.Select(1, domain.FormatOpenAIChat, "space-bunny-free")
	require.NoError(t, err, "带路径的模板 base_url 不应让健康账号变成不可路由")
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}
