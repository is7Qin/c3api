package protoconv

import (
	"strings"

	"github.com/tidwall/gjson"
)

// schemaMode 工具参数 schema 的处理模式。零值必须是 Unknown（fail-fast），
// 由 appendMessSchema 的 default: panic 与 validateToolsMaps 双向兜底。
type schemaMode uint8

const (
	schemaModeUnknown schemaMode = iota
	schemaModeMessDialect
	schemaModeObjectOnly
	schemaModeExistence
)

// degenerateKind 退化种类。写序处按它选择追加哪一份提示文案（v18 定稿）。
type degenerateKind uint8

const (
	degNone degenerateKind = iota
	degSimplify
	degBail
)

const (
	maxRefHops          = 32
	maxDescriptionBytes = 8192

	degenerateHint = " [args simplified: some constraints could not be expressed]"
	bailHint       = " [args dropped: schema could not be preserved, call with no arguments]"
)

// forbiddenRootKeys 根级禁键（decoded 比较）。
var forbiddenRootKeys = [...]string{"oneOf", "anyOf", "allOf", "enum", "const", "not"}

// refKeys 引用键（decoded 比较，盲扫用）。
var refKeys = [...]string{"$ref", "$dynamicRef", "$recursiveRef"}

// bailKeys 出现即兜底的键（基址重置/资源边界，无法证明本地指针仍指向同一位置）。
var bailKeys = [...]string{"$id", "$anchor", "$dynamicAnchor"}

// metaBranchKeys 4c 的元信息桶：丢弃不损语义。
var metaBranchKeys = map[string]struct{}{
	"title": {}, "description": {}, "default": {}, "examples": {}, "$comment": {},
	"deprecated": {}, "readOnly": {}, "writeOnly": {}, "$schema": {}, "$id": {},
	"$anchor": {}, "$dynamicAnchor": {}, "$vocabulary": {},
	"contentEncoding": {}, "contentMediaType": {},
}

// ---------------------------------------------------------------------------
// schemaDoc：源 schema 文档基准
// ---------------------------------------------------------------------------

// span 值在原字节中的半开区间 [lo,hi)。
type span struct{ lo, hi int }

// schemaDoc schema 文档基准。字段非导出，只能经 newSchemaDoc 构造。
// 不持有 gjson.Result：gjson 的指针求值与多值读取都是"首胜"，与第 1/9 步的
// "末个胜"相反 ⇒ 一切分段匹配自扫 raw。
type schemaDoc struct {
	raw  []byte
	memo map[string]span // 请求级 Pointer→值区间；懒建；{lo:-1} 表示解析失败
}

func newSchemaDoc(raw []byte) *schemaDoc { return &schemaDoc{raw: raw} }

var failedSpan = span{lo: -1, hi: -1}

// resolve 解析本地 Pointer 子集：接受 "#" 与 "#/a/b"；段内 ~1→'/'、~0→'~'
//（先 ~1 后 ~0，故 "~01" 得到字面量 "~1"）；不做 URI percent 解码；
// 数组段按十进制下标。每一层只在当前对象区间内扫键，重名取末个。
func (d *schemaDoc) resolve(ptr string) ([]byte, bool) {
	if d == nil {
		return nil, false
	}
	if s, ok := d.memo[ptr]; ok {
		if s.lo < 0 {
			return nil, false
		}
		return d.raw[s.lo:s.hi], true
	}
	lo, hi, ok := d.resolveUncached(ptr)
	if d.memo == nil {
		d.memo = make(map[string]span, 8)
	}
	if !ok {
		d.memo[ptr] = failedSpan
		return nil, false
	}
	d.memo[ptr] = span{lo: lo, hi: hi}
	return d.raw[lo:hi], true
}

func (d *schemaDoc) resolveUncached(ptr string) (int, int, bool) {
	if ptr == "#" {
		return 0, len(d.raw), true
	}
	if !strings.HasPrefix(ptr, "#/") {
		return 0, 0, false
	}
	rest := ptr[2:]
	lo, hi := 0, len(d.raw)
	for {
		var seg string
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			seg, rest = rest, ""
		}
		name := pointerUnescape(seg)
		var ok bool
		lo, hi, ok = stepInto(d.raw, lo, hi, name)
		if !ok {
			return 0, 0, false
		}
		if rest == "" {
			return lo, hi, true
		}
	}
}

