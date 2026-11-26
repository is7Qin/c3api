// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/tidwall/sjson"
)

// ApplyRequestBodyPatch 是 request_patch body 面的**唯一运行期执行器**
// （spec custom-request-patch §2.5/§5.1/§7.4，P3 落地）。HTTP、WS 帧与探测
// 合成体共用同一套语义；本函数是纯函数——不读全局、不加锁、不写日志。
//
// 语义：
//   - fp == nil / 空（Set 与 Delete 皆空）⇒ body 原样返回（同一个 slice，零拷贝）；
//   - body 为空 / 非 JSON / 顶层不是 object 或 array ⇒ body 原样返回 + nil error：
//     **设计性跳过**（multipart edits、二进制、探测的合成体等），预期内，不是失败；
//   - 否则 delete（路径排序后逐个执行）→ set（路径排序后逐个执行，值用
//     SetRawBytes 写入保存期已归一好的原始 JSON 字节）。同一 path 同时出现在
//     delete 与 set 中时 set 胜（delete 先做，set 后覆盖）；
//   - JSON 合法但 sjson 执行失败（保存期已拦 → 残余只可能是脏库值/极端组合）：
//     **原 body 整体返回 + error**（不返回半应用结果）。调用方据此裸透传 +
//     限频 WARN（三面共用同一实现：HTTP/WS 直接调用，probe 面经注入回调），
//     **不中止请求**。
//
// 字节保真：set 的值走 sjson.SetRawBytes（不经 interface{} / float64），大整数
// 与嵌套值逐字保留；delete/set 的路径排序保证同一配置在不同 map 迭代序下产出
// 完全相同的字节（确定性）。body 不解析成 map[string]any。
func ApplyRequestBodyPatch(body []byte, fp *FormatPatch) ([]byte, error) {
	if fp == nil || (len(fp.Set) == 0 && len(fp.Delete) == 0) {
		return body, nil
	}
	if !isJSONContainerBody(body) {
		return body, nil
	}
	out := body
	if len(fp.Delete) > 0 {
		paths := make([]string, 0, len(fp.Delete))
		paths = append(paths, fp.Delete...)
		sort.Strings(paths)
		for _, path := range paths {
			next, err := sjson.DeleteBytes(out, path)
			if err != nil {
				// sjson 在失败时可能已构造部分结果；一律丢弃，原 body 全字节返回
				// （§7.4：「裸透传 + 不改写 body」）。
				return body, err
			}
			out = next
		}
	}
	if len(fp.Set) > 0 {
		paths := make([]string, 0, len(fp.Set))
		for path := range fp.Set {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			next, err := sjson.SetRawBytes(out, path, fp.Set[path])
			if err != nil {
				return body, err
			}
			out = next
		}
	}
	if len(out) == 0 {
		// sjson 只会在合法容器上做 splice，这里不可达（防御：空结果不得当 body）。
		return body, nil
	}
	return out, nil
}

// isJSONContainerBody 报告 body 是否是可执行 splice 的 JSON 容器
// （顶层 object 或 array）。空 / 全空白 / 非 JSON / 顶层标量都返回 false
// ——它们都是 §2.5 的「设计性跳过」，不是运行期失败。
//
// json.Valid 前置是防御性的：sjson 对畸形 JSON 会按自己的解析器尽力而为
// （可能产出更糟的字节），而畸形 body 从来不是网关要 patch 的目标——上游
// 出站 body 由网关自己构造，JSON 面已过入口 json.Valid。（images multipart
// 在这里第一字节即被挡下。）
func isJSONContainerBody(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	return json.Valid(trimmed)
}
