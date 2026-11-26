// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/is7qin/c3api/pkg/headervalid"
)

// 出站请求修饰器（spec custom-request-patch §2/§5.1/§5.2）的**领域形状与唯一解析入口**。
//
// 语义总览：
//   - Headers 是全局层，Formats 是七键格式层；格式层逐键覆盖全局层（同名不歧义）。
//   - 每层内：非空值 ⇒ Set/Del 覆盖网关最终出网头；值 "" ⇒ Del（删除该头）。
//   - Formats.<k>.Set 是 gjson path → 原始 JSON 值（字节级保存，不经过 float64）；
//     Delete 是 path 列表。运行期执行顺序 delete（排序）→ set（排序）在数据面，
//     本层只做保存期文法/合法性校验（spec §2.5/§5.2）。
//   - 空层归一 nil（headers 空 / formats 空 / format patch 全空 / 全空 ⇒ nil），
//     保证「未配置」只有一种落库形态（NULL）。
//
// 本类型只被配置面（domain.TemplateExt / repository / service / handler）使用；
// 数据面消费（proxy/probe 应用）是 P2/P3，届时直接读快照里的只读实例。

// RequestPatch 模板出站请求修饰器（custom 专用；nil = 未配置）。
type RequestPatch struct {
	// Headers 全局层头：键为 http.CanonicalHeaderKey 规范形；值 "" = 删除。
	Headers map[string]string
	// Formats 格式层（七键词汇，见 upstream_paths.go）。
	Formats map[UpstreamPathKey]FormatPatch
}

// FormatPatch 单个出站格式的修饰（空 patch 在解析期被归一删除）。
type FormatPatch struct {
	// Headers 格式层头：同名覆盖全局层；值 "" = 删除（含覆盖全局层 Set 的三态语义）。
	Headers map[string]string
	// Set gjson path → 原始 JSON 值（json.RawMessage 保原字面量，大整数不丢精度）。
	Set map[string]json.RawMessage
	// Delete 待删除的 gjson path 列表（解析期去重、执行期排序，保存期只校验文法）。
	Delete []string
}

// requestPatchWire 是 wire 解码形状（唯一入口的内部细节）。用 map[string]string
// 承接 header 值：JSON null 解到 string 是零值 ""（不报错），与空串同义 = Del
// （spec §5.2 三态表的第一行）。
type requestPatchWire struct {
	Formats map[string]formatPatchWire `json:"formats"`
	Headers map[string]string          `json:"headers"`
}

type formatPatchWire struct {
	Delete  []string                   `json:"delete"`
	Headers map[string]string          `json:"headers"`
	Set     map[string]json.RawMessage `json:"set"`
}

