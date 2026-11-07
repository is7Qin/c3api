package protoconv

// (h) I1 超集性 property test 的**独立最小求值器**。
// 纪律：本文件不得调用 toolschema.go 的任何辅助函数（只允许被测试的 evalMessSchema），
// 一切解析走 encoding/json，指针解析自己实现。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// ---------- 独立求值器 ----------

type evaluator struct {
	root  any
	depth int
}

func newEvaluator(doc any) *evaluator { return &evaluator{root: doc} }

// valid 判定 inst 是否满足 s（2020-12 子集）。遇到不支持的组合关键字由调用方跳过。
func (e *evaluator) valid(s any, inst any) bool {
	switch sc := s.(type) {
	case bool:
		return sc
	case map[string]any:
		return e.validObject(sc, inst)
	}
	return true // 非 schema（如字符串）按注解忽略
}

func (e *evaluator) validObject(s map[string]any, inst any) bool {
	// 深度保险丝：自引用/环引用在 2020-12 里语义未定义，视为满足（源侧保留它，我们也不收窄）。
	if e.depth > 64 {
		return true
	}
	e.depth++
	defer func() { e.depth-- }()
	if ref, ok := s["$ref"].(string); ok {
		target, ok := e.resolve(ref)
		if !ok {
			return false
		}
		if !e.valid(target, inst) {
			return false
		}
	}
	if t, ok := s["type"]; ok && !matchesType(t, inst) {
		return false
	}
	if v, ok := s["enum"]; ok {
		arr, _ := v.([]any)
		found := false
		for _, x := range arr {
			if deepEqual(x, inst) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if v, ok := s["const"]; ok && !deepEqual(v, inst) {
		return false
	}
	if v, ok := s["not"]; ok && e.valid(v, inst) {
		return false
	}
	if v, ok := s["allOf"]; ok {
		arr, _ := v.([]any)
		for _, x := range arr {
			if !e.valid(x, inst) {
				return false
			}
		}
	}
	if v, ok := s["anyOf"]; ok {
		arr, _ := v.([]any)
		hit := false
		for _, x := range arr {
			if e.valid(x, inst) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if v, ok := s["oneOf"]; ok {
		arr, _ := v.([]any)
		n := 0
		for _, x := range arr {
			if e.valid(x, inst) {
				n++
			}
		}
		if n != 1 {
			return false
		}
	}
	if c, ok := s["if"]; ok {
		branch, has := s["then"], true
		if !e.valid(c, inst) {
			branch, has = s["else"], s["else"] != nil
		}
		if has && branch != nil && !e.valid(branch, inst) {
			return false
		}
	}
	if obj, ok := inst.(map[string]any); ok {
		if v, ok := s["required"]; ok {
			arr, _ := v.([]any)
			for _, x := range arr {
				name, _ := x.(string)
				if _, has := obj[name]; !has {
					return false
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		pats, _ := s["patternProperties"].(map[string]any)
		if v, ok := s["minProperties"]; ok && len(obj) < toInt(v) {
			return false
		}
		if v, ok := s["maxProperties"]; ok && len(obj) > toInt(v) {
			return false
		}
		if v, ok := s["dependentRequired"]; ok {
			if m, ok := v.(map[string]any); ok {
				for k, need := range m {
					if _, has := obj[k]; !has {
						continue
					}
					arr, _ := need.([]any)
					for _, x := range arr {
						if _, has := obj[x.(string)]; !has {
							return false
						}
					}
				}
			}
		}
		if v, ok := s["dependentSchemas"]; ok {
			if m, ok := v.(map[string]any); ok {
				for k, sub := range m {
					if _, has := obj[k]; has && !e.valid(sub, inst) {
						return false
					}
				}
			}
		}
		for name, val := range obj {
			if sub, ok := props[name]; ok && !e.valid(sub, val) {
				return false
			}
			matched := false
			for pat, sub := range pats {
				re, err := regexp.Compile(pat)
				if err == nil && re.MatchString(name) {
					matched = true
					if !e.valid(sub, val) {
						return false
					}
				}
			}
			if _, inProps := props[name]; !inProps && !matched {
				if ap, ok := s["additionalProperties"]; ok && !e.valid(ap, val) {
					return false
				}
			}
		}
	}
	if arr, ok := inst.([]any); ok {
		if v, ok := s["prefixItems"]; ok {
			subs, _ := v.([]any)
			for i, sub := range subs {
				if i < len(arr) && !e.valid(sub, arr[i]) {
					return false
				}
			}
		}
		start := 0
		if v, ok := s["prefixItems"]; ok {
			if subs, ok := v.([]any); ok {
				start = len(subs)
			}
		}
		if v, ok := s["items"]; ok {
			if subs, ok := v.([]any); ok { // draft-07 元组
				for i, sub := range subs {
					if i < len(arr) && !e.valid(sub, arr[i]) {
						return false
					}
				}
			} else {
				for i := start; i < len(arr); i++ {
					if !e.valid(v, arr[i]) {
						return false
					}
				}
			}
		}
		if v, ok := s["contains"]; ok {
			n := 0
			for _, x := range arr {
				if e.valid(v, x) {
					n++
				}
			}
			min := 1
			if v, ok := s["minContains"]; ok {
				min = toInt(v)
			}
			if n < min {
				return false
			}
			if v, ok := s["maxContains"]; ok && n > toInt(v) {
				return false
			}
		}
		if v, ok := s["minItems"]; ok && len(arr) < toInt(v) {
			return false
		}
		if v, ok := s["maxItems"]; ok && len(arr) > toInt(v) {
			return false
		}
	}
	if str, ok := inst.(string); ok {
		if v, ok := s["minLength"]; ok && len([]rune(str)) < toInt(v) {
			return false
		}
		if v, ok := s["maxLength"]; ok && len([]rune(str)) > toInt(v) {
			return false
		}
		if v, ok := s["pattern"].(string); ok {
			re, err := regexp.Compile(v)
			if err == nil && !re.MatchString(str) {
				return false
			}
		}
	}
	if num, ok := toFloat(inst); ok {
		if v, ok := s["minimum"]; ok && num < toFloatOr(v) {
			return false
		}
		if v, ok := s["maximum"]; ok && num > toFloatOr(v) {
			return false
		}
		if v, ok := s["exclusiveMinimum"]; ok && num <= toFloatOr(v) {
			return false
		}
		if v, ok := s["exclusiveMaximum"]; ok && num >= toFloatOr(v) {
			return false
		}
	}
	return true
}

// resolve 本地 Pointer（自己实现，不复用生产 resolve）。
func (e *evaluator) resolve(ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	cur := e.root
	for _, seg := range strings.Split(ref[2:], "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, ok := parseIdx(seg)
			if !ok || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func parseIdx(s string) (int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
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

func matchesType(t any, inst any) bool {
	switch tv := t.(type) {
	case string:
		return oneType(tv, inst)
	case []any:
		for _, x := range tv {
			if s, ok := x.(string); ok && oneType(s, inst) {
				return true
			}
		}
		return false
	}
	return true
}

func oneType(name string, inst any) bool {
	switch name {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "number":
		_, ok := toFloat(inst)
		return ok
	case "integer":
		f, ok := toFloat(inst)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "null":
		return inst == nil
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func toFloatOr(v any) float64 {
	f, _ := toFloat(v)
	return f
}

func toInt(v any) int { f, _ := toFloat(v); return int(f) }

func deepEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// ---------- (h) 超集性 property test ----------

// I1：对对象实例 x，若 x 在 S 下合法，则 x 在 T(S) 下必须仍合法。
func TestI1Superset(t *testing.T) {
	valueCats := []any{
		nil, true, false, float64(0), float64(1), float64(-1), 1.5,
		"", "a", "b", []any{}, []any{float64(0)}, map[string]any{}, map[string]any{"k": float64(1)},
	}
	for _, r := range loadSchemaCases(t) {
		src := gjson.Parse(r.src)
		// 只对"真进 T"且不兜底的用例做超集性检查；兜底产物是全集的，平凡成立。
		out, deg := messSchema(r.src)
		if deg == degBail {
			continue
		}
		var srcDoc, outDoc any
		if err := json.Unmarshal([]byte(r.src), &srcDoc); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(out), &outDoc); err != nil {
			t.Fatalf("%s: 产物不是合法 JSON: %v", r.name, err)
		}
		_ = src
		evS := newEvaluator(srcDoc)
		evT := newEvaluator(outDoc)
		names := map[string]struct{}{"__x": {}}
		gjson.Parse(r.src).Get("properties").ForEach(func(k, _ gjson.Result) bool {
			names[k.Str] = struct{}{}
			return true
		})
		gjson.Parse(out).Get("properties").ForEach(func(k, _ gjson.Result) bool {
			names[k.Str] = struct{}{}
			return true
		})
		var nameList []string
		for n := range names {
			nameList = append(nameList, n)
		}
		// 0 键实例（spec 的 required/分支语义最常在这里出错）
		empty := map[string]any{}
		if evS.valid(srcDoc, empty) && !evT.valid(outDoc, empty) {
			t.Fatalf("%s: I1 违反（空实例）—— 源=%s\n产物=%s", r.name, r.src, out)
		}
		// 键集合的所有子集 × 值类别（单键赋值，覆盖度足够发现收窄）
		for _, n := range nameList {
			for _, v := range valueCats {
				inst := map[string]any{n: v}
				if !evS.valid(srcDoc, inst) {
					continue
				}
				if !evT.valid(outDoc, inst) {
					t.Fatalf("%s: I1 违反 —— 实例 %v 在源下合法、在产物下非法\n源=%s\n产物=%s",
						r.name, inst, r.src, out)
				}
			}
		}
		// 两键同值（覆盖 required 组合）
		for i := 0; i < len(nameList); i++ {
			for j := 0; j < len(nameList); j++ {
				inst := map[string]any{nameList[i]: "a", nameList[j]: float64(1)}
				if !evS.valid(srcDoc, inst) || evT.valid(outDoc, inst) {
					continue
				}
				t.Fatalf("%s: I1 违反（双键）—— 实例 %v\n源=%s\n产物=%s", r.name, inst, r.src, out)
			}
		}
	}
}

var _ = fmt.Sprintf
