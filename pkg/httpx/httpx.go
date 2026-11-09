// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package httpx 构造共享的上游 HTTP 客户端（规格 §10.2 连接层参数）。
package httpx

import (
	"net"
	"net/http"
	"net/url"
	"time"
)

type TransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	// MaxConnsPerHost 单 host 连接总数上限（含在用+空闲；0 = 不限）。
	// 网关既有 client 保持 0 不限（压测验证形态）；出口池按代理 spec 克隆同一底座，
	// codex 的 HTTP/WS 两条路径也用这份底座（sdkbridge/codex.go 注入 eg.HTTP.Transport），
	// 因此这里没有第二处参数来源。
	MaxConnsPerHost int
	IdleConnTimeout time.Duration
	DialTimeout     time.Duration
	ForceHTTP2      bool
	// HTTP2Shards h2 连接分片数（spec-upstream-h2-sharding）：>1 且 ForceHTTP2 时，
	// 上游并发摊到 N 个独立连接池，换 N 倍上行 BDP 与 1/N 的 TCP HOL 影响面。
	// <=1 = 历史行为（单 transport，逐字节等价）。
	HTTP2Shards int
	// Proxy 上游请求代理函数（nil = 直连，默认）。不再隐式装配
	// http.ProxyFromEnvironment——HTTP_PROXY 设了会静默改道全部上游请求
	//（含 x-api-key/Authorization 凭据，WS 升级大概率失败），压测行为随
	// 环境漂移。装配方显式决定：生产只有 main 一处 NewClient，codex 的 HTTP 与
	// WS 两条路径都注入出口池的 eg.HTTP.Transport（sdkbridge/codex.go），
	// 因此这里的每个参数对它们同样生效。
	Proxy func(*http.Request) (*url.URL, error)
}

func NewTransport(cfg TransportConfig) *http.Transport {
	return &http.Transport{
		Proxy: cfg.Proxy, // nil = 直连（防环境代理劫持 + 压测确定性）；装配方显式决定
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     cfg.ForceHTTP2,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func NewClient(cfg TransportConfig) *http.Client {
	tr := NewTransport(cfg)
	// 分片只在 h2 在用时才有意义：h1 每条连接一次只跑一个请求，本就没有共享
	// 连接窗，分片只会把连接数放大 N 倍且失去复用。
	if cfg.ForceHTTP2 {
		if st := NewShardedTransport(tr, cfg.HTTP2Shards); st != nil {
			return &http.Client{Transport: st}
		}
	}
	return &http.Client{Transport: tr}
}
