// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package headervalid 是出站 HTTP 头 wire 合法性判据的**全仓单源**，也是一个
// **只依赖 stdlib 的中立叶子包**（本包不得 import 除 stdlib 之外的任何东西）。
//
// 存在理由（为什么判据不在 pkg/httpx）：消费者横跨值类型层与可复用层——
// internal/domain 的 ParseRequestPatch 用它做模板 request_patch 的头名校验与
// 头值卫生，pkg/aiclient 的 RelayHeaders 用它做入站头值卫生（spec request_patch
// §2.6/§5.2）。pkg/httpx 同时承载 net/http 传输装配（TransportConfig /
// NewTransport / NewClient / ShardedTransport），而 Go 的 import 粒度是**包**：
// domain 只想调两个纯判据函数，却会把整棵传输装配拖进值类型层的依赖图。
//
// 方向（硬约束）：
//   - 本包**不得** import 任何 internal/*（架构门禁的 pkg/* 硬规则，零豁免）或
//     pkg/httpx（否则等于把判据又绑回传输装配层，搬家的意义归零）；
//   - 本包**只陈述 wire 合法性，不携带任何策略**：Accept-Encoding 的保存期拒绝
//     （响应编码不变量）、{api_key} 占位符、头名规范化都在请求 patches 的消费者
//     里，这里不做（否则就成了「两份判据」里的另一份）。
package headervalid

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
// 等价性的**冻结口径**：旧私有 valueClean 的判据只留在 headervalid_test.go 的
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
