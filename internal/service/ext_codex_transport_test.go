// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// 本文件覆盖 spec-codex-outbound-transport §4.3 的判定矩阵，而且**顺序本身**
// 也是被测对象（顺序变了这些用例就红）：
//
//	0. 类型存在性 → 1. 值域 → 2. 前置读旧值 → 3. 类型面（含幂等例外）
//	→ 4. 降级门 → 5. 放行降级时 WARN
//
// 判定只有 UpsertTemplateExt 一处入口（handler 只透传 `?confirm=`），所以这里
// 的用例直接打 service，不经 HTTP。

// allowTemplateExtWrite 恒放行的 decide：**只用于播种**（把一行 ext 直接写进
// 底层 store，模拟「陈旧行 / 旧值 ws」这类只有 DB 直改才到得了的状态）。
// 被测对象是 service 的判定，播种不需要仓储层再判一次——真正的判定入口只有
// Service.UpsertTemplateExt 一处。
func allowTemplateExtWrite(*domain.TemplateExt) (bool, error) { return false, nil }

// storedTransport 直接读仓储层的落库值——不经 service，避免用被测代码自证。
func storedTransport(t *testing.T, svc *Service, templateID int64) (domain.CodexResponsesTransport, bool) {
	t.Helper()
	e, err := svc.store.GetTemplateExt(context.Background(), templateID)
	if err != nil {
		require.ErrorIs(t, err, repository.ErrNotFound, "除「无 ext 行」外，读回不该有别的错误")
		return domain.CodexResponsesTransportDefault, false
	}
	return e.CodexResponsesTransport, true
}

// TestUpsertTemplateExtTransportAcceptsEmptyForAllExtTypes 矩阵第一行：带 ext
// 行的四种类型 + `""` 一律通过（api_key 通过的不是"通过"而是 400，见
// TestUpsertTemplateExtTransportTypeExistencePrecedesGate）。首次建行（无 ext
// 行）也必须成功——旧值缺失不是 404。
func TestUpsertTemplateExtTransportAcceptsEmptyForAllExtTypes(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	cases := []struct {
		name    string
		ct      credential.Type
		formats []domain.RequestFormat
	}{
		{name: "custom", ct: credential.TypeCustom, formats: []domain.RequestFormat{domain.FormatOpenAIChat}},
		{name: "special", ct: credential.TypeResponsesSpecial, formats: []domain.RequestFormat{domain.FormatOpenAIResponses}},
		{name: "oauth", ct: credential.TypeCodexOAuth, formats: []domain.RequestFormat{domain.FormatOpenAIResponses}},
		{name: "pat", ct: credential.TypeCodexPAT, formats: []domain.RequestFormat{domain.FormatOpenAIResponses}},
	}
	for _, tc := range cases {
		tpl := seedExtTemplate(t, svc, "t-empty-"+tc.name, tc.ct, tc.formats...)
		saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: tc.ct,
		}, "")
		require.NoError(t, err, "%s：首次建行 + 空值必须通过（旧值缺失不是 404）", tc.name)
		require.Equal(t, domain.CodexResponsesTransportDefault, saved.CodexResponsesTransport)
		got, ok := storedTransport(t, svc, tpl.ID)
		require.True(t, ok, "%s：空值也必须真的建行", tc.name)
		require.Equal(t, domain.CodexResponsesTransportDefault, got)
	}
}

// TestUpsertTemplateExtTransportCodexTriState codex 模板的合法三态：`""`/
// `"http"`/`"ws"` 原样落库（不 trim、不归一），且升级方向（→ ws）不需要确认。
func TestUpsertTemplateExtTransportCodexTriState(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	for i, want := range []domain.CodexResponsesTransport{
		domain.CodexResponsesTransportDefault,
		domain.CodexResponsesTransportHTTP,
		domain.CodexResponsesTransportWS,
	} {
		tpl := seedExtTemplate(t, svc, "t-tri-"+strconv.Itoa(i), credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
		saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: want,
		}, "")
		require.NoError(t, err, "codex 模板必须接受 %q", string(want))
		require.Equal(t, want, saved.CodexResponsesTransport, "upsert 返回值必须反映落库值")
		got, ok := storedTransport(t, svc, tpl.ID)
		require.True(t, ok)
		require.Equal(t, want, got, "落库值必须与请求逐字一致（不 trim、不大小写归一）")
	}

	// 升级方向不需要确认："" → http → ws 连续放行。
	tpl := seedExtTemplate(t, svc, "t-tri-upgrade", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	for _, v := range []domain.CodexResponsesTransport{
		domain.CodexResponsesTransportHTTP, domain.CodexResponsesTransportWS,
	} {
		_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: v,
		}, "")
		require.NoError(t, err, "升级到 %q 不需要确认", string(v))
	}
}

