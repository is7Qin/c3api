// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

// ─── 本文件钉什么 ──────────────────────────────────────────────────────────
//
// EffectiveBaseURL 与 BPSProfileDigest 是 S3a 收口出来的两个派生点：scheduler 的
// 计划键（internal/scheduler/static_key.go）与路由候选指纹（AccountCandidateFingerprint）
// 都改读它们。收口的前提是**行为逐字节不变**，所以这里铺一张 6×3×2 的合成网格，
// 期望值全部手写（字面量表），**不调用被测函数**也**不重算摘要**——任何"用实现算
// 期望"的写法都只能证明实现等于自己。
//
// profile 与摘要的关系：BPSProfileHash 的输入是 profile 的**规范 JSON**
// （键排序、紧凑、无空白），即
//
//	{"profile_id":"p","device_id":"d"}  →  {"device_id":"d","profile_id":"p"}
//	sha256  → 78d6ba09c3c0a0982a8d1fb9f56f3883ebfbc606710e6e4eb2d5478b8fa4fc30
//
// 下面把这条十六进制串写死：profile 摘要进的是候选指纹与计划键，改一个字节就得
// 在 diff 里看得见。
const (
	gridProfileJSON   = `{"profile_id":"p","device_id":"d"}`
	gridProfileDigest = "78d6ba09c3c0a0982a8d1fb9f56f3883ebfbc606710e6e4eb2d5478b8fa4fc30"
	gridTemplateBase  = "https://tpl.example/v1"
	gridBPSLiteral    = "https://bps.openai.com/basispoints/api"
)

// gridTemplates 是 tpl 维度：nil + 五种具体凭据类型。nil 的 cred 取 credential.Type
// 零值（""）——真实调用点不允许 tpl == nil 时调 BPSProfileDigest，这里仍然铺一格，
// 是为了钉住"零类型不是 BPS ⇒ 空摘要"（调用点失守时也不会凭空产出一枚摘要）。
func gridTemplates() []struct {
	name string
	tpl  *Template
	cred credential.Type
} {
	return []struct {
		name string
		tpl  *Template
		cred credential.Type
	}{
		{name: "nil", tpl: nil, cred: ""},
		{name: "bps", tpl: &Template{BaseURL: gridTemplateBase, CredentialType: credential.TypeBPS}, cred: credential.TypeBPS},
		{name: "api_key", tpl: &Template{BaseURL: gridTemplateBase, CredentialType: credential.TypeAPIKey}, cred: credential.TypeAPIKey},
		{name: "codex-pat", tpl: &Template{BaseURL: gridTemplateBase, CredentialType: credential.TypeCodexPAT}, cred: credential.TypeCodexPAT},
		{name: "custom", tpl: &Template{BaseURL: gridTemplateBase, CredentialType: credential.TypeCustom}, cred: credential.TypeCustom},
		{name: "responses-special", tpl: &Template{BaseURL: gridTemplateBase, CredentialType: credential.TypeResponsesSpecial}, cred: credential.TypeResponsesSpecial},
	}
}

// gridAccountBaseURLs 是 acc.BaseURL 维度：nil / 空串 / 非空。三层语义都不同
// （nil = 继承模板；空串 = DB 不会落但快照可能手搓；非空 = 覆盖），所以逐个铺。
func gridAccountBaseURLs() []*string {
	empty := ""
	value := "https://x"
	return []*string{nil, &empty, &value}
}

// gridExts 是 ext 维度：nil / 带一段合法 BPSProfile（非 BPS 账号上的"残留"形态）。
func gridExts() []*AccountExt {
	return []*AccountExt{nil, {BPSProfile: []byte(gridProfileJSON)}}
}

// wantGridBaseURL 是 EffectiveBaseURL 的 6×3 手写期望表：
//
//	行 = 模板（nil / bps / api_key / codex-pat / custom / responses-special）
//	列 = acc.BaseURL（nil / "" / "https://x"）
//
// 两条关键语义在这张表里肉眼可见：BPS 行恒为固定端点（账号覆盖被忽略）；nil 行的
// 第二列仍是 ""（tpl == nil 不影响账号覆盖生效），第三列是账号值。
var wantGridBaseURL = map[string][3]string{
	"nil":               {"", "", "https://x"},
	"bps":               {gridBPSLiteral, gridBPSLiteral, gridBPSLiteral},
	"api_key":           {gridTemplateBase, gridTemplateBase, "https://x"},
	"codex-pat":         {gridTemplateBase, gridTemplateBase, "https://x"},
	"custom":            {gridTemplateBase, gridTemplateBase, "https://x"},
	"responses-special": {gridTemplateBase, gridTemplateBase, "https://x"},
}

