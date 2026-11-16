// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 自定义嵌入页（custom pages，spec 2026-11-16 §4）：管理面配置用户台/管理台侧栏
// 里的 iframe 入口。存储形态是一条 JSON 数组字符串设置（SettingCustomPages），
// 顺序即展示顺序，空数组 = 功能未启用。
//
// 本文件是解析/归一/校验/序列化的**唯一入口**：service 的写入路径
// （UpdateSetting）与读取路径（Service.CustomPages）都只调这四个函数。别处不得
// 再写 json.Unmarshal——同一份配置若有两条解析路径，校验结论迟早会分叉，非法值
// 就能从没校验的那条漏出去。

// CustomPageVisibility 条目可见范围：admin 只在管理台出现，user 只在用户台出现。
type CustomPageVisibility string

const (
	CustomPageVisibilityAdmin CustomPageVisibility = "admin"
	CustomPageVisibilityUser  CustomPageVisibility = "user"
)

// Valid 命中枚举才算合法；空串也不合法（缺省补齐是 Normalize 的职责，
// 校验不做猜测，否则「写错枚举」和「没写」在这里会被混为一谈）。
func (v CustomPageVisibility) Valid() bool {
	switch v {
	case CustomPageVisibilityAdmin, CustomPageVisibilityUser:
		return true
	}
	return false
}

// CustomPageIcon 侧栏图标名（前端图标注册表的键，本包只管值域）。
type CustomPageIcon string

const (
	CustomPageIconLink     CustomPageIcon = "link"
	CustomPageIconGlobe    CustomPageIcon = "globe"
	CustomPageIconBook     CustomPageIcon = "book"
	CustomPageIconChart    CustomPageIcon = "chart"
	CustomPageIconLifebuoy CustomPageIcon = "lifebuoy"
	CustomPageIconTicket   CustomPageIcon = "ticket"
	CustomPageIconCode     CustomPageIcon = "code"
	CustomPageIconSparkles CustomPageIcon = "sparkles"
)

func (i CustomPageIcon) Valid() bool {
	switch i {
	case CustomPageIconLink, CustomPageIconGlobe, CustomPageIconBook, CustomPageIconChart,
		CustomPageIconLifebuoy, CustomPageIconTicket, CustomPageIconCode, CustomPageIconSparkles:
		return true
	}
	return false
}

const (
	// SettingCustomPages 设置 key：值是一条 JSON 数组字符串（默认 "[]"）。
	SettingCustomPages = "custom_pages"

	// 上限（spec §4.2/§5）：条目数、标签 rune 数、URL/ID 字节数、序列化总字节数。
	// 都是「拒绝」阈值：超过即写入 400 / 读路径 500，不做截断（静默截断会改掉
	// 管理员写下的配置）。
	CustomPageMaxItems      = 20
	CustomPageMaxLabelRunes = 40
	CustomPageMaxURLBytes   = 2048
	CustomPageMaxIDBytes    = 32
	CustomPageMaxTotalBytes = 64 << 10
)

// CustomPageIcons 顺序即前端下拉顺序（§11-T4 跨语言门禁逐位比对，别按字母序重排）。
var CustomPageIcons = []string{"link", "globe", "book", "chart", "lifebuoy", "ticket", "code", "sparkles"}

// CustomPage 一条嵌入页配置。json 键固定小写且按声明序输出
// （id,label,url,visibility,icon,enabled）——库内字节与前端序列化必须一致。
type CustomPage struct {
	ID         string               `json:"id"`
	Label      string               `json:"label"`
	URL        string               `json:"url"`
	Visibility CustomPageVisibility `json:"visibility"`
	Icon       CustomPageIcon       `json:"icon"`
	Enabled    bool                 `json:"enabled"`
}

// IsVisibleToAdmin 管理台专属条目：启用且 visibility=admin。
// 管理台本身还能看到 user 条目（§4.4 角色过滤），所以这里表达的是「只在管理台」。
func (p CustomPage) IsVisibleToAdmin() bool {
	return p.Enabled && p.Visibility == CustomPageVisibilityAdmin
}

// IsVisibleToUser 用户台条目：启用且 visibility=user。
func (p CustomPage) IsVisibleToUser() bool {
	return p.Enabled && p.Visibility == CustomPageVisibilityUser
}

// customPageWire 反序列化中间结构。Enabled 用 *bool 区分「字段未写」与「显式
// false」：直接 bool 会把缺省当 false，管理员少写一个字段页面就凭空消失（spec D11）。
// 未知字段由 encoding/json 默认丢弃（前向兼容：老后端读新前端写的配置不报错）。
type customPageWire struct {
	ID         string               `json:"id"`
	Label      string               `json:"label"`
	URL        string               `json:"url"`
	Visibility CustomPageVisibility `json:"visibility"`
	Icon       CustomPageIcon       `json:"icon"`
	Enabled    *bool                `json:"enabled"`
}

