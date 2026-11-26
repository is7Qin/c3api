// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

// 出站 HTTP 头的 wire 合法性判据的**全仓单源**（spec request_patch §2.6/§5.2）：
//
//   - pkg/aiclient 的 RelayHeaders 用它做入站头的值卫生（替换原私有 valueClean）；
//   - internal/domain 的 ParseRequestPatch / service 的保存期校验用它做模板
//     request_patch 的头名校验与值校验。
//
// 放在 pkg 而不是 internal 的理由：三个消费者分属 pkg 与 internal，反向依赖
// （pkg → internal）会造出环；pkg/httpx 是既有可复用层。判据本身不新增依赖。
//
// 本文件**只陈述 wire 合法性**，不携带任何策略：Accept-Encoding 的保存期拒绝
// （响应编码不变量）、{api_key} 占位符、头名规范化都在请求 patches 的消费者里，
// 这里不做（否则就成了「两份判据」里的另一份）。

// ValidHeaderName 报告 s 是否是合法的 HTTP field-name（RFC 9110 §5.1 token）：
// 非空且每个字节都是 tchar（ALPHA / DIGIT / "!#$%&'*+-.^_`|~"）。大小写不敏感，
// 但本函数**不**做规范化——调用方负责 http.CanonicalHeaderKey。
func ValidHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		}
		return false
	}
	return true
}

// ValidHeaderValue 报告 s 是否可作为 HTTP field-value 出站（RFC 9110 §5.5 的
// field-vchar 放宽版，**逐字节等价于** pkg/aiclient 原 valueClean 判据）：
// 允许可打印 ASCII、TAB 与 UTF-8 高位字节；拒绝 0x7f（DEL）与 <0x20 的非 TAB
// 控制字节（CR/LF/NUL 等）。空串合法（空值在 request_patch 里另表 Del 语义）。
//
// 等价性的**冻结口径**：旧私有 valueClean 的判据只留在 header_test.go 的
// legacyValueClean（测试内的历史实现副本）里，表驱动等价测试对整个字节域
// （0x00–0xff 单字节 + 边界组合）逐项比对；改判据会红。
func ValidHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x7f || (c < 0x20 && c != '\t') {
			return false
		}
	}
	return true
}
