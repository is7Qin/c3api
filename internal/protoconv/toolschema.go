package protoconv

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// schemaMode 工具参数 schema 的处理模式。零值必须是 Unknown（fail-fast），
// 由 evalSchemaField 的 default: panic 与 validateToolsMaps 双向兜底。
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
	maxValveDepth       = 256
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
// （先 ~1 后 ~0，故 "~01" 得到字面量 "~1"）；不做 URI percent 解码；
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
	parts := strings.Split(ptr[2:], "/") // 保留尾部空段：`#/a/` 与 `#/a` 是不同指针
	lo, hi := 0, len(d.raw)
	for _, part := range parts {
		name := pointerUnescape(part)
		var ok bool
		lo, hi, ok = stepInto(d.raw, lo, hi, name)
		if !ok {
			return 0, 0, false
		}
	}
	return lo, hi, true
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

// rootKey 一个根成员：解码名 + 名字/值区间（同名组只保留末个）。
type rootKey struct {
	name         string
	keyLo, keyHi int
	valLo, valHi int
}

// verdict 根级判定结果。
type verdict struct {
	isObject  bool
	keys      []rootKey // 源顺序，只保留同名组的末个
	typeVal   string    // 解码后的根 type（非字符串为空）
	forbidden []string  // 出现的禁键（解码名，去重）
	dupCount  int
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
	var walk func(lo, hi, depth int)
	walk = func(lo, hi, depth int) {
		if !ok || lo < 0 || hi > len(b) || lo >= hi {
			return
		}
		// 深度上限：递归深度 = 输入嵌套深度。Go 的栈可增长，但遍历成本是
		// O(深度 × 字节)，不设限会被深嵌套输入拖死；超限即"不能证明可解析"⇒ 兜底。
		// 256 覆盖真实 schema（工具参数一般 < 30 层），同时给开销一个硬上界。
		if depth > maxValveDepth {
			ok = false
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
				walk(vl, vh, depth+1)
				return ok
			})
		case '[':
			scanArray(b, lo, hi, func(vl, vh int) bool {
				walk(vl, vh, depth+1)
				return ok
			})
		}
	}
	walk(0, len(b), 0)
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
	props        []propEnt
	propIndex    map[string]int
	forceEmpty   map[string]bool // 某析取组不约束该属性 ⇒ 合并后必须无约束（否则收窄）
	lossyUnres   bool            // (L1) 读不到
	lossyDrop    bool            // (L2) 丢了分支约束
	lossyRoot    bool            // (L3) 删了根约束
	hasCombo     bool
	groupNonObj  map[int]bool // 该析取组里有"什么都不约束"的分支（非对象字面量）
	allNonObj    bool         // 全部组合分支都"非 object"（spec 第 11 步的判据）
	anyBranch    bool         // 至少有一个组合分支
	branch0Empty bool         // branch₀（根）既无 properties 也无 required
	reasons      []string
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

// refString 取对象里 $ref 的**末个**值（末个胜，不用 gjson.Get 的首胜）。
func refString(obj []byte) (string, bool) {
	sp, ok := fieldSpan(obj, "$ref")
	if !ok || sp.lo >= len(obj) || obj[sp.lo] != '"' {
		return "", false
	}
	r := gjson.ParseBytes(obj[sp.lo:sp.hi])
	if r.Type != gjson.String {
		return "", false
	}
	return r.Str, true
}