// TestUpsertTemplateExtTransportRejectsDirtyLiterals 值域（第 1 步）：不做
// 大小写/别名/空白归一，且被拒写入不得改动原值。
func TestUpsertTemplateExtTransportRejectsDirtyLiterals(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-dirty", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	// 先落一个 ws 当"原值"，好验证后续 400 不写坏它。
	_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, "")
	require.NoError(t, err)

	for _, bad := range []domain.CodexResponsesTransport{"WS", "websocket", " ", "http "} {
		_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: bad,
		}, "")
		require.ErrorIs(t, err, ErrInvalidInput, "非法字面量 %q 必须 400", string(bad))
		require.NotErrorIs(t, err, ErrConflict, "值域校验先于降级门：%q 不得先吃 409", string(bad))
	}

	got, ok := storedTransport(t, svc, tpl.ID)
	require.True(t, ok)
	require.Equal(t, domain.CodexResponsesTransportWS, got, "被拒写入不得改动原值")
}

// TestUpsertTemplateExtTransportRejectsNonCodexExplicit 第 3 步：非 codex 只
// 接受 `""`——`"http"` 与 `"ws"` 一样是永不生效的配置（都是 400 的对象）。
// 确认令牌只作用于降级门，不得把类型面放宽。
func TestUpsertTemplateExtTransportRejectsNonCodexExplicit(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	cases := []struct {
		name    string
		ct      credential.Type
		formats []domain.RequestFormat
	}{
		{name: "custom", ct: credential.TypeCustom, formats: []domain.RequestFormat{domain.FormatOpenAIChat}},
		{name: "special", ct: credential.TypeResponsesSpecial, formats: []domain.RequestFormat{domain.FormatOpenAIResponses}},
	}
	for _, tc := range cases {
		tpl := seedExtTemplate(t, svc, "t-noncodex-"+tc.name, tc.ct, tc.formats...)
		for _, v := range []domain.CodexResponsesTransport{
			domain.CodexResponsesTransportHTTP, domain.CodexResponsesTransportWS,
		} {
			_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
				TemplateID: tpl.ID, CredentialType: tc.ct, CodexResponsesTransport: v,
			}, "")
			require.ErrorIs(t, err, ErrInvalidInput, "%s 模板不接受显式 %q", tc.name, string(v))

			_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
				TemplateID: tpl.ID, CredentialType: tc.ct, CodexResponsesTransport: v,
			}, "transport-downgrade")
			require.ErrorIs(t, err, ErrInvalidInput, "确认令牌不得放宽类型面（%s + %q）", tc.name, string(v))
		}
		_, ok := storedTransport(t, svc, tpl.ID)
		require.False(t, ok, "被拒写入不得建行（%s）", tc.name)
	}
}

// TestUpsertTemplateExtTransportIdempotentStaleRow 幂等例外（第 3 步）：
// 模板类型改掉后陈旧 ext 行原样带回（new == old）必须放行；例外**只**覆盖
// 相等，不等于就回到 400，而清空仍走降级门。
func TestUpsertTemplateExtTransportIdempotentStaleRow(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-stale-keep", credential.TypeCustom, domain.FormatOpenAIChat)

	// 绕过 service 直接落一行陈旧 ws：模板类型从 codex 改成 custom 时
	// UpdateTemplate 不碰 ext 行，DB 直改/导入也能到同一个状态。
	_, _, err := svc.store.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, allowTemplateExtWrite)
	require.NoError(t, err)

	// 原样带回：没有幂等例外，这里必 400（custom 不接受 ws）。
	saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, "")
	require.NoError(t, err, "陈旧 ext 行原样带回不得 400（幂等例外）")
	require.Equal(t, domain.CodexResponsesTransportWS, saved.CodexResponsesTransport)

	// 例外只覆盖 new == old：改值就回到类型面 400（且 400 先于 409）。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
	}, "")
	require.ErrorIs(t, err, ErrInvalidInput, "幂等例外不得放过改值（custom + http）")
	require.NotErrorIs(t, err, ErrConflict, "类型面 400 必须排在降级门之前")

	// 清空（ws → ""）⇒ 409：证明上一行放行的 ws 真的是"原值"，门是活的。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
	}, "")
	require.ErrorIs(t, err, ErrConflict, "old == ws 的清空仍必须确认")

	got, ok := storedTransport(t, svc, tpl.ID)
	require.True(t, ok)
	require.Equal(t, domain.CodexResponsesTransportWS, got, "两次被拒写入都不得改动原值")
}