// ParseCustomPages 解析设置原值。空值语义（spec §4.3 D11）：""、"[]"、"null"
// 都表示「未配置」，返回 nil,nil。null 只可能来自手改 DB（Go 把 null 反序列化成
// nil 切片且不报错）；写入路径永远产出 "[]"。
//
// 非数组（对象/字符串/数字）与语法错一律报错：V1 的边界就是 encoding/json 的
// 类型检查，这里不额外放宽。解析不做校验也不补缺省——顺序固定为
// Parse → Normalize → Validate → Marshal（service 两条路径同序）。
func ParseCustomPages(raw string) ([]CustomPage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// 先验 UTF-8，再 Unmarshal（spec §4.3 补充条款 / §19-2）：encoding/json 会把非法
	// 字节**静默替换成 U+FFFD**，于是「非法输入」会以「看起来合法的替换字符」形式落库
	// 并被下发——静默改写比报错更糟。顺序不可调换：放到 Unmarshal 之后检查的就是已经被
	// 替换过的串，永远合法。
	if !utf8.ValidString(trimmed) {
		return nil, fmt.Errorf("custom pages: parse: input is not valid UTF-8")
	}
	var wire []customPageWire
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return nil, fmt.Errorf("custom pages: parse: %w", err)
	}
	if len(wire) == 0 {
		return nil, nil
	}
	pages := make([]CustomPage, 0, len(wire))
	for _, w := range wire {
		page := CustomPage{
			ID:         w.ID,
			Label:      w.Label,
			URL:        w.URL,
			Visibility: w.Visibility,
			Icon:       w.Icon,
			Enabled:    true, // 缺省 = true；显式 false 只在指针非 nil 时生效
		}
		if w.Enabled != nil {
			page.Enabled = *w.Enabled
		}
		pages = append(pages, page)
	}
	return pages, nil
}

// NormalizeCustomPages 补齐缺省并修剪标签；幂等（已归一的值再跑一次不变）。
// 返回新切片而不是就地改：读取路径传进来的是设置快照的值，就地写会污染快照。
//
// 只 trim label：V5 的「trim 后非空」就是针对标签的（用户手输的自由文本）。
// url/id 不 trim——带空白的 URL 或 ID 是配置错误（尤其 url 端点上的换行/空格），
// 静默裁剪会把「含控制字符」这种危险值洗白，交给 V3/V6 报错更好。
func NormalizeCustomPages(pages []CustomPage) []CustomPage {
	if len(pages) == 0 {
		return pages
	}
	out := make([]CustomPage, len(pages))
	for i, page := range pages {
		page.Label = strings.TrimSpace(page.Label)
		// 只补空串；非法枚举（如 "all"）原样留给 Validate 拒绝，不做「纠正」。
		if page.Visibility == "" {
			page.Visibility = CustomPageVisibilityUser
		}
		if page.Icon == "" {
			page.Icon = CustomPageIconLink
		}
		out[i] = page
	}
	return out
}

// CustomPageIDPattern id 形状（spec §4.2 V3）。**导出**是为了让 §11-T4 的跨语言门禁
// 能把它与前端的 `CUSTOM_PAGE_ID_PATTERN` 字面量做等号比对——这条规则若只活在
// ValidateCustomPages 里、前端另写一份，就是没有门禁的第二份真源（改一端另一端静默漂移）。
const CustomPageIDPattern = `^[a-z0-9][a-z0-9-]*$`

// customPageIDPattern 编译版（判定用）；字面量只保留在 CustomPageIDPattern 一处。
var customPageIDPattern = regexp.MustCompile(CustomPageIDPattern)

// CustomPageURLSchemes url 允许的 scheme 白名单（小写；判定前把 url 转小写，故
// `HTTP://` 合法）。同 CustomPageIDPattern：导出 + §11-T4 门禁与前端
// `CUSTOM_PAGE_URL_SCHEMES` 逐位比对，避免"前端放行、后端 400"或反之。
var CustomPageURLSchemes = []string{"http://", "https://"}

// customPageURLAllowedScheme url 是否以白名单里的 scheme 开头（大小写不敏感）。
// 单独成函数是为了让"字面前缀判定"只有一处实现：`http:/x` 这类会被浏览器补全的写法
// 必须在 scheme 判定里就拒掉，不能落到 url.Parse/host 判定上（报错原因会指错方向）。
func customPageURLAllowedScheme(lowered string) bool {
	for _, scheme := range CustomPageURLSchemes {
		if strings.HasPrefix(lowered, scheme) {
			return true
		}
	}
	return false
}

// ValidateCustomPages V1–V9 全量校验。空列表合法（= 功能未启用）。
// 条目级错误带下标前缀 `custom pages[i]: ...`，管理面能直接指出是第几条。
func ValidateCustomPages(pages []CustomPage) error {
	// V2：条目数上限。
	if len(pages) > CustomPageMaxItems {
		return fmt.Errorf("custom pages: %d items exceeds max %d", len(pages), CustomPageMaxItems)
	}
	seen := make(map[string]int, len(pages))
	for i, page := range pages {
		if err := validateCustomPage(page); err != nil {
			return fmt.Errorf("custom pages[%d]: %w", i, err)
		}
		// V4：全表唯一。这里报第二次出现的下标，管理员改那条即可。
		if first, ok := seen[page.ID]; ok {
			return fmt.Errorf("custom pages[%d]: duplicate id %q (first at index %d)", i, page.ID, first)
		}
		seen[page.ID] = i
	}
	// V9：序列化后总值上限（防御性上界）。字段级上限（20×2 KiB）本该够不到
	// 64 KiB，但 JSON 转义会放大（`\` 翻倍、U+2028 变 \u2028 6 字节），且未来
	// 上限可能调大——量的是真正要落库/走快照的那串字节。
	encoded, err := MarshalCustomPages(pages)
	if err != nil {
		return fmt.Errorf("custom pages: marshal: %w", err)
	}
	if len(encoded) > CustomPageMaxTotalBytes {
		return fmt.Errorf("custom pages: encoded %d bytes exceeds max %d", len(encoded), CustomPageMaxTotalBytes)
	}
	return nil
}