// wantGridDigest 是 BPSProfileDigest 的 6×2 手写期望表：
//
//	行 = 模板类型（同上）
//	列 = ext（nil / 带 BPSProfile）
//
// 关键负例在表的形状里：只有 bps 行的第二列非空。非 BPS 行即使 ext 带 profile 也
// 必须是 ""（S1 刻意保留的残留字段不许算进摘要）。
var wantGridDigest = map[string][2]string{
	"nil":               {"", ""},
	"bps":               {"", gridProfileDigest},
	"api_key":           {"", ""},
	"codex-pat":         {"", ""},
	"custom":            {"", ""},
	"responses-special": {"", ""},
}

// TestEffectiveBaseURLGrid 逐格断言生效 baseURL 的优先级：BPS 固定端点 ⇒ 账号覆盖
// （含 tpl == nil）⇒ 模板值 ⇒ ""。
func TestEffectiveBaseURLGrid(t *testing.T) {
	// 固定端点同时钉字面量：网格里写死的是 URL 本身，常量被改道时这条先红，
	// 网格不会跟着"漂移地绿"。
	require.Equal(t, gridBPSLiteral, BPSDefaultEndpoint,
		"BPS 固定端点被改动：它是 provider 拥有的端点身份，账号覆盖不得改道")

	baseURLs := gridAccountBaseURLs()
	for _, tc := range gridTemplates() {
		t.Run(tc.name, func(t *testing.T) {
			for col, accBase := range baseURLs {
				acc := &Account{ID: 1, BaseURL: accBase}
				got := EffectiveBaseURL(tc.tpl, acc)
				require.Equal(t, wantGridBaseURL[tc.name][col], got,
					"tpl=%s acc.BaseURL=%s", tc.name, baseURLLabel(accBase))
			}
		})
	}
}

// TestBPSProfileDigestGrid 逐格断言 profile 摘要：类型门先于 ext 判空，非 BPS
// 模板上的残留 profile 一律不算。
func TestBPSProfileDigestGrid(t *testing.T) {
	exts := gridExts()
	for _, tc := range gridTemplates() {
		t.Run(tc.name, func(t *testing.T) {
			for col, ext := range exts {
				got := BPSProfileDigest(tc.cred, ext)
				require.Equal(t, wantGridDigest[tc.name][col], got,
					"cred=%q ext=%s", tc.cred, extLabel(ext))
			}
		})
	}
}

// TestBPSProfileDigestIgnoresResidualProfileOnNonBPSTemplates 是签名裁决的关键负例，
// 单独点名：ext 带 BPSProfile、模板是非 BPS ⇒ 摘要必须 ""。
//
// 这一格正是 ext-only 签名（BPSProfileDigest(ext *AccountExt)）会答错的地方：它会
// 返回非空摘要，把"账号根本不消费的数据"绑进候选身份。末尾的正例对照让"恒返回空"
// 这种作弊实现也红。
func TestBPSProfileDigestIgnoresResidualProfileOnNonBPSTemplates(t *testing.T) {
	ext := &AccountExt{BPSProfile: []byte(gridProfileJSON)}
	// 表外类型（"" 与 unknown）同样不得产出摘要：IsBPS 的 `ok &&` 语义是既有契约。
	for _, typ := range []credential.Type{
		credential.TypeAPIKey,
		credential.TypeResponsesSpecial,
		credential.TypeCodexOAuth,
		credential.TypeCodexPAT,
		credential.TypeCustom,
		"",
		"unknown-type",
	} {
		require.Equal(t, "", BPSProfileDigest(typ, ext),
			"type=%q: 非 BPS 模板带 BPSProfile 的 ext 不得进摘要（残留字段）", typ)
	}
	// 正例对照：同一段 ext 在 BPS 下必须产出摘要。
	require.Equal(t, gridProfileDigest, BPSProfileDigest(credential.TypeBPS, ext),
		"BPS 模板的 profile 摘要必须非空：否则上面的负例可以用恒空实现作弊")
}

// TestBPSProfileDigestAbsentProfileIsEmpty 钉住"未配置 profile 的 BPS 账号没有摘要"
// 这一既有语义（候选过滤据此判不可路由）：空 / 空白 / JSON null / 非 object 一律 ""。
func TestBPSProfileDigestAbsentProfileIsEmpty(t *testing.T) {
	cases := []struct {
		name    string
		profile []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"blank", []byte(" \n\t ")},
		{"json null", []byte("null")},
		{"array", []byte(`[1]`)},
		{"string", []byte(`"x"`)},
		{"broken json", []byte(`{"profile_id":`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, "", BPSProfileDigest(credential.TypeBPS, &AccountExt{BPSProfile: tc.profile}),
				"没有可用的 profile 就没有摘要（不得退化成伪摘要）")
		})
	}
	require.Equal(t, "", BPSProfileDigest(credential.TypeBPS, nil), "ext == nil ⇒ 空摘要")
}

// baseURLLabel 只用于失败信息：指针值打出来是地址，看不出维度。
func baseURLLabel(v *string) string {
	if v == nil {
		return "<nil>"
	}
	return `"` + *v + `"`
}

func extLabel(ext *AccountExt) string {
	if ext == nil {
		return "<nil>"
	}
	return "with-bps-profile"
}