// TestUpsertTemplateExtTransportDowngradeGate 降级门（第 4 步）：ws → http 与
// ws → "" 两种降级都 409，消息带确认参数提示、不引入 expect_old；带确认放行。
func TestUpsertTemplateExtTransportDowngradeGate(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-gate", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, "")
	require.NoError(t, err)

	for _, newV := range []domain.CodexResponsesTransport{
		domain.CodexResponsesTransportHTTP, domain.CodexResponsesTransportDefault,
	} {
		_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: newV,
		}, "")
		require.ErrorIs(t, err, ErrConflict, "ws → %q 未确认必须 409", string(newV))
		require.Contains(t, err.Error(), "codex_responses_transport", "409 消息要点名被降级的字段")
		require.Contains(t, err.Error(), "?confirm=transport-downgrade", "409 消息必须带重试提示")
		require.Contains(t, err.Error(), "is a downgrade to http", "409 消息要用英文短句（会原样进响应体）")
		require.Contains(t, err.Error(), "resend with", "409 消息要用英文短句（会原样进响应体）")
		require.NotContains(t, err.Error(), "降级", "409 消息不得含中文（既有 ErrConflict 全是英文短句）")
		require.NotContains(t, err.Error(), "expect_old", "不引入 expect_old")

		got, ok := storedTransport(t, svc, tpl.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportWS, got, "409 不得改动原值")

		saved, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: newV,
		}, "transport-downgrade")
		require.NoError(t, err, "带确认令牌必须放行 ws → %q", string(newV))
		require.Equal(t, newV, saved.CodexResponsesTransport)

		// 复位成 ws 继续下一轮（升级不需要确认）。
		_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		}, "")
		require.NoError(t, err)
	}
}

// TestUpsertTemplateExtTransportConfirmTokenIsLiteral 确认令牌只认字面量：
// "yes"/大小写变体/带空白都不放行（避免"顺手带个 confirm 就过"）。
func TestUpsertTemplateExtTransportConfirmTokenIsLiteral(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-confirm", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, "")
	require.NoError(t, err)

	for _, bad := range []string{"yes", "1", "true", "transport-downgrade ", " Transport-Downgrade", "transport_downgrade"} {
		_, err := svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
		}, bad)
		require.ErrorIs(t, err, ErrConflict, "confirm=%q 不是令牌，必须 409", bad)
	}
	got, ok := storedTransport(t, svc, tpl.ID)
	require.True(t, ok)
	require.Equal(t, domain.CodexResponsesTransportWS, got, "被拒降级不得改动原值")

	// 令牌全等即放行（同一行数据，唯一变量就是令牌）。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
	}, "transport-downgrade")
	require.NoError(t, err)
}

// TestUpsertTemplateExtTransportTypeExistencePrecedesGate 第 0 步必须排在
// 409 门之前：api_key 模板根本没有 ext 行，若不先拦，陈旧 ws 行会让它先吃
// 409 再吃 400（spec §4.3 点名的那条顺序漏洞）。
func TestUpsertTemplateExtTransportTypeExistencePrecedesGate(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-apikey", credential.TypeAPIKey, domain.FormatOpenAIChat)

	// 陈旧行（仅 DB 直改可达）：api_key 模板不该有 ext 行，更不该有 ws。
	_, _, err := svc.store.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeAPIKey,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, allowTemplateExtWrite)
	require.NoError(t, err)

	// 走到降级门就是 ws → ""，本该 409；类型存在性必须先拦成 400。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeAPIKey,
	}, "")
	require.ErrorIs(t, err, ErrInvalidInput, "api_key 模板写 ext 行必须 400")
	require.NotErrorIs(t, err, ErrConflict, "类型存在性必须先于 409 门")

	got, ok := storedTransport(t, svc, tpl.ID)
	require.True(t, ok)
	require.Equal(t, domain.CodexResponsesTransportWS, got, "被拒写入不得改动原值")
}