// stepInto 在当前值 [lo,hi) 里按对象键名或数组下标前进一层。
func stepInto(b []byte, lo, hi int, name string) (int, int, bool) {
	if lo < 0 || hi > len(b) || lo >= hi {
		return 0, 0, false
	}
	switch b[lo] {
	case '{':
		var outLo, outHi int
		found := false
		ok := scanObject(b, lo, hi, func(keyRaw []byte, vl, vh int) bool {
			if rawKeyEq(keyRaw, name) { // 末个胜：继续扫，覆盖
				outLo, outHi, found = vl, vh, true
			}
			return true
		})
		if !ok || !found {
			return 0, 0, false
		}
		return outLo, outHi, true
	case '[':
		idx, ok := parseArrayIndex(name)
		if !ok {
			return 0, 0, false
		}
		var outLo, outHi int
		found := false
		n := 0
		ok = scanArray(b, lo, hi, func(vl, vh int) bool {
			if n == idx {
				outLo, outHi, found = vl, vh, true
			}
			n++
			return n <= idx
		})
		if !ok || !found {
			return 0, 0, false
		}
		return outLo, outHi, true
	}
	return 0, 0, false
}

func parseArrayIndex(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	if s == "0" {
		return 0, true
	}
	if s[0] == '0' { // 禁前导零
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// pointerUnescape JSON Pointer 段反转义（先 ~1 后 ~0）。
func pointerUnescape(s string) string {
	if !strings.ContainsRune(s, '~') {
		return s
	}
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// ---------------------------------------------------------------------------
// 字节级扫描原语
// ---------------------------------------------------------------------------

// rawKeyEq 判定原始键（含引号）的 JSON 解码名是否等于 name。
// 算法与 jsraw.go 的 gjsonKeyEq 逐字对齐：转义检测先于长度判定。
func rawKeyEq(keyRaw []byte, name string) bool {
	if len(keyRaw) < 2 || keyRaw[0] != '"' || keyRaw[len(keyRaw)-1] != '"' {
		return false
	}
	if len(keyRaw) == len(name)+2 {
		for i := 1; i < len(keyRaw)-1; i++ {
			if keyRaw[i] != name[i-1] {
				return false
			}
		}
		return true
	}
	for i := 1; i < len(keyRaw)-1; i++ {
		if keyRaw[i] == '\\' {
			return gjson.ParseBytes(keyRaw).Str == name
		}
	}
	return false
}

// scanObject 遍历 [lo,hi) 内的对象，对每个成员回调（名字原始字节含引号，值区间）。
// 回调返回 false 停止。结构非法返回 false。
func scanObject(b []byte, lo, hi int, fn func(keyRaw []byte, valLo, valHi int) bool) bool {
	return scanObjectPos(b, lo, hi, func(keyRaw []byte, _, _ int, vl, vh int) bool {
		return fn(keyRaw, vl, vh)
	})
}

// scanObjectPos 同 scanObject，额外给出键在 b 中的区间 [keyLo,keyHi)。
func scanObjectPos(b []byte, lo, hi int, fn func(keyRaw []byte, keyLo, keyHi, valLo, valHi int) bool) bool {
	if lo < 0 || hi > len(b) || lo >= hi || b[lo] != '{' {
		return false
	}
	i := skipSpace(b, lo+1)
	if i < hi && b[i] == '}' {
		return true
	}
	for i < hi {
		i = skipSpace(b, i)
		if i >= hi || b[i] != '"' {
			return false
		}
		keyStart := i
		keyEnd, ok := rawStringEnd(b, i)
		if !ok {
			return false
		}
		keyRaw := b[keyStart:keyEnd]
		i = skipSpace(b, keyEnd)
		if i >= hi || b[i] != ':' {
			return false
		}
		i = skipSpace(b, i+1)
		valLo := i
		valHi, ok := skipValue(b, i)
		if !ok {
			return false
		}
		if !fn(keyRaw, keyStart, keyEnd, valLo, valHi) {
			return true
		}
		i = skipSpace(b, valHi)
		if i >= hi {
			return false
		}
		switch b[i] {
		case ',':
			i++
		case '}':
			return true
		default:
			return false
		}
	}
	return false
}

// scanArray 遍历 [lo,hi) 内的数组。回调返回 false 停止。
func scanArray(b []byte, lo, hi int, fn func(valLo, valHi int) bool) bool {
	if lo < 0 || hi > len(b) || lo >= hi || b[lo] != '[' {
		return false
	}
	i := skipSpace(b, lo+1)
	if i < hi && b[i] == ']' {
		return true
	}
	for i < hi {
		i = skipSpace(b, i)
		valLo := i
		valHi, ok := skipValue(b, i)
		if !ok {
			return false
		}
		if !fn(valLo, valHi) {
			return true
		}
		i = skipSpace(b, valHi)
		if i >= hi {
			return false
		}
		switch b[i] {
		case ',':
			i++
		case ']':
			return true
		default:
			return false
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 根级扫描（P_in 与第 1/9 步共用一趟）
// ---------------------------------------------------------------------------

// rootKey 一个根成员：解码名 + 名字/值区间 + 是否同名组的末个。
type rootKey struct {
	name         string
	keyLo, keyHi int
	valLo, valHi int
	last         bool
}

// verdict 根级判定结果。
type verdict struct {
	isObject bool
	keys     []rootKey // 源顺序，只保留同名组的末个
	typeVal  string    // 解码后的根 type（非字符串为空）
	forbidden []string // 出现的禁键（解码名，去重）
	dupCount int
}

func scanRoot(raw []byte) verdict {
	var v verdict
	if len(raw) == 0 || raw[0] != '{' {
		return v
	}
	var all []rootKey
	ok := scanObjectPos(raw, 0, len(raw), func(keyRaw []byte, kLo, kHi, vl, vh int) bool {
		all = append(all, rootKey{
			name:  decodeRawKey(keyRaw),
			keyLo: kLo, keyHi: kHi, valLo: vl, valHi: vh,
		})
		return true
	})
	if !ok {
		return v
	}
	v.isObject = true
	// 末个胜：从后往前标记末个，再按源顺序输出。
	seen := make(map[string]struct{}, len(all))
	keep := make([]bool, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if _, dup := seen[all[i].name]; dup {
			v.dupCount++
			continue
		}
		seen[all[i].name] = struct{}{}
		keep[i] = true
	}
	for i := range all {
		if !keep[i] {
			continue
		}
		k := all[i]
		k.last = true
		v.keys = append(v.keys, k)
		for _, f := range forbiddenRootKeys {
			if k.name == f {
				v.forbidden = append(v.forbidden, f)
			}
		}
		if k.name == "type" {
			if t := gjson.ParseBytes(raw[k.valLo:k.valHi]); t.Type == gjson.String {
				v.typeVal = t.Str
			}
		}
	}
	return v
}

func decodeRawKey(keyRaw []byte) string { return gjson.ParseBytes(keyRaw).Str }

// pIn 源侧后置条件 in-1..in-5。
func (v verdict) pIn(raw []byte) bool {
	if !v.isObject {
		return false
	}
	if v.typeVal != "object" {
		return false
	}
	if len(v.forbidden) != 0 {
		return false
	}
	if v.dupCount != 0 {
		return false
	}
	for _, k := range v.keys {
		if k.name == "$ref" {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 9b 引用安全阀
// ---------------------------------------------------------------------------

// refNameOK 保守白名单（v18）：名字只允许 ASCII 字母数字与 . _ -，长度 1..128。
// 该子集上 JSON 解码、URI percent 解码、Pointer 反转义都是恒等映射，
// 因此放行的引用在客户端必然解析到同一 $defs 名字。
func refNameOK(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// refOK 判定一个引用值（原始 JSON 字符串字节）是否符合安全阀。
// 返回 (是否安全, 需要兜底的其它理由)。
func refOK(valRaw []byte, tables map[string]map[string]struct{}) bool {
	if len(valRaw) < 2 || valRaw[0] != '"' {
		return false // 非字符串：判据是全称的
	}
	s := gjson.ParseBytes(valRaw).Str // JSON 解码
	var name string
	switch {
	case strings.HasPrefix(s, "#/$defs/"):
		name = s[len("#/$defs/"):]
		if _, ok := tables["$defs"][name]; !ok {
			return false
		}
	case strings.HasPrefix(s, "#/definitions/"):
		name = s[len("#/definitions/"):]
		if _, ok := tables["definitions"][name]; !ok {
			return false
		}
	default:
		return false
	}
	return refNameOK(name)
}

// collectTables 收集产物里 $defs / definitions 的键名（JSON 解码后）。
func collectTables(b []byte) map[string]map[string]struct{} {
	tables := map[string]map[string]struct{}{
		"$defs":       {},
		"definitions": {},
	}
	scanObject(b, 0, len(b), func(keyRaw []byte, vl, vh int) bool {
		name := decodeRawKey(keyRaw)
		if name != "$defs" && name != "definitions" {
			return true
		}
		if vl >= len(b) || b[vl] != '{' {
			return true
		}
		scanObject(b, vl, vh, func(k2 []byte, _, _ int) bool {
			tables[name][decodeRawKey(k2)] = struct{}{}
			return true
		})
		return true
	})
	return tables
}

// valveOK 在产物上盲扫：任何引用值不安全、或出现 $id/$anchor/$dynamicAnchor ⇒ 不通过。
func valveOK(b []byte) bool {
	tables := collectTables(b)
	ok := true
	var walk func(lo, hi int)
	walk = func(lo, hi int) {
		if !ok || lo < 0 || hi > len(b) || lo >= hi {
			return
		}
		switch b[lo] {
		case '{':
			scanObject(b, lo, hi, func(keyRaw []byte, vl, vh int) bool {
				name := decodeRawKey(keyRaw)
				for _, bk := range bailKeys {
					if name == bk {
						ok = false
						return false
					}
				}
				isRef := false
				for _, rk := range refKeys {
					if name == rk {
						isRef = true
						break
					}
				}
				if isRef {
					if !refOK(b[vl:vh], tables) {
						ok = false
						return false
					}
					return true
				}
				walk(vl, vh)
				return ok
			})
		case '[':
			scanArray(b, lo, hi, func(vl, vh int) bool {
				walk(vl, vh)
				return ok
			})
		}
	}
	walk(0, len(b))
	return ok
}

// ---------------------------------------------------------------------------
// 归一化 T（第 1~9 步）与 9b 安全阀
// ---------------------------------------------------------------------------

type branchKind uint8

const (
	brRoot branchKind = iota // branch₋₁ / branch₀
	brAllOf
	brAnyOf
	brOneOf
)

// propEnt 合并后的一个属性。
type propEnt struct {
	name string
	raws [][]byte // 跨贡献者同名 ⇒ 多个；单值一个
}

func (p *propEnt) add(raw []byte) {
	for _, r := range p.raws {
		if bytesEq(r, string(raw)) {
			return // 只做字节去重
		}
	}
	p.raws = append(p.raws, raw)
}

// flattenState 一次压平的累积状态。
type flattenState struct {
	props      []propEnt
	propIndex  map[string]int
	forceEmpty map[string]bool // 某组合分支不约束该属性 ⇒ 合并后必须无约束（否则收窄）
	rootReq    []string
	allOfReq   [][]string
	anyOneReq  [][]string
	lossyUnres bool // (L1) 读不到
	lossyDrop  bool // (L2) 丢了分支约束
	lossyRoot  bool // (L3) 删了根约束
	hasCombo   bool
	sawNonObj  bool // 出现过非 object 分支
	anyNonObj  bool // 任一组合分支不是对象 ⇒ 该组"什么都不约束" ⇒ 全部属性必须无约束
	reasons    []string
}

func (st *flattenState) reason(r string) {
	for _, x := range st.reasons {
		if x == r {
			return
		}
	}
	st.reasons = append(st.reasons, r)
}

func (st *flattenState) addProp(name string, raw []byte) {
	if i, ok := st.propIndex[name]; ok {
		st.props[i].add(raw)
		return
	}
	st.propIndex[name] = len(st.props)
	st.props = append(st.props, propEnt{name: name, raws: [][]byte{raw}})
}

// classifyBranch 4c 三桶：返回是否计入 lossy。
func (st *flattenState) classifyBranch(keyName string) {
	if _, ok := metaBranchKeys[keyName]; ok {
		st.reason("dropped-branch-meta")
		return
	}
	if keyName == "type" {
		st.reason("dropped-branch-type")
		return
	}
	switch keyName {
	case "$ref", "$defs", "definitions":
		return // 引用已被解引用、定义表不约束实例：都不算丢弃约束
	}
	st.lossyDrop = true
	st.reason("dropped-branch-constraint")
}

// derefChain 沿本地 $ref 链走到底。返回 (raw, ok)；ok=false 表示"读不到"。
func derefChain(doc *schemaDoc, raw []byte, visiting map[string]struct{}) ([]byte, bool) {
	cur := raw
	for hop := 0; hop < maxRefHops; hop++ {
		r := gjson.ParseBytes(cur)
		if !r.IsObject() || r.Get("$ref").Type != gjson.String {
			return cur, true
		}
		ptr := r.Get("$ref").Str
		if _, seen := visiting[ptr]; seen {
			return cur, true // 环：按当前对象处理（不放宽也不丢内容）
		}
		visiting[ptr] = struct{}{}
		target, ok := doc.resolve(ptr)
		if !ok {
			return nil, false
		}
		if t := gjson.ParseBytes(target); !t.IsObject() {
			return cur, true
		}
		cur = target
	}
	return cur, true
}

// evalMessSchema 跑 P_in → T（1~9）→ 9b 安全阀 → 退化判定。
// 只返回产物与退化种类；不返回任何中间产出（避免自证通道）。
// evalMessSchema 返回 (原字节, 产物字节, 退化种类)：
// 前两者恰有一个非空——P_in 成立时返回原始字符串视图（零分配），否则返回产物。
func evalMessSchema(src gjson.Result) (string, []byte, degenerateKind) {
	raw := []byte(src.Raw)
	if len(raw) == 0 {
		return "", []byte(`{"type":"object"}`), degSimplify
	}
	v := scanRoot(raw)
	if v.pIn(raw) {
		return src.Raw, nil, degNone
	}
	doc := newSchemaDoc(raw)
	out, st := flattenMessSchema(doc, raw)
	if st == nil {
		return "", []byte(`{"type":"object"}`), degSimplify
	}
	// 9b 引用安全阀
	if !valveOK(out) {
		st.reason("dangling-after-flatten")
		return "", []byte(`{"type":"object"}`), degBail
	}
	lossy := st.lossyUnres || st.lossyDrop || st.lossyRoot
	if lossy {
		return "", out, degSimplify
	}
	if st.hasCombo && st.sawNonObj && len(st.props) == 0 {
		return "", out, degSimplify // no-object-branch
	}
	return "", out, degNone
}

// contrib 一个贡献者（分支或根）。
type contrib struct {
	kind   branchKind
	raw    []byte
	isRoot bool // branch₀：其余根键由第 9 步保留，不进 4c 分类
	branch int  // 组合键分支序号；brRoot 用 -1/-2
}

// fieldSpan 取对象里某个键的**末个**取值区间（末个胜，不用 gjson.Get）。
func fieldSpan(obj []byte, name string) (span, bool) {
	v := scanRoot(obj)
	if !v.isObject {
		return span{}, false
	}
	for _, k := range v.keys {
		if k.name == name {
			return span{k.valLo, k.valHi}, true
		}
	}
	return span{}, false
}

// propsOf 取对象的 properties 成员（数组形式：名字 + 值原始字节），贡献者内部末个胜。
func propsOf(obj []byte) []struct {
	name string
	raw  []byte
} {
	sp, ok := fieldSpan(obj, "properties")
	if !ok || sp.lo >= len(obj) || obj[sp.lo] != '{' {
		return nil
	}
	type ent struct {
		name string
		raw  []byte
	}
	var all []ent
	if !scanObjectPos(obj, sp.lo, sp.hi, func(kr []byte, _, _ int, vl, vh int) bool {
		all = append(all, ent{name: decodeRawKey(kr), raw: append([]byte(nil), obj[vl:vh]...)})
		return true
	}) {
		return nil
	}
	seen := map[string]struct{}{}
	keep := make([]bool, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if _, dup := seen[all[i].name]; dup {
			continue
		}
		seen[all[i].name] = struct{}{}
		keep[i] = true
	}
	var out []struct {
		name string
		raw  []byte
	}
	for i := range all {
		if keep[i] {
			out = append(out, all[i])
		}
	}
	return out
}

// requiredOf 取对象的 required 名字（末个胜）。
func requiredOf(obj []byte) []string {
	sp, ok := fieldSpan(obj, "required")
	if !ok || obj[sp.lo] != '[' {
		return nil
	}
	var out []string
	scanArray(obj, sp.lo, sp.hi, func(vl, vh int) bool {
		if e := gjson.ParseBytes(obj[vl:vh]); e.Type == gjson.String {
			out = append(out, e.Str)
		}
		return true
	})
	return out
}

// unionInto 把 names 并入 set。
func unionInto(set map[string]struct{}, names []string) {
	for _, n := range names {
		set[n] = struct{}{}
	}
}

// flattenMessSchema 第 1~9 步：把根级组合关键字压平成普通 object schema。
// raw 必须是 src.Raw（源字节）；doc 是同一个文档基准。
func flattenMessSchema(doc *schemaDoc, raw []byte) ([]byte, *flattenState) {
	st := &flattenState{propIndex: map[string]int{}, forceEmpty: map[string]bool{}}
	v := scanRoot(raw)
	if !v.isObject {
		return []byte(`{"type":"object"}`), nil
	}
	if v.dupCount > 0 {
		st.reason("duplicate-key")
	}
	rootMap := make(map[string]span, len(v.keys))
	for _, k := range v.keys {
		rootMap[k.name] = span{k.valLo, k.valHi}
	}
	rootRaw := func(name string) []byte {
		if s, ok := rootMap[name]; ok {
			return raw[s.lo:s.hi]
		}
		return nil
	}

	// 第 2 步：根 $ref 解引用到底
	var refContrib []byte
	if rootRaw("$ref") != nil {
		st.reason("duplicate-key") // 占位：$ref 存在时 in-5 已保证 P_in 假
		b, ok := derefChain(doc, raw, map[string]struct{}{})
		if !ok {
			st.lossyUnres = true
			st.reason("unresolved-ref")
		} else if !bytesEq(b, string(raw)) {
			refContrib = b
		}
	}

	// 第 3 步：拆组合关键字（固定序 oneOf → anyOf → allOf）
	type combo struct {
		kind branchKind
		arr  []byte
	}
	var combos []combo
	for _, c := range []struct {
		name string
		kind branchKind
	}{{"oneOf", brOneOf}, {"anyOf", brAnyOf}, {"allOf", brAllOf}} {
		if a := rootRaw(c.name); a != nil {
			if gjson.ParseBytes(a).IsArray() {
				combos = append(combos, combo{kind: c.kind, arr: a})
				st.hasCombo = true
				st.reason("merged-union")
			}
		}
	}

	// 第 4/5 步：收集贡献者
	var contribs []contrib
	if refContrib != nil {
		contribs = append(contribs, contrib{kind: brRoot, raw: refContrib, branch: -2})
	}
	contribs = append(contribs, contrib{kind: brRoot, raw: raw, isRoot: true, branch: -1})
	for ci, cb := range combos {
		present := false
		elemIdx := 0
		scanArray(cb.arr, 0, len(cb.arr), func(elLo, elHi int) bool {
			elem := cb.arr[elLo:elHi]
			branchKey := ci*4096 + elemIdx
			elemIdx++
			obj := gjson.ParseBytes(elem)
			if !obj.IsObject() {
				st.sawNonObj = true
				st.anyNonObj = true // true/false/非对象：该分支对属性没有任何约束
				return true
			}
			if obj.Get("$ref").Type == gjson.String {
				b, ok := derefChain(doc, elem, map[string]struct{}{})
				if !ok {
					st.lossyUnres = true
					st.reason("unresolved-ref")
				} else if !bytesEq(b, string(elem)) {
					contribs = append(contribs, contrib{kind: cb.kind, raw: b, branch: branchKey})
					present = true
				}
			}
			if hasComboKey(obj) {
				sub, _ := flattenMessSchema(doc, elem)
				contribs = append(contribs, contrib{kind: cb.kind, raw: sub, branch: branchKey})
				present = true
				return true
			}
			contribs = append(contribs, contrib{kind: cb.kind, raw: elem, branch: branchKey})
			present = true
			return true
		})
		_ = present
	}

	// 第 6 步：合并
	branchReq := map[int]map[string]struct{}{}
	branchProps := map[int]map[string]struct{}{}
	kinds := map[int]branchKind{}
	reqUnion := map[string]struct{}{}
	reqInter := map[string]struct{}{}
	var interOrder []int
	for _, c := range contribs {
		obj := gjson.ParseBytes(c.raw)
		if !obj.IsObject() {
			continue
		}
		// properties（贡献者内部末个胜）
		declared := map[string]struct{}{}
		for _, e := range propsOf(c.raw) {
			st.addProp(e.name, e.raw)
			declared[e.name] = struct{}{}
		}
		if !c.isRoot && c.kind != brRoot {
			if branchProps[c.branch] == nil {
				branchProps[c.branch] = map[string]struct{}{}
			}
			for n := range declared {
				branchProps[c.branch][n] = struct{}{}
			}
		}
		// required
		req := requiredOf(c.raw)
		if c.isRoot {
			unionInto(reqUnion, req)
		} else if c.kind == brRoot {
			unionInto(reqUnion, req)
		} else {
			if branchReq[c.branch] == nil {
				branchReq[c.branch] = map[string]struct{}{}
				kinds[c.branch] = c.kind
			}
			unionInto(branchReq[c.branch], req)
		}
		// 4c 分类（只对分支贡献者；用 scanRoot 的末个胜键集合）
		if !c.isRoot {
			for _, k := range scanRoot(c.raw).keys {
				switch k.name {
				case "properties", "required":
				default:
					st.classifyBranch(k.name)
				}
			}
		}
	}
	for bi, set := range branchReq {
		if kinds[bi] == brAllOf {
			for n := range set {
				reqUnion[n] = struct{}{}
			}
			continue
		}
		if len(interOrder) == 0 {
			for n := range set {
				reqInter[n] = struct{}{}
			}
		} else {
			for n := range reqInter {
				if _, ok := set[n]; !ok {
					delete(reqInter, n)
				}
			}
		}
		interOrder = append(interOrder, bi)
	}
	for n := range reqInter {
		reqUnion[n] = struct{}{}
	}
	// I1（v18 修）：属性若被某个组合分支"不约束"，合并后必须无约束 ——
	// 否则把 oneOf/anyOf 的析取当成了合取，实例会从合法变非法。
	if st.anyNonObj {
		for _, p := range st.props {
			st.forceEmpty[p.name] = true
		}
	}
	for _, p := range st.props {
		if st.forceEmpty[p.name] {
			continue
		}
		for bi, set := range branchProps {
			// allOf 是**合取**：各分支的约束可以安全并列（不产生收窄）。
			// anyOf/oneOf 是**析取**：某分支不约束该属性 ⇒ 合并后必须无约束。
			if kinds[bi] == brAllOf {
				continue
			}
			if _, ok := set[p.name]; !ok {
				st.forceEmpty[p.name] = true
				break
			}
		}
	}

	// 第 7 步：根 enum / const / not
	for _, k := range []string{"enum", "const", "not"} {
		if rootRaw(k) != nil {
			st.lossyRoot = true
			st.reason("dropped-root-constraint")
		}
	}
	// 第 8 步：type
	if v.typeVal != "object" {
		st.reason("non-object-root")
	}

	// 第 9 步：其余根键（源顺序），$defs/definitions 各自末个胜
	skip := map[string]struct{}{
		"type": {}, "$ref": {}, "properties": {}, "required": {},
		"oneOf": {}, "anyOf": {}, "allOf": {}, "enum": {}, "const": {}, "not": {},
	}
	var kept [][2][]byte // 名字原始字节、值原始字节
	for _, k := range v.keys {
		if _, ok := skip[k.name]; ok {
			continue
		}
		nameRaw := append([]byte(nil), raw[k.keyLo:k.keyHi]...)
		valRaw := append([]byte(nil), raw[k.valLo:k.valHi]...)
		if k.name == "$defs" || k.name == "definitions" {
			valRaw = dedupeDefs(valRaw)
		}
		kept = append(kept, [2][]byte{nameRaw, valRaw})
	}

	// 第 10 步：序列化
	out := make([]byte, 0, len(raw)+64)
	out = append(out, `{"type":"object"`...)
	if len(st.props) > 0 {
		out = append(out, `,"properties":{`...)
		for i := range st.props {
			p := &st.props[i]
			if i > 0 {
				out = append(out, ',')
			}
			out = appendJSONString(out, p.name)
			out = append(out, ':')
			if st.forceEmpty[p.name] {
				out = append(out, `{}`...)
				continue
			}
			if len(p.raws) == 1 {
				out = append(out, p.raws[0]...)
				continue
			}
			out = append(out, `{"anyOf":[`...)
			for j, r := range p.raws {
				if j > 0 {
					out = append(out, ',')
				}
				out = append(out, r...)
			}
			out = append(out, `]}`...)
		}
		out = append(out, '}')
	}
	if len(reqUnion) > 0 {
		names := make([]string, 0, len(reqUnion))
		for n := range reqUnion {
			names = append(names, n)
		}
		sortStrings(names)
		out = append(out, `,"required":[`...)
		for i, n := range names {
			if i > 0 {
				out = append(out, ',')
			}
			out = appendJSONString(out, n)
		}
		out = append(out, ']')
	}
	for _, kv := range kept {
		out = append(out, ',')
		out = append(out, kv[0]...)
		out = append(out, ':')
		out = append(out, kv[1]...)
	}
	out = append(out, '}')
	return out, st
}

// hasComboKey 判定对象是否带根级组合关键字（4b 的递归判据）。
func hasComboKey(obj gjson.Result) bool {
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if obj.Get(k).IsArray() {
			return true
		}
	}
	return false
}

// dedupeDefs 对 $defs/definitions 的对象值做同名末个胜去重（保留源顺序）。
func dedupeDefs(val []byte) []byte {
	type ent struct {
		nameRaw []byte
		valRaw  []byte
	}
	var all []ent
	if !scanObjectPos(val, 0, len(val), func(kr []byte, kLo, kHi, vl, vh int) bool {
		all = append(all, ent{
			nameRaw: append([]byte(nil), val[kLo:kHi]...),
			valRaw:  append([]byte(nil), val[vl:vh]...),
		})
		return true
	}) {
		return val
	}
	seen := map[string]struct{}{}
	keep := make([]bool, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		n := decodeRawKey(all[i].nameRaw)
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		keep[i] = true
	}
	out := []byte{'{'}
	first := true
	for i := range all {
		if !keep[i] {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, all[i].nameRaw...)
		out = append(out, ':')
		out = append(out, all[i].valRaw...)
	}
	if first {
		return []byte(`{}`)
	}
	return append(out, '}')
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// 模式派生与自检
// ---------------------------------------------------------------------------

// schemaModeFor 单向派生：mess 目标走方言层，其余保持既有语义。
// 豁免（若将来出现"mess 目标但需宽松"的上游）只允许来自具名表，且每条豁免
// 必须有 (b) 具名行——本轮无豁免。
func schemaModeFor(dir direction) schemaMode {
	switch dir {
	case dirChatToMess, dirRespToMess:
		return schemaModeMessDialect
	case dirMessToResp:
		return schemaModeObjectOnly
	case dirChatToResp:
		return schemaModeExistence
	}
	return schemaModeUnknown
}

// validateToolsMaps 纯函数自检，由 init 与单测共用。
func validateToolsMaps(maps []*toolsFieldMap) error {
	seen := map[string]struct{}{}
	for _, m := range maps {
		if _, dup := seen[fmtDir(m.dir)]; dup {
			return fmtErrToolsMaps("duplicate dir " + fmtDir(m.dir))
		}
		seen[fmtDir(m.dir)] = struct{}{}
		if m.mode == schemaModeUnknown {
			return fmtErrToolsMaps("unknown mode for " + fmtDir(m.dir))
		}
		if want := schemaModeFor(m.dir); m.mode != want {
			return fmtErrToolsMaps("mode mismatch for " + fmtDir(m.dir))
		}
	}
	return nil
}

func fmtDir(d direction) string {
	if int(d) < len(dirName) {
		return dirName[d]
	}
	return "?"
}

type toolsMapsError string

func (e toolsMapsError) Error() string { return "protoconv: tools maps: " + string(e) }

func fmtErrToolsMaps(msg string) error { return toolsMapsError(msg) }

func init() {
	if err := validateToolsMaps([]*toolsFieldMap{
		&toolsMessFromChat, &toolsMessFromResp, &toolsRespFromMess, &toolsRespFromChat,
	}); err != nil {
		panic(err)
	}
}

// 与 testdata/spec/dialect_anchors.tsv 双向机检的方言常量。
const (
	requireRootType = "object"
	localRefOnly    = true
)

// reasonWhitelist 允许出现在日志/度量里的 reason（(n)① 的反向断言依据）。
var reasonWhitelist = map[string]struct{}{
	"no-schema": {}, "duplicate-key": {}, "unresolved-ref": {},
	"dangling-after-flatten": {}, "merged-union": {},
	"dropped-branch-constraint": {}, "dropped-branch-meta": {}, "dropped-branch-type": {},
	"dropped-root-constraint": {}, "non-object-root": {}, "dropped-forbidden-key": {},
	"required-without-property": {}, "degenerate-lossy": {}, "empty-schema": {},
	"no-object-branch": {},
}