// ParseRequestPatch 是 wire JSON → domain.RequestPatch 的**唯一入口**（保存期校验
// 与 repo 读路径共用；spec §2.6/§5.2）。nil / JSON null / "{}" ⇒ (nil, nil)。
//
// 校验只针对 wire 合法性（不设策略限制），完整清单：
//   - 头名：RFC 9110 token（pkg/headervalid.ValidHeaderName，全仓单源）；一律先
//     http.CanonicalHeaderKey 归一后存储；同一层内归一后同名（X-A / x-a）⇒ error
//     （歧义配置）；跨层同名合法（格式层覆盖全局层）。
//   - 头值：pkg/headervalid.ValidHeaderValue（与本包其余保存期判据同源）；允许
//     {api_key} 占位符（不展开，本轮不识别任何变量）；"" = Del。
//   - Accept-Encoding（规范形）任意一层非空值 ⇒ error：**出站响应编码不变量**
//     （spec §2.4 评审 oracle C1）——它是计费/明文响应体的机械前提，不是请求
//     语义限制；空串/Del 允许（Go 会追加默认 gzip 并透明解压）。运行期仍会
//     防御性跳过（脏库值），保存期这里是第一道门。
//   - formats 键必须属于七键（domain.UpstreamPathFormat 的 ok），未知键 ⇒ error。
//   - set 值必须 json.Valid；set/delete 的 path 过 ValidateSJSONPath 文法校验。
//   - delete 列表解析期去重（保留首次出现顺序；spec §17.4 / 评审 momus m3）——
//     重复项运行期无副作用，但会让 P2 digest 因书写重复算出不同 key。
//   - 空层归一 nil（见类型注释）。
//
// 错误消息**只含字段名/path，不含 header 值或 body 值**：配置值可能是密钥
// （spec §2.4/§8），错误会进 API 响应体。
func ParseRequestPatch(raw json.RawMessage) (*RequestPatch, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var wire requestPatchWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("not a valid object: %w", err)
	}
	out := &RequestPatch{}
	headers, err := normalizePatchHeaders(wire.Headers)
	if err != nil {
		return nil, err
	}
	out.Headers = headers
	if len(wire.Formats) > 0 {
		formats := make(map[UpstreamPathKey]FormatPatch, len(wire.Formats))
		for key, fw := range wire.Formats {
			if _, ok := UpstreamPathFormat(key); !ok {
				return nil, fmt.Errorf("unknown format key %q", key)
			}
			fp, err := normalizeFormatPatch(key, fw)
			if err != nil {
				return nil, err
			}
			if fp.isEmpty() {
				continue
			}
			formats[UpstreamPathKey(key)] = fp
		}
		if len(formats) > 0 {
			out.Formats = formats
		}
	}
	if out.IsEmpty() {
		return nil, nil
	}
	return out, nil
}

// normalizePatchHeaders 校验并归一**一层**头（全局层/格式层共用）：头名 token +
// 规范形归一 + 同层归一后同名歧义 + 值卫生 + Accept-Encoding 硬拒。
// 空入参 ⇒ (nil, nil)。
func normalizePatchHeaders(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for rawName, value := range in {
		if !headervalid.ValidHeaderName(rawName) {
			return nil, fmt.Errorf("invalid header name %q", rawName)
		}
		// 归一必须**先于**一切比较：Accept-Encoding 拒绝、Host 特例与运行期
		// Set/Del 都按规范形比较，否则小写 accept-encoding 能绕过唯一硬边界
		// （relay.go:20-23 有同型历史教训；spec §2.6 评审 momus I-C）。
		name := http.CanonicalHeaderKey(rawName)
		if _, ok := out[name]; ok {
			// 同一层两个不同写法归一到同一个规范形 = 歧义配置（同名到底 Del 还是
			// Set？）。值可能不同，消息不回显值。
			return nil, fmt.Errorf("duplicate header %q after canonicalization", name)
		}
		if !headervalid.ValidHeaderValue(value) {
			return nil, fmt.Errorf("invalid value for header %q", name)
		}
		if name == "Accept-Encoding" && value != "" {
			return nil, fmt.Errorf("Accept-Encoding must not be set (outbound response encoding invariant)")
		}
		out[name] = value
	}
	return out, nil
}

