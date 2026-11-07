package protoconv

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// (r-A) R_req 的**独立**归纳定义：从根出发，沿"根链 / allOf / anyOf / oneOf"收集 $ref。
// - 带 `$ref` 的对象：把该 ptr 加入集合，并**沿链逐跳**继续加入，链终点继续 R_req(终点)；
// - 对 oneOf/anyOf/allOf 的每个元素 e：R_req(e)，若 e 带 `$ref` 还要对其**解引用结果**递归；
// - **不进入** properties/required/$defs 的值位置。
type reqRef struct {
	ptr string
	src string
}

func collectRReq(ev *evaluator, node any, src string, out *[]reqRef, depth int) {
	if depth > 16 {
		return
	}
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		cur := ref
		seen := map[string]struct{}{}
		for hop := 0; hop < 32; hop++ {
			if _, dup := seen[cur]; dup {
				break
			}
			seen[cur] = struct{}{}
			*out = append(*out, reqRef{ptr: cur, src: src})
			next, ok := ev.derefOne(cur)
			if !ok {
				break
			}
			cur = next
		}
		if target, ok := ev.resolve(ref); ok {
			collectRReq(ev, target, src, out, depth+1)
		}
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		arr, ok := m[k].([]any)
		if !ok {
			continue
		}
		for _, e := range arr {
			collectRReq(ev, e, k, out, depth+1)
			if em, ok := e.(map[string]any); ok {
				if r, ok := em["$ref"].(string); ok {
					if target, ok := ev.resolve(r); ok {
						collectRReq(ev, target, k, out, depth+1)
					}
				}
			}
		}
	}
}

// derefOne 若 ptr 目标是对象且带字符串 $ref，返回链的下一跳。
func (e *evaluator) derefOne(ptr string) (string, bool) {
	t, ok := e.resolve(ptr)
	if !ok {
		return "", false
	}
	m, ok := t.(map[string]any)
	if !ok {
		return "", false
	}
	next, ok := m["$ref"].(string)
	return next, ok
}

// (r-B) 产物侧三项断言。判据全部独立实现（不复用生产侧的 refOK/collectTables）。
func TestRReqCoverage(t *testing.T) {
	refCount, propCount, reqCount := 0, 0, 0
	for _, r := range loadSchemaCases(t) {
		var srcDoc any
		if err := json.Unmarshal([]byte(r.src), &srcDoc); err != nil {
			continue // 非 JSON 源（如缺失 schema 的 ""）由 (b) 的字节断言覆盖
		}
		out, deg := messSchema(r.src)
		if deg == degBail {
			// (r-B)③ 归一化：兜底时两侧都是 {"type":"object"}，由 (b′) 金格锁定。
			continue
		}
		var outDoc any
		if err := json.Unmarshal([]byte(out), &outDoc); err != nil {
			t.Fatalf("%s: 产物不是合法 JSON: %v", r.name, err)
		}
		var refs []reqRef
		collectRReq(newEvaluator(srcDoc), srcDoc, "root-chain", &refs, 0)
		ev := newEvaluator(srcDoc)
		outRoot, _ := outDoc.(map[string]any)
		outProps, _ := outRoot["properties"].(map[string]any)
		multiGroup := len(comboGroups(srcDoc)) > 1
		for _, ref := range refs {
			refCount++
			target, ok := ev.resolve(ref.ptr)
			if !ok {
				continue // 源侧本就解不出的 ptr：由 (b) 的 root_ref_missing 行覆盖
			}
			tm, ok := target.(map[string]any)
			if !ok {
				continue
			}
			// ① 逐属性字节归属
			if props, ok := tm["properties"].(map[string]any); ok {
				for name, srcVal := range props {
					propCount++
					prodVal, has := outProps[name]
					require.True(t, has, "%s: R_req 的 %s 在产物里丢了属性 %q（来源 %s）\n产物=%s",
						r.name, ref.ptr, name, ref.src, out)
					if m, ok := prodVal.(map[string]any); ok && len(m) == 0 {
						continue // v19/v20 的加宽：{}
					}
					if deepEqual(prodVal, srcVal) {
						continue
					}
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
			// ② 按来源分支的 required 断言
			if multiGroup {
				continue // 多组并存时产物 required 是各组交集的并，单组断言不适用（v21 登记）
			}
			reqCount++
			srcReq := toStringSet(tm["required"])
			outReq := toStringSet(outRoot["required"])
			switch ref.src {
			case "root-chain", "allOf":
				for n := range srcReq {
					require.Contains(t, outReq, n,
						"%s: %s 来源的 required %q 必须出现在产物里（%s）", r.name, ref.src, n, out)
				}
			default: // anyOf / oneOf：产物 required 只能是该分支的子集（或为空）
				for n := range outReq {
					require.Contains(t, srcReq, n,
						"%s: %s 来源要求产物 required ⊆ 分支 required，但 %q 不在（%s）", r.name, ref.src, n, out)
				}
			}
		}
	}
	// 防恒绿（C-3）
	require.Greater(t, refCount, 10, "(r-A) 必须真的扫到引用，否则门禁空过")
	require.Greater(t, propCount, 10, "(r-B)① 必须真的比对过属性，否则门禁空过")
	require.Greater(t, reqCount, 10, "(r-B)② 必须真的比对过 required，否则门禁空过")
}

func toStringSet(v any) map[string]struct{} {
	out := map[string]struct{}{}
	arr, _ := v.([]any)
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out[s] = struct{}{}
		}
	}
	return out
}

// comboGroups 源文档根级组合关键字的分组（独立实现）。
func comboGroups(doc any) []string {
	m, _ := doc.(map[string]any)
	var out []string
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := m[k].([]any); ok {
			out = append(out, k)
		}
	}
	return out
}
