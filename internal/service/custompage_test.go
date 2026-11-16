// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// customPages21Input 21 条合法条目（V2 上限 20 的越界形态）。挑 V2 当"能解析
// 但违反列表级规则"的代表：单条都合法，只有列表级校验才拦得住。
func customPages21Input() string {
	items := make([]string, 0, 21)
	for i := range 21 {
		items = append(items, fmt.Sprintf(`{"id":"p%d","label":"P%d","url":"https://example.com/%d"}`, i, i, i))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// newCustomPagesTestService 字面量 Service（绕过 New，照 setting_test.go 现有写法）。
// 先 ReloadSettings 建快照：读路径走快照，写路径的内部 reload 才有"旧快照"可比。
func newCustomPagesTestService(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	require.NoError(t, svc.ReloadSettings(context.Background()))
	return svc
}

// TestUpdateSettingCustomPagesNormalizes §11-T5 ①②：非规范输入（键乱序 /
// label 带首尾空格 / 缺 visibility、icon、enabled）落库后必须是规范串，且
// 返回体、库内值、快照值三者字节相等（Go 的字符串相等即字节相等）。
func TestUpdateSettingCustomPagesNormalizes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "键乱序且label带首尾空格",
			raw:  `[{"url":"https://docs.example.com/guide","label":"  Docs  ","icon":"book","visibility":"admin","id":"docs","enabled":true}]`,
			want: `[{"id":"docs","label":"Docs","url":"https://docs.example.com/guide","visibility":"admin","icon":"book","enabled":true}]`,
		},
		{
			name: "缺visibility_icon_enabled_补默认",
			// query 里的 & 必须原样留下（SetEscapeHTML(false)），否则库内字节
			// 与管理员在设置页看到的字符串不一致。
			raw:  `[{"id":"status","label":"  状态页  ","url":"https://status.example.com/?a=1&b=2#frag"}]`,
			want: `[{"id":"status","label":"状态页","url":"https://status.example.com/?a=1&b=2#frag","visibility":"user","icon":"link","enabled":true}]`,
		},
		{
			name: "顺序即展示顺序_不排序",
			raw:  `[{"id":"z","label":"Z","url":"https://z.example.com/"},{"id":"a","label":"A","url":"https://a.example.com/"}]`,
			want: `[{"id":"z","label":"Z","url":"https://z.example.com/","visibility":"user","icon":"link","enabled":true},{"id":"a","label":"A","url":"https://a.example.com/","visibility":"user","icon":"link","enabled":true}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := newCustomPagesTestService(t, fs)

			set, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, set.Value, "返回体是归一化值")

			stored, err := fs.GetSetting(ctx, domain.SettingCustomPages)
			require.NoError(t, err)
			require.Equal(t, tc.want, stored.Value, "库内字节与期望规范串相等")

			require.Equal(t, tc.want, svc.settingValue(domain.SettingCustomPages),
				"快照值与库内值同串（三者一致）")
		})
	}
}

// TestUpdateSettingCustomPagesIdempotent §11-T5 ③：同一份配置再存一次字节不变；
// 把上一次的归一化结果原样再存也必须是同一串（Normalize/Marshal 幂等）。
func TestUpdateSettingCustomPagesIdempotent(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newCustomPagesTestService(t, fs)
	raw := `[{"url":"https://a.example.com/x?b=2&a=1","label":" A ","id":"a"},{"id":"b","label":"B","url":"https://b.example.com/","visibility":"admin","icon":"chart","enabled":false}]`

	first, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, raw)
	require.NoError(t, err)
	second, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, raw)
	require.NoError(t, err)
	require.Equal(t, first.Value, second.Value, "同一份原始配置重复保存字节不变")

	third, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, first.Value)
	require.NoError(t, err)
	require.Equal(t, first.Value, third.Value, "归一化结果再存一次字节不变")

	stored, err := fs.GetSetting(ctx, domain.SettingCustomPages)
	require.NoError(t, err)
	require.Equal(t, first.Value, stored.Value)
}

// TestUpdateSettingCustomPagesEmptyForms §4.5 空值语义（D11）：""/"[]"/"null"
// 都按空配置处理并不报错，落库统一为 "[]"（"null" 只能来自手改 DB，写入路径
// 永不产出）。
func TestUpdateSettingCustomPagesEmptyForms(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{"", "[]", "null"} {
		t.Run(fmt.Sprintf("raw=%q", raw), func(t *testing.T) {
			fs := newFakeStore()
			svc := newCustomPagesTestService(t, fs)

			set, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, raw)
			require.NoError(t, err)
			require.Equal(t, "[]", set.Value)

			stored, err := fs.GetSetting(ctx, domain.SettingCustomPages)
			require.NoError(t, err)
			require.Equal(t, "[]", stored.Value)

			pages, err := svc.CustomPages()
			require.NoError(t, err)
			require.Empty(t, pages)
		})
	}
}

// TestUpdateSettingCustomPagesRejectsInvalid §11-T5 ④：每类非法都 → ErrInvalidInput，
// 且失败整笔不写——库内旧值与快照都必须保持上一次成功保存的字节。
// （M6 变异点：删掉 UpdateSetting 的校验分支后本用例必红。）
func TestUpdateSettingCustomPagesRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		raw  string
	}{
		{"语法错", `{oops`},
		{"非数组", `{"id":"docs","label":"Docs","url":"https://docs.example.com/"}`},
		{"javascript伪协议", `[{"id":"docs","label":"Docs","url":"javascript:alert(1)"}]`},
		{"相对路径", `[{"id":"docs","label":"Docs","url":"/relative"}]`},
		{"userinfo", `[{"id":"docs","label":"Docs","url":"https://u:p@h/"}]`},
		{"重复id", `[{"id":"docs","label":"A","url":"https://a.example.com/"},{"id":"docs","label":"B","url":"https://b.example.com/"}]`},
		{"大写id", `[{"id":"Docs","label":"Docs","url":"https://docs.example.com/"}]`},
		{"空label", `[{"id":"docs","label":"   ","url":"https://docs.example.com/"}]`},
		{"21条", customPages21Input()},
		{"坏visibility", `[{"id":"docs","label":"Docs","url":"https://docs.example.com/","visibility":"all"}]`},
		{"坏icon", `[{"id":"docs","label":"Docs","url":"https://docs.example.com/","icon":"rocket"}]`},
		{"enabled非布尔", `[{"id":"docs","label":"Docs","url":"https://docs.example.com/","enabled":"true"}]`},
	}
	fs := newFakeStore()
	svc := newCustomPagesTestService(t, fs)
	seed := `[{"id":"seed","label":"Seed","url":"https://seed.example.com/"}]`
	set, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, seed)
	require.NoError(t, err)
	before := set.Value
	require.NotEqual(t, "[]", before, "旧值必须非空，才能证明失败没有整笔覆盖")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, tc.raw)
			require.ErrorIs(t, err, ErrInvalidInput)

			stored, err := fs.GetSetting(ctx, domain.SettingCustomPages)
			require.NoError(t, err)
			require.Equal(t, before, stored.Value, "非法输入不得落库（库内旧值不变）")

			require.Equal(t, before, svc.settingValue(domain.SettingCustomPages),
				"非法输入不得污染快照")
		})
	}
}

// TestUpdateSettingCustomPagesVisibleImmediately §11-T5 ⑤：UpdateSetting 之后
// CustomPages() 立即返回新值（读快照，不需要任何外部失效广播）。
func TestUpdateSettingCustomPagesVisibleImmediately(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newCustomPagesTestService(t, fs)

	pages, err := svc.CustomPages()
	require.NoError(t, err)
	require.Empty(t, pages, "默认值 []（功能未启用）")

	set, err := svc.UpdateSetting(ctx, domain.SettingCustomPages,
		`[{"id":"docs","label":" Docs ","url":"https://docs.example.com/"}]`)
	require.NoError(t, err)

	got, err := svc.CustomPages()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "docs", got[0].ID)
	require.Equal(t, "Docs", got[0].Label)
	require.Equal(t, domain.CustomPageVisibilityUser, got[0].Visibility)
	require.Equal(t, domain.CustomPageIconLink, got[0].Icon)
	require.True(t, got[0].Enabled)
	require.Equal(t, set.Value, svc.settingValue(domain.SettingCustomPages),
		"返回体与快照同串")
}

// TestUpdateSettingCustomPagesInvalidCarriesPosition §19-3：400 body 直接取
// err.Error()（httpface.writeInvalidInput），所以错误必须**同时**可判别
// （errors.Is(err, ErrInvalidInput)）且携带 domain 的「条目下标 + 字段」详情——
// 少任何一半都会让管理面退回"service: invalid input"这种无法行动的解释。
func TestUpdateSettingCustomPagesInvalidCarriesPosition(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "单条字段错带下标与字段名",
			raw:  `[{"id":"docs","label":"Docs","url":"javascript:alert(1)"}]`,
			want: "custom pages[0]: url must use the http or https scheme",
		},
		{
			name: "下标指向真正有问题的那条",
			raw:  `[{"id":"a","label":"A","url":"https://a.example.com/"},{"id":"b","label":"B","url":"/relative"}]`,
			want: "custom pages[1]:",
		},
		{
			name: "列表级错（条数）仍带 custom pages 前缀",
			raw:  customPages21Input(),
			want: "custom pages: 21 items exceeds max 20",
		},
		{
			name: "解析层错也带定位前缀",
			raw:  `{oops`,
			want: "custom pages: parse:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := newCustomPagesTestService(t, fs)

			_, err := svc.UpdateSetting(ctx, domain.SettingCustomPages, tc.raw)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidInput, "哨兵必须可判别（上层据 400 分流）")
			require.Contains(t, err.Error(), tc.want,
				"详情必须能定位（条目级带下标 custom pages[i]，列表级/解析级带 custom pages 前缀）")
		})
	}
}

// TestCustomPagesReadsSnapshotNotDB §19-5（M3 变异必须真能红）：直接写库但**不**
// ReloadSettings，CustomPages() 必须仍返回旧快照值。现有「UpdateSetting 后立即读」的
// 用例在"读库"实现下同样绿（UpdateSetting 会同时写库），只有这条反向用例能区分
// "读快照"与"读库"。
func TestCustomPagesReadsSnapshotNotDB(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newCustomPagesTestService(t, fs)
	_, err := svc.UpdateSetting(ctx, domain.SettingCustomPages,
		`[{"id":"old","label":"Old","url":"https://old.example.com/"}]`)
	require.NoError(t, err)

	// 绕过 UpdateSetting 与 ReloadSettings：库里换一条（运维手改 DB / 另一实例写入），
	// 本实例快照不动。
	_, err = fs.SetSetting(ctx, domain.SettingCustomPages, domain.SettingTypeString,
		`[{"id":"new","label":"New","url":"https://new.example.com/"}]`)
	require.NoError(t, err)

	pages, err := svc.CustomPages()
	require.NoError(t, err)
	require.Len(t, pages, 1)
	require.Equal(t, "old", pages[0].ID, "读的必须是设置快照，不是库")

	// 重载后新值才可见——证明上面那条不是"恰好读到了同一份缓存"的巧合。
	require.NoError(t, svc.ReloadSettings(ctx))
	pages, err = svc.CustomPages()
	require.NoError(t, err)
	require.Len(t, pages, 1)
	require.Equal(t, "new", pages[0].ID, "ReloadSettings 之后才应看到库里的新值")
}

// TestCustomPagesReadPathRejectsCorruptSnapshot §6-5：读路径也校验。绕过
// UpdateSetting 直接写库（运维手改 / 迁移脚本 / 旧版本回滚），再重载快照——
// CustomPages() 必须报错而不是静默返回空配置，错误文案固定可判别。
func TestCustomPagesReadPathRejectsCorruptSnapshot(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		raw  string
	}{
		{"语法错", `{oops`},
		{"能解析但违反V6", `[{"id":"docs","label":"Docs","url":"javascript:alert(1)"}]`},
		{"能解析但违反V2", customPages21Input()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := newCustomPagesTestService(t, fs)
			// 绕过 UpdateSetting：直接落一条未经校验的值。
			_, err := fs.SetSetting(ctx, domain.SettingCustomPages, domain.SettingTypeString, tc.raw)
			require.NoError(t, err)
			require.NoError(t, svc.ReloadSettings(ctx))

			pages, err := svc.CustomPages()
			require.Error(t, err)
			require.Contains(t, err.Error(), "custom pages config is invalid",
				"固定文案可判别（上层据此返回 500）")
			require.Nil(t, pages, "损坏不得静默降级为空配置")
		})
	}
}