// TestUpsertTemplateExtTransportDowngradeWarns WARN 是"实验臂何时被降级"的
// 唯一事后证据，所以它必须真的写进日志，且**只**为放行的降级写一条。
func TestUpsertTemplateExtTransportDowngradeWarns(t *testing.T) {
	// 日志目录用 os.MkdirTemp 而不是 t.TempDir()：zap 会持有日志文件句柄，
	// t.TempDir() 的自动清理在 Windows 上会因句柄未释放而让测试假红。
	dir, err := os.MkdirTemp("", "c3api-transport-warn-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logPath := filepath.Join(dir, "warn.log")
	log, err := logx.New("warn", logPath)
	require.NoError(t, err)

	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: log}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-warn", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	// 升级（"" → ws）：不是降级，不写 WARN。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, "")
	require.NoError(t, err)

	// 被拒的降级（ws → http，无确认）：写面没发生，也不写 WARN。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
	}, "")
	require.ErrorIs(t, err, ErrConflict)

	// 放行的降级：唯一的 WARN。
	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
	}, "transport-downgrade")
	require.NoError(t, err)

	require.NoError(t, log.Sync())
	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	out := string(raw)
	require.Equal(t, 1, strings.Count(out, "codex responses transport downgraded"),
		"只有真正放行的降级写一条 WARN（升级与 409 都不写）")
	require.Contains(t, out, `"level":"warn"`)
	require.Contains(t, out, fmt.Sprintf(`"template_id":%d`, tpl.ID), "WARN 必须带 template_id")
	require.Contains(t, out, `"old":"ws"`, "WARN 必须带降级前值")
	require.Contains(t, out, `"new":"http"`, "WARN 必须带降级后值")
}

// failingExtWriteStore 包一层 fakeStore，只让 ext 写入失败（模拟真实 repo 的写库
// 错误），其余方法原样透传——用于钉住「写失败不得留下已降级的审计证据」。
type failingExtWriteStore struct {
	*fakeStore
	err error
}

func (s *failingExtWriteStore) UpsertTemplateExt(context.Context, *domain.TemplateExt, repository.TemplateExtDecide) (*domain.TemplateExt, bool, error) {
	return nil, false, s.err
}

// TestUpsertTemplateExtTransportNoWarnWhenWriteFails WARN 落在落库成功之后：
// 写库失败时一条都不能写——否则审计里会出现"已降级"的假证据。
func TestUpsertTemplateExtTransportNoWarnWhenWriteFails(t *testing.T) {
	// 同 TestUpsertTemplateExtTransportDowngradeWarns：日志目录用 os.MkdirTemp，
	// 不用 t.TempDir()（zap 持有文件句柄，Windows 上自动清理会假红）。
	dir, err := os.MkdirTemp("", "c3api-transport-nowarn-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logPath := filepath.Join(dir, "warn.log")
	log, err := logx.New("warn", logPath)
	require.NoError(t, err)

	writeErr := errors.New("boom: ext write failed")
	st := &failingExtWriteStore{fakeStore: newFakeStore(), err: writeErr}
	svc := &Service{store: st, inv: &invRecorder{}, log: log}
	ctx := context.Background()
	tpl := seedExtTemplate(t, svc, "t-warn-fail", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)

	// 旧值 ws 直接写进底层 store（绕过会失败的那个写面），让本次请求真的走降级路径。
	_, _, err = st.fakeStore.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportWS,
	}, allowTemplateExtWrite)
	require.NoError(t, err)

	_, err = svc.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
		CodexResponsesTransport: domain.CodexResponsesTransportHTTP,
	}, "transport-downgrade")
	require.ErrorIs(t, err, writeErr, "写库失败必须上抛（降级已确认，但没落库）")

	require.NoError(t, log.Sync())
	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "codex responses transport downgraded",
		"写库失败不得写 WARN——审计必须反映事实")
	// 落库态也没变（写失败 = 什么都没发生）。
	got, ok := storedTransport(t, svc, tpl.ID)
	require.True(t, ok)
	require.Equal(t, domain.CodexResponsesTransportWS, got)
}
