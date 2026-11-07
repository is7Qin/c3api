package protoconv

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// (r-A) R_req：从根出发，只沿"根链 / allOf / anyOf / oneOf"收集 $ref（不进入
// properties/required/$defs 的值位置），并把每个 ptr 的来源标记下来。
type reqRef struct{ ptr, src string }

func collectRReq(node any, src string, out *[]reqRef, depth int) {
	if depth > 16 {
		return
	}
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		*out = append(*out, reqRef{ptr: ref, src: src})
		return
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		arr, ok := m[k].([]any)
		if !ok {
			continue
		}
		for _, e := range arr {
			collectRReq(e, k, out, depth+1)
		}
	}
}

// (r-B)① 产物侧逐属性字节归属：R_req 里每个 ptr 解引用后若带 properties，
// 每个属性名都必须在产物里出现；产物值要么是 {}（v19 的加宽），要么与源值逐字节
// 相等，要么是含该字节的 anyOf 成员。
func TestRReqCoverage(t *testing.T) {
	refCount, propCount := 0, 0
	for _, r := range loadSchemaCases(t) {
		out, deg := messSchema(r.src)
		if deg == degBail {
			continue // 兜底：产物是 {"type":"object"}，由 (b′) 单独覆盖
		}
		var srcDoc, outDoc any
		if err := json.Unmarshal([]byte(r.src), &srcDoc); err != nil {
			t.Fatalf("%s: 源不是合法 JSON: %v", r.name, err)
		}
		if err := json.Unmarshal([]byte(out), &outDoc); err != nil {
			t.Fatalf("%s: 产物不是合法 JSON: %v", r.name, err)
		}
		var refs []reqRef
		collectRReq(srcDoc, "root-chain", &refs, 0)
		ev := newEvaluator(srcDoc)
		outProps, _ := outDoc.(map[string]any)["properties"].(map[string]any)
		for _, ref := range refs {
			refCount++
			target, ok := ev.resolve(ref.ptr)
			if !ok {
				continue // 源侧本就解不出的 ptr 由 L1/(b) 覆盖
			}
			props, ok := target.(map[string]any)["properties"].(map[string]any)
			if !ok {
				continue
			}
			for name, srcVal := range props {
				prodVal, has := outProps[name]
				propCount++
				require.True(t, has, "%s: R_req 的 %s 在产物里丢了属性 %q（来源 %s）\n产物=%s",
					r.name, ref.ptr, name, ref.src, out)
				if m, ok := prodVal.(map[string]any); ok && len(m) == 0 {
					continue // v19 的加宽：{}
				}
				if deepEqual(prodVal, srcVal) {
					continue
				}
				// anyOf 成员命中
				if m, ok := prodVal.(map[string]any); ok {
					if arr, ok := m["anyOf"].([]any); ok {
						hit := false
						for _, x := range arr {
							if deepEqual(x, srcVal) {
								hit = true
								break
							}
						}
						require.True(t, hit, "%s: 属性 %q 的 anyOf 里没有源字节（来源 %s）", r.name, name, ref.src)
						continue
					}
				}
				t.Fatalf("%s: 属性 %q 的产物值既不是 {}、也不等于源、也不是 anyOf 成员\n源=%v\n产物=%v",
					r.name, name, srcVal, prodVal)
			}
		}
	}
}