// validateCustomPage 单条校验，刻意不导出：唯一调用者是列表校验，导出会让
// 调用方以为可以绕过 V2/V4（条数、唯一性）这类列表级规则。
func validateCustomPage(page CustomPage) error {
	// V3：id 形状 + 字节上限。
	if len(page.ID) > CustomPageMaxIDBytes {
		return fmt.Errorf("id must be at most %d bytes", CustomPageMaxIDBytes)
	}
	if !customPageIDPattern.MatchString(page.ID) {
		return fmt.Errorf("id %q must match ^[a-z0-9][a-z0-9-]*$ (lowercase letters, digits, dashes)", page.ID)
	}
	// V5：label 合法 UTF-8、trim 后非空、≤40 rune。先验 UTF-8 再 trim：
	// 非法字节序列连「空格」都判断不了，让它单独报错更好定位。
	if !utf8.ValidString(page.Label) {
		return fmt.Errorf("label is not valid UTF-8")
	}
	label := strings.TrimSpace(page.Label)
	if label == "" {
		return fmt.Errorf("label is required")
	}
	if n := utf8.RuneCountInString(label); n > CustomPageMaxLabelRunes {
		return fmt.Errorf("label is %d runes, max %d", n, CustomPageMaxLabelRunes)
	}
	// V6：url 必须是 http(s)、有 host、无 userinfo、无控制字符、≤2048 字节。
	if err := validateCustomPageURL(page.URL); err != nil {
		return err
	}
	// V7 / V8：枚举值域。空串不补缺省（缺省由 Normalize 补），直接拒。
	if !page.Visibility.Valid() {
		return fmt.Errorf("visibility %q must be admin or user", page.Visibility)
	}
	if !page.Icon.Valid() {
		return fmt.Errorf("icon %q is not in CustomPageIcons", page.Icon)
	}
	return nil
}

// validateCustomPageURL V6 的 URL 判定。顺序有意义：先钉 scheme，再谈 host/
// userinfo——javascript:/data:/file: 这类伪协议没有 host，光靠 url.Parse 的
// Host=="" 也「碰巧」能拒，但报错会指向错误的原因，且换个写法就可能漏
// （spec §11-T1 的 M1 变异就是删这个判定，必须让 javascript: 用例立刻变红）。
func validateCustomPageURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	// 非法 UTF-8 在落库前就必须拒：encoding/json 序列化会把它替换成 U+FFFD，
	// 存进去的就不再是管理员写的那个 URL（静默改写比报错更糟）。
	if !utf8.ValidString(raw) {
		return fmt.Errorf("url is not valid UTF-8")
	}
	for _, r := range raw {
		// 控制字符（\n、\t、DEL、C1）会打断 iframe src 与请求行；不清洗，直接拒。
		if unicode.IsControl(r) {
			return fmt.Errorf("url must not contain control characters")
		}
	}
	if len(raw) > CustomPageMaxURLBytes {
		return fmt.Errorf("url must be at most %d bytes", CustomPageMaxURLBytes)
	}
	// scheme 大小写不敏感（HTTP:// 合法），但必须是字面 http(s)://（白名单
	// CustomPageURLSchemes 单一真源，§11-T4 与前端逐位比对）。
	lowered := strings.ToLower(raw)
	if !customPageURLAllowedScheme(lowered) {
		return fmt.Errorf("url must use the http or https scheme")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %v", err)
	}
	if u.Host == "" {
		return fmt.Errorf("url must have a host")
	}
	// 凭据不能进配置：条目是给所有目标角色看的，user:pass@ 会随条目泄露；
	// 且 iframe 里的凭据会绕开账号体系，直接拒。
	if u.User != nil {
		return fmt.Errorf("url must not contain userinfo")
	}
	return nil
}

// MarshalCustomPages 序列化为落库字符串：键序固定、不带行尾换行、nil/空 → "[]"。
//
// SetEscapeHTML(false)：url 里的 & < > 原样留下（默认转义成 \u0026 会让库内字节
// 与管理员在设置页看到的字符串不一致，diff/排障都变难）。这也让同一份配置
// 反复序列化字节相等（§11-T5 的前提）。
func MarshalCustomPages(pages []CustomPage) (string, error) {
	if len(pages) == 0 {
		return "[]", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(pages); err != nil {
		return "", fmt.Errorf("custom pages: marshal: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