// derefChain 沿本地 $ref 链走到底。返回 (raw, ok)；ok=false 表示"读不到"。
func derefChain(doc *schemaDoc, raw []byte, visiting map[string]struct{}) ([]byte, bool) {
	cur := raw
	for hop := 0; hop < maxRefHops; hop++ {
		ptr, ok := refString(cur)
		if !ok {
			return cur, true
		}
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

// evalResult 一次 mess 方言求值的结果（spec §3.5 的观测契约）。
type evalResult struct {
	raw     string // P_in 成立时的原字节视图（与 out 恰有一个非空）
	out     []byte
	deg     degenerateKind
	reasons []string
	missing bool // 源侧没有可用 schema（无参工具 / null / 非对象值）
}

var emptyObjectSchema = []byte(`{"type":"object"}`)

// ---------------------------------------------------------------------------
// 日志与度量接线（spec §3.5）
// ---------------------------------------------------------------------------

// SchemaEvent 一次 mess 方言求值的观测事件；字段是 §3.5 的常驻集合。
type SchemaEvent struct {
	Direction   string   // 方向名（dirChatToMess / dirRespToMess）
	Tool        string   // 工具名；源工具无 name 时为空
	Reasons     []string // 动作级 reason 集合
	Degenerate  string   // none / simplify / bail
	SchemaBytes int      // 源 schema 字节数（缺失记 0）
	RootKeys    int      // 源根键数（只在 WARN 路径序列化）
	Missing     bool     // 源侧没有 schema
}

var (
	schemaMu    sync.RWMutex
	schemaSink  func(SchemaEvent)
	schemaStats sync.Map // reason → *atomic.Int64
)

// SetSchemaSink 注入观测出口（cmd/server 启动时接到 logx；不注入 = 不记日志）。
// 计数始终记录，与 sink 是否存在无关。
func SetSchemaSink(fn func(SchemaEvent)) {
	schemaMu.Lock()
	schemaSink = fn
	schemaMu.Unlock()
}

func schemaRepairedAdd(reason string) {
	v, ok := schemaStats.Load(reason)
	if !ok {
		c := new(atomic.Int64)
		actual, loaded := schemaStats.LoadOrStore(reason, c)
		if loaded {
			c = actual.(*atomic.Int64)
		}
		v = c
	}
	v.(*atomic.Int64).Add(1)
}

// SchemaRepaired 读单个 reason 的累计次数（健康检查与 (n)⑥ 门禁用）。
func SchemaRepaired(reason string) int64 {
	if v, ok := schemaStats.Load(reason); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// SchemaRepairedCounts 全部 reason 的计数快照。
func SchemaRepairedCounts() map[string]int64 {
	out := map[string]int64{}
	schemaStats.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// resetSchemaRepairedCounts 清空计数（仅同包测试用；生产不调用）。
func resetSchemaRepairedCounts() {
	schemaStats.Range(func(k, _ any) bool {
		schemaStats.Delete(k)
		return true
	})
}

func degName(d degenerateKind) string {
	switch d {
	case degSimplify:
		return "simplify"
	case degBail:
		return "bail"
	}
	return "none"
}

// observeSchema 把一次求值结果送出（计数已在 evalMessSchema 里记过）。
// 只在"真的改了字节"的路径上调用 ⇒ P_in 命中的热路径零分配。
func observeSchema(m toolsFieldMap, src, in gjson.Result, res evalResult) {
	schemaMu.RLock()
	sink := schemaSink
	schemaMu.RUnlock()
	if sink == nil {
		return
	}
	ev := SchemaEvent{
		Direction:   fmtDir(m.dir),
		Reasons:     res.reasons,
		Degenerate:  degName(res.deg),
		SchemaBytes: len(in.Raw),
		Missing:     res.missing,
	}
	if name := src.Get("name"); name.Type == gjson.String {
		ev.Tool = name.Str
	}
	if res.deg != degNone { // root_keys 只在 WARN 路径序列化（免热路径开销）
		ev.RootKeys = len(scanRoot([]byte(in.Raw)).keys)
	}
	sink(ev)
}

// evalMessSchema 跑 P_in → T（1~9）→ 9b 安全阀 → 退化判定，并记录 reason 计数。
func evalMessSchema(src gjson.Result) evalResult {
	res := evalMessSchemaInner(src)
	if len(res.reasons) > 0 {
		for _, r := range res.reasons {
			schemaRepairedAdd(r)
		}
	}
	return res
}

func evalMessSchemaInner(src gjson.Result) evalResult {
	if src.Raw == "" || src.Type == gjson.Null {
		// 第 0 步：源侧根本没有 schema（无参工具最常见）。这不是退化 ——
		// 没有任何约束被丢掉 ⇒ degNone，不加 hint（momus-r2 I-4）。
		return evalResult{out: emptyObjectSchema, deg: degNone, reasons: []string{"no-schema"}, missing: true}
	}
	if !src.IsObject() {
		// 第 0 步：字符串/数组/数字/布尔一律不解析其内容（否则字符串里的 $defs
		// 会成为第二个文档基准）。
		return evalResult{out: emptyObjectSchema, deg: degNone, reasons: []string{"no-schema"}}
	}
	if pInFast(src) {
		return evalResult{raw: src.Raw, deg: degNone}
	}
	raw := []byte(src.Raw)
	v := scanRoot(raw)
	if v.pIn(raw) {
		return evalResult{raw: src.Raw, deg: degNone}
	}
	doc := newSchemaDoc(raw)
	out, st := flattenMessSchema(doc, raw)
	if st == nil {
		return evalResult{out: emptyObjectSchema, deg: degNone, reasons: []string{"no-schema"}}
	}
	// 9b 引用安全阀：产物里每个引用都必须能证明可解析，否则整文档兜底。
	if !valveOK(out) {
		st.reason("dangling-after-flatten")
		return evalResult{out: emptyObjectSchema, deg: degBail, reasons: st.reasons}
	}
	switch {
	case st.lossyUnres || st.lossyDrop || st.lossyRoot:
		st.reason("degenerate-lossy")
		return evalResult{out: out, deg: degSimplify, reasons: st.reasons}
	case st.hasCombo && st.allNonObj && st.branch0Empty && len(st.props) == 0:
		st.reason("no-object-branch")
		return evalResult{out: out, deg: degSimplify, reasons: st.reasons}
	case !st.hasCombo && len(st.props) == 0 && len(propsOf(raw)) == 0 && !st.lossyUnres:
		// 安全：源本来就没有约束（三互斥的第三个兄弟，不算退化、不发 hint）。
		st.reason("empty-schema")
	}
	return evalResult{out: out, deg: degNone, reasons: st.reasons}
}

// contrib 一个贡献者（分支或根）。
type contrib struct {
	kind   branchKind
	raw    []byte
	isRoot bool // branch₀：其余根键由第 9 步保留，不进 4c 分类
	branch int  // 组合键分支序号；brRoot 用 -1/-2
}

// fieldSpan 取对象里某个键的**末个**取值区间（末个胜，不用 gjson.Get）。
// 只扫一遍、不建键表 ⇒ 无分配。
func fieldSpan(obj []byte, name string) (span, bool) {
	var lo, hi int
	found := false
	if !scanObjectPos(obj, 0, len(obj), func(keyRaw []byte, _, _ int, vl, vh int) bool {
		if rawKeyEq(keyRaw, name) {
			lo, hi, found = vl, vh, true
		}
		return true
	}) {
		return span{}, false
	}
	if !found {
		return span{}, false
	}
	return span{lo, hi}, true
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
	st := &flattenState{propIndex: map[string]int{}, forceEmpty: map[string]bool{}, groupNonObj: map[int]bool{}}
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

	// 分支级累计（key = 分支序号）
	branchReq := map[int]map[string]struct{}{}
	branchProps := map[int]map[string]struct{}{}
	branchKinds := map[int]branchKind{}
	branchIsObj := map[int]bool{}

	// 第 4/5 步：收集贡献者
	var contribs []contrib
	if refContrib != nil {
		contribs = append(contribs, contrib{kind: brRoot, raw: flattenNested(doc, refContrib, st), branch: -2})
	}
	contribs = append(contribs, contrib{kind: brRoot, raw: raw, isRoot: true, branch: -1})
	for ci, cb := range combos {
		elemIdx := 0
		scanArray(cb.arr, 0, len(cb.arr), func(elLo, elHi int) bool {
			elem := cb.arr[elLo:elHi]
			branchKey := ci*4096 + elemIdx
			elemIdx++
			st.anyBranch = true
			branchKinds[branchKey] = cb.kind
			if !gjson.ParseBytes(elem).IsObject() {
				// `true`/`false`/数字/字符串/数组分支：什么都不约束。
				// 析取（anyOf/oneOf）组里出现这类分支 ⇒ 该组不再要求别处的键；
				// allOf 是合取 ⇒ 不受影响（momus-r2 I-5）。
				if cb.kind != brAllOf {
					st.groupNonObj[ci] = true
				}
				return true
			}
			// 候选贡献者：分支自身 + 4a 的解引用结果（v21：两者都要判 4b）
			cands := [][]byte{elem}
			if _, isRef := refString(elem); isRef {
				b, ok := derefChain(doc, elem, map[string]struct{}{})
				if !ok {
					st.lossyUnres = true
					st.reason("unresolved-ref")
				} else if !bytesEq(b, string(elem)) {
					cands = append(cands, b)
				}
			}
			isObj := false
			for _, c := range cands {
				if isObjectSchema(c) {
					isObj = true
				}
			}
			branchIsObj[branchKey] = isObj
			for _, c := range cands {
				contribs = append(contribs, contrib{kind: cb.kind, raw: flattenNested(doc, c, st), branch: branchKey})
			}
			return true
		})
	}

	// spec 第 11 步：全部分支"非 object"？
	if st.anyBranch {
		st.allNonObj = true
		for _, isObj := range branchIsObj {
			if isObj {
				st.allNonObj = false
				break
			}
		}
	}
	st.branch0Empty = len(propsOf(raw)) == 0 && len(requiredOf(raw)) == 0

	// 第 6 步：合并
	reqUnion := map[string]struct{}{}
	for _, c := range contribs {
		if !gjson.ParseBytes(c.raw).IsObject() {
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
		// required：branch₋₁/根/allOf 取并，anyOf/oneOf 按组取交
		req := requiredOf(c.raw)
		if c.isRoot || c.kind == brRoot {
			unionInto(reqUnion, req)
		} else {
			if branchReq[c.branch] == nil {
				branchReq[c.branch] = map[string]struct{}{}
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

	// required 交集：**每组各算一个**再并入（momus-r2 I-6）；
	// 组里有"什么都不约束"的分支 ⇒ 该组交集为空（v20 ④）。
	interByGroup := map[int]map[string]struct{}{}
	for bi, set := range branchReq {
		if branchKinds[bi] == brAllOf { // 合取：直接并入
			for n := range set {
				reqUnion[n] = struct{}{}
			}
			continue
		}
		ci := bi / 4096
		if st.groupNonObj[ci] {
			continue
		}
		g, ok := interByGroup[ci]
		if !ok {
			g = map[string]struct{}{}
			for n := range set {
				g[n] = struct{}{}
			}
			interByGroup[ci] = g
			continue
		}
		for n := range g {
			if _, ok := set[n]; !ok {
				delete(g, n)
			}
		}
	}
	for _, g := range interByGroup {
		for n := range g {
			reqUnion[n] = struct{}{}
		}
	}

	// I1：属性若被某个**析取组**的分支"不约束"，合并后必须无约束 ——
	// 否则把 oneOf/anyOf 的析取当成了合取，实例会从合法变非法。
	for bi, set := range branchProps {
		if branchKinds[bi] == brAllOf {
			continue
		}
		if st.groupNonObj[bi/4096] {
			for n := range set {
				st.forceEmpty[n] = true
			}
		}
	}
	for _, p := range st.props {
		if st.forceEmpty[p.name] {
			continue
		}
		for bi, set := range branchProps {
			// allOf 是**合取**：各分支的约束可以安全并列（不产生收窄）。
			// anyOf/oneOf 是**析取**：某分支不约束该属性 ⇒ 合并后必须无约束。
			if branchKinds[bi] == brAllOf {
				continue
			}
			if _, ok := set[p.name]; !ok {
				st.forceEmpty[p.name] = true
				break
			}
		}
	}

	// 第 6 步补：required 里未在 properties 定义的键保留 + 计数（L12）
	if len(reqUnion) > 0 {
		declared := make(map[string]struct{}, len(st.props))
		for _, p := range st.props {
			declared[p.name] = struct{}{}
		}
		for n := range reqUnion {
			if _, ok := declared[n]; !ok {
				st.reason("required-without-property")
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
			if isForbiddenRootKey(k.name) {
				st.reason("dropped-forbidden-key")
			}
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

// hasComboKey 4b 的递归判据：根带组合关键字（末个胜取值）。
func hasComboKey(raw []byte) bool {
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if sp, ok := fieldSpan(raw, k); ok && sp.lo < len(raw) && raw[sp.lo] == '[' {
			return true
		}
	}
	return false
}

// flattenNested 4b：贡献者根带组合关键字 ⇒ 递归压平，内层 state 并回外层
// （内层丢掉的约束必须让 hint 与度量看见）。
func flattenNested(doc *schemaDoc, raw []byte, st *flattenState) []byte {
	if !hasComboKey(raw) {
		return raw
	}
	sub, sst := flattenMessSchema(doc, raw)
	if sst != nil {
		st.lossyUnres = st.lossyUnres || sst.lossyUnres
		st.lossyDrop = st.lossyDrop || sst.lossyDrop
		st.lossyRoot = st.lossyRoot || sst.lossyRoot
		for _, r := range sst.reasons {
			st.reason(r)
		}
	}
	return sub
}

// isForbiddenRootKey 判定解码后的键名是否在根级禁键集里。
func isForbiddenRootKey(name string) bool {
	for _, f := range forbiddenRootKeys {
		if name == f {
			return true
		}
	}
	return false
}

// rawIsStrEq v 是否是恰好等于 want 的 JSON 字符串字面量（无转义、无分配）。
func rawIsStrEq(v []byte, want string) bool {
	if len(v) != len(want)+2 || v[0] != '"' || v[len(v)-1] != '"' {
		return false
	}
	for i := 0; i < len(want); i++ {
		if v[i+1] != want[i] {
			return false
		}
	}
	return true
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
	defsTables      = "$defs,definitions"
)

// reasonOrder 白名单的权威顺序：(e)① 的 logReasons 与 (n)① 的反向断言都读它。
var reasonOrder = []string{
	"no-schema", "duplicate-key", "unresolved-ref", "dangling-after-flatten",
	"merged-union", "dropped-branch-constraint", "dropped-branch-meta", "dropped-branch-type",
	"dropped-root-constraint", "non-object-root", "dropped-forbidden-key",
	"required-without-property", "degenerate-lossy", "empty-schema", "no-object-branch",
}

// reasonWhitelist 允许出现在日志/度量里的 reason（(n)① 的反向断言依据）。
var reasonWhitelist = func() map[string]struct{} {
	m := make(map[string]struct{}, len(reasonOrder))
	for _, r := range reasonOrder {
		m[r] = struct{}{}
	}
	return m
}()

// pInFast 源侧后置条件的快路径：gjson 视图遍历 + 键名按**原始字节**比较，
// 不建键表、不分配。只有它返回 false 才走 scanRoot 的完整判定；
// (i) 的 TestPInFastParity 断言两者逐输入一致。
func pInFast(src gjson.Result) bool {
	if !src.IsObject() {
		return false
	}
	var seen [24]string // 已见键的原始 JSON 文本（含引号；根键很少，固定数组免分配）
	n := 0
	ok := true
	sawType := false
	src.ForEach(func(k, v gjson.Result) bool {
		rawKey := k.Raw
		for i := 0; i < n; i++ {
			if keyRawEqual(seen[i], rawKey) {
				ok = false // 重复根键 ⇒ 交给完整判定
				return false
			}
		}
		if n == len(seen) {
			ok = false // 根键过多 ⇒ 交给完整判定
			return false
		}
		seen[n] = rawKey
		n++
		for _, f := range forbiddenRootKeys {
			if keyIsRaw(rawKey, f) {
				ok = false
				return false
			}
		}
		if keyIsRaw(rawKey, "$ref") {
			ok = false
			return false
		}
		if keyIsRaw(rawKey, "type") {
			sawType = true
			if v.Type != gjson.String || v.Str != requireRootType {
				ok = false
				return false
			}
		}
		return true
	})
	return ok && sawType
}

// keyIsRaw 原始键文本（含引号）的解码名是否等于 name。无转义时逐字节，
// 有转义时才走 gjson 解码（罕见路径允许分配）。
func keyIsRaw(rawKey, name string) bool {
	if rawStrEq(rawKey, name) {
		return true
	}
	if strings.IndexByte(rawKey, '\\') >= 0 {
		return gjson.Parse(rawKey).Str == name
	}
	return false
}

// keyRawEqual 两个原始键的解码名是否相等。
func keyRawEqual(a, b string) bool {
	if a == b {
		return true
	}
	if strings.IndexByte(a, '\\') >= 0 || strings.IndexByte(b, '\\') >= 0 {
		return gjson.Parse(a).Str == gjson.Parse(b).Str
	}
	return false
}

// isObjectSchema spec 第 11 步的「object 分支」判据（4c 前、解引用后）：
// 带 properties，或根 type 解码后等于 "object"（含 ["object", …] 联合）。
func isObjectSchema(raw []byte) bool {
	if sp, ok := fieldSpan(raw, "properties"); ok && sp.lo < len(raw) && raw[sp.lo] == '{' {
		return true
	}
	sp, ok := fieldSpan(raw, "type")
	if !ok || sp.lo >= len(raw) {
		return false
	}
	v := raw[sp.lo:sp.hi]
	if rawIsStrEq(v, "object") {
		return true
	}
	if v[0] == '[' {
		hit := false
		scanArray(v, 0, len(v), func(vl, vh int) bool {
			if rawIsStrEq(v[vl:vh], "object") {
				hit = true
				return false
			}
			return true
		})
		return hit
	}
	return false
}