// normalizeFormatPatch 校验并归一单个格式层；空 patch ⇒ (zero, nil)。
func normalizeFormatPatch(key string, w formatPatchWire) (FormatPatch, error) {
	var out FormatPatch
	headers, err := normalizePatchHeaders(w.Headers)
	if err != nil {
		return FormatPatch{}, err
	}
	out.Headers = headers
	if len(w.Set) > 0 {
		set := make(map[string]json.RawMessage, len(w.Set))
		for path, value := range w.Set {
			if err := ValidateSJSONPath(path); err != nil {
				return FormatPatch{}, fmt.Errorf("formats.%s.set: %w", key, err)
			}
			canonical, err := canonicalJSONValue(value)
			if err != nil {
				return FormatPatch{}, fmt.Errorf("formats.%s.set[%q]: value is not valid JSON", key, path)
			}
			set[path] = canonical
		}
		out.Set = set
	}
	if len(w.Delete) > 0 {
		// 解析期去重（spec §17.4 / 评审 momus m3）：运行期重复 delete 无副作用，
		// 但 P2 digest 会把「重复项」与「去重后语义相同」的配置算成不同 key。
		// 保留首次出现顺序（CanonicalBytes 再统一排序）。
		seen := make(map[string]struct{}, len(w.Delete))
		del := make([]string, 0, len(w.Delete))
		for _, path := range w.Delete {
			if err := ValidateSJSONPath(path); err != nil {
				return FormatPatch{}, fmt.Errorf("formats.%s.delete: %w", key, err)
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			del = append(del, path)
		}
		out.Delete = del
	}
	return out, nil
}

// canonicalJSONValue 把 set 的原始 JSON 值归一成**确定性字节**：解码走
// json.Decoder.UseNumber（数字字面量原样保留，绝不经 float64），再用
// SetEscapeHTML(false) 的编码器输出紧凑形——嵌套对象的键按字典序、去掉源文本的
// 空白。这样「语义相同的值、不同的书写形态」得到同一份规范字节，P2 digest 不会
// 因为客户端把嵌套对象写错顺序/多空格而换叶；字符串里的 & < > 保持原字节
// （不是 \u00XX 转义），回显与人读都更接近配置原文。
//
// 值来自外层 JSON 解码（结构上合法），错误分支只在手构非法 RawMessage 时可达。
func canonicalJSONValue(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// isEmpty 报告格式层是否为空（三字段皆空）。
func (p FormatPatch) isEmpty() bool {
	return len(p.Headers) == 0 && len(p.Set) == 0 && len(p.Delete) == 0
}

// IsEmpty 报告 patch 是否为空（nil 或两层皆空）。空 patch 落库为 NULL，与
// 「未配置」同一形态（spec §2.1）。
func (p *RequestPatch) IsEmpty() bool {
	return p == nil || (len(p.Headers) == 0 && len(p.Formats) == 0)
}

// CanonicalBytes 返回确定性规范序列化（同输入不同 map 迭代序 ⇒ 同字节；供
// repository 落库与 P2 的 template digest 共用，spec §2.6/§5.1）。nil/空 ⇒ nil。
//
// 规范化规则：
//   - headers / formats / set 的键按字典序（encoding/json 对 map 唯一排序）；
//   - set 值**不是**原样直通：ParseRequestPatch 已在解析期做深归一
//     （canonicalJSONValue：UseNumber 保数字字面量 + 嵌套键序/空白/HTML 转义
//     一次收敛），本函数再按 JSON 编码形状紧凑输出——大整数 1e18 /
//     9007199254740993 不经过 float64，逐字保留；
//   - delete 排序后输出（解析期已去重，重复项不会产生不同摘要）；
//   - 不转义 HTML 字符（SetEscapeHTML(false)：值按 json.Compact 原字节，而不是
//     把 & < > 变成 \u00XX——回显与摘要都以「原字节」为准）。
//
// **落库/读回边界（P2 digest 必读）**：本函数只对**内存中解析后的值**保证
// 确定性；jsonb 读回会再规范化数字字面量——指数写法 1e18 展开成
// 1000000000000000000（语义/精度不变），>2^53 的整数字面量逐字保留。因此 P2 的
// digestTemplateUpstream 必须用**读回/解析后**的 Patch.CanonicalBytes()，不能用
// 写入前的 bytes，否则同一配置在「写后」与「快照期」会得到不同摘要
// （spec §17.2）。
//
// 空层不输出（omitempty）：CanonicalBytes 是落库/digest 的规范形，不是回显协议。
func (p *RequestPatch) CanonicalBytes() []byte {
	if p.IsEmpty() {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p.canonical()); err != nil {
		// 不可达：canonical() 只含 map[string]string 与已 JSON 合法的 RawMessage
		// （ParseRequestPatch 已校验）。手构非法 RawMessage 时按未配置处理，
		// 不 panic（读路径脏值语义只到 repo 为止，这里是最后一道）。
		return nil
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// canonicalPatch 是 CanonicalBytes 的编码形状（字段顺序固定为顶层 headers →
// formats；内层固定 headers → set → delete，与 spec §2.2 的示例同序）。
type canonicalPatch struct {
	Headers map[string]string               `json:"headers,omitempty"`
	Formats map[string]canonicalFormatPatch `json:"formats,omitempty"`
}

type canonicalFormatPatch struct {
	Headers map[string]string          `json:"headers,omitempty"`
	Set     map[string]json.RawMessage `json:"set,omitempty"`
	Delete  []string                   `json:"delete,omitempty"`
}

// canonical 把 domain 形状转成固定的编码形状；delete 排序（执行期语义「delete
// 路径排序后执行」同序，digest 不依赖配置书写顺序）。
func (p *RequestPatch) canonical() canonicalPatch {
	out := canonicalPatch{Headers: p.Headers}
	if len(p.Formats) > 0 {
		formats := make(map[string]canonicalFormatPatch, len(p.Formats))
		for key, fp := range p.Formats {
			cf := canonicalFormatPatch{Headers: fp.Headers, Set: fp.Set}
			if len(fp.Delete) > 0 {
				del := append([]string(nil), fp.Delete...)
				sort.Strings(del)
				cf.Delete = del
			}
			formats[string(key)] = cf
		}
		out.Formats = formats
	}
	return out
}

// ValidateSJSONPath 校验 sjson/gjson path 的**文法**（保存期 400 的一部分，
// spec §2.6/§5.2 评审 oracle I6：空文档试跑 sjson 只作辅助，不能当唯一门）。
//
// 允许：点分键（任意非保留字符；\ 转义任意一个后继字符，用于字面 '\'/'.' 等）、
// 数组下标（十进制，可负：tools.-1）、首字符 ':' 的 force-string-key 前缀
// （sjson 的既有文法，不是表达式）。
//
// 拒绝：空 path、空段（前导/尾随/连续 '.'）、尾随转义符（sjson 会静默吞掉它，
// 静默丢字符不可接受）、未转义的复杂表达式字符 | # @ * ? [ ]（gjson 通配/过滤器/
// 修饰符文法；sjson set 只走简单路径，spec 明确不开放）、路径中的 NUL/控制字节。
func ValidateSJSONPath(path string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	// sjson 的 force-string-key 前缀：仅首字符有效，单独出现没有段可指 ⇒ 拒绝。
	if path[0] == ':' {
		if len(path) == 1 {
			return fmt.Errorf("path %q has no segments", path)
		}
		path = path[1:]
	}
	// 单遍扫描：转义字符会把后继字节**原样收进当前段**（sjson 的 parsePath 同
	// 语义：escaped '.' 不分割、escaped '\' 是字面反斜杠），因此分割与检查必须
	// 在同一遍里做，不能先 split 再查段。
	seg := make([]byte, 0, len(path))
	flush := func() error {
		if len(seg) == 0 {
			return fmt.Errorf("path has an empty segment")
		}
		seg = seg[:0]
		return nil
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' {
			if i == len(path)-1 {
				return fmt.Errorf("path ends with a dangling escape")
			}
			i++ // 转义的任何一个后继字节都是字面字符（与 sjson 一致）
			seg = append(seg, path[i])
			continue
		}
		if c == '.' {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("path contains a control character")
		}
		switch c {
		case '|', '#', '@', '*', '?', '[', ']':
			// gjson 的通配/过滤器/查询/修饰符文法（如 a.#(b==1)）；sjson 的 set 只走
			// 简单路径（parsePath 的 isSimpleChar 对 |#@*? 走复杂路径；[ ] 在 set 里
			// 会被当字面键——语义歧义，静默 no-op 的来源，故一并拒绝）。
			return fmt.Errorf("path contains an unsupported expression character %q", string(c))
		}
		seg = append(seg, c)
	}
	return flush()
}
