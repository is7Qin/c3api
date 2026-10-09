// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package redisx 全仓唯一 Redis 客户端构造点（spec 2026-08-25-redis-foundation-design
// §2.2）：cmd/server/main.go 调 Open，产物 *redis.Client 以构造注入分发。任何包不得
// 自建第二客户端（review grep 点：除本包与 main 外禁止出现 redis.NewClient）。
// 不做命令级包装层——miniredis 已解决可测性，接口是投机抽象；消费方直接用具体方法。
// Redis 只承载可丢的易失协调状态，永不作为 SoR/缓存层/失效总线。
package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// pingTimeout 启动期 Ping 预算：Redis 必选依赖，不可达 = 启动即 fatal，无需长等待。
const pingTimeout = 5 * time.Second

// defaultIOTimeout 是无 ctx deadline 命令的后备 socket 读写上界（= go-redis v9
// 默认 5s，此处显式钉住以明示契约）。启用 ContextTimeoutEnabled 后，逐命令的
// ctx deadline 生效（socket deadline 取 min(ctx deadline, now+defaultIOTimeout)）；
// 仅**不带 deadline 的 ctx** 才会退化到此值，故它必须 ≥ 全仓最大的 ctx 预算
// （当前最大为 pingTimeout 5s），不得无意缩短——否则会截短携带 ctx 的命令。
const defaultIOTimeout = 5 * time.Second

// Options 连接参数（config.RedisConfig 直映；TLS 本期不做，升级路径见 foundation spec §1）。
type Options struct {
	Addr     string
	Password string
	DB       int
}

// Open 构造并 Ping（foundation spec §2.2）：Ping 通过才交付非 nil 客户端；失败返回
// 错误由调用方 main 直接 fatal（Redis 必选，无"未启用"分支 → 消费方零 nil 容忍逻辑）。
// 运行期连接丢失 ≠ 此处失败：连接池自带重连，降级语义由消费方定义。
// 密码永不入日志/错误链——错误只含 addr。
//
// ContextTimeoutEnabled: true 是硬超时保证（spec 2026-10-09 §2.2b，裁决 A）：go-redis
// v9 默认忽略命令级 ctx deadline，"每批 Redis 操作 2s"与既有 contOpTimeout=2s 都靠它
// 才真正生效。全局生效——所有 Redis 消费者共享该客户端。
func Open(opt Options) (*redis.Client, error) {
	c := redis.NewClient(&redis.Options{
		Addr:                  opt.Addr,
		Password:              opt.Password,
		DB:                    opt.DB,
		ContextTimeoutEnabled: true,
		ReadTimeout:           defaultIOTimeout,
		WriteTimeout:          defaultIOTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping %s: %w", opt.Addr, err)
	}
	return c, nil
}

// Close 透传（worker manager 排空完成后最后资源释放，见 foundation spec §2.3）。
func Close(c *redis.Client) error { return c.Close() }
