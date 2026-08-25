// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package verification 邮箱验证码的 Redis 存储（spec 2026-08-25-emailcode-redis-migration
// §2）：一个 (purpose,email) 一个 HASH，四命令直映 service.EmailCodeStore，零 Lua。
//
//	key    c3api:emailcode:<purpose>:<email>
//	fields code_sha256 / attempts / updated_at(unixmilli)
//	expiry EXPIREAT <expires_at> —— 原生 TTL 取代 PG 版 expires_at 列的判定职责，
//	       过期键由 Redis 删除 ⇒ Get 返回 (nil,nil) ⇒ 服务层走 "code invalid" 分支。
//
// 客户端经构造注入不自建（pkg/redisx 单点构造纪律）；Redis 必选依赖 ⇒ 无 nil 分支。
package verification

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/domain"
)

// Store 验证码存储：实现 service.EmailCodeStore 四方法（语义与 PG 版逐条对齐，
// spec §2 映射表）。
type Store struct {
	client *redis.Client
}

// New 构造注入客户端（main 装配点唯一调用者）。
func New(client *redis.Client) *Store {
	return &Store{client: client}
}

// key 键名（spec §2）：purpose ∈ {register,reset} 有界集，email 为业务地址。
func key(purpose, email string) string {
	return "c3api:emailcode:" + purpose + ":" + email
}

// GetEmailCode HGETALL + PTTL 同管线取行；键不存在/已过期（含两命令间隙被删）
// 返回 (nil,nil)。ExpiresAt 由剩余 PTTL 重构——过期键已被删除，服务层过期分支
// 成为死路径（spec §2.1 取舍①），重构值仅供结构完整。
func (s *Store) GetEmailCode(ctx context.Context, email, purpose string) (*domain.EmailCode, error) {
	k := key(purpose, email)
	pipe := s.client.Pipeline()
	hgetAll := pipe.HGetAll(ctx, k)
	pttl := pipe.PTTL(ctx, k)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("verification: hgetall %s: %w", k, err)
	}
	ttl := pttl.Val()
	// ttl<=0 一并视同不存在：-2=键不存在；-1=无 TTL 孤儿（崩溃残留，spec §2 取舍）；
	// 0=本毫秒恰好到期——若不拦，重构的 ExpiresAt 会令服务层打出 "code expired"
	// 文案（审查发现④边界），原生 TTL 本应让它塌缩为 invalid。
	if ttl <= 0 || len(hgetAll.Val()) == 0 {
		return nil, nil
	}
	attempts, err := strconv.Atoi(hgetAll.Val()["attempts"])
	if err != nil {
		return nil, fmt.Errorf("verification: corrupt attempts %s: %w", k, err)
	}
	ms, err := strconv.ParseInt(hgetAll.Val()["updated_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("verification: corrupt updated_at %s: %w", k, err)
	}
	return &domain.EmailCode{
		Email:      email,
		Purpose:    domain.EmailCodePurpose(purpose),
		CodeSHA256: hgetAll.Val()["code_sha256"],
		ExpiresAt:  time.Now().Add(ttl),
		Attempts:   attempts,
		UpdatedAt:  time.UnixMilli(ms),
	}, nil
}

// UpsertEmailCode 覆盖写三字段 + EXPIREAT：覆盖 ⇒ attempts 归零、旧码失效；
// updated_at 刷新 ⇒ 60s 重发抑制滑窗（spec §2 映射表）。返回组装的行（调用方
// 忽略返回值，仅用 error）。
// 两命令走 TxPipeline（MULTI/EXEC——非 Lua/WATCH，不违 spec 裁决）：原子消除了
// 「HSET 成功后崩溃残留无 TTL 孤儿」窗口（审查发现①），顺带省 1 RTT。
func (s *Store) UpsertEmailCode(ctx context.Context, email, purpose, sha256 string, expiresAt time.Time) (*domain.EmailCode, error) {
	k := key(purpose, email)
	now := time.Now()
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, k, map[string]any{
		"code_sha256": sha256,
		"attempts":    0,
		"updated_at":  now.UnixMilli(),
	})
	pipe.ExpireAt(ctx, k, expiresAt)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("verification: upsert %s: %w", k, err)
	}
	return &domain.EmailCode{
		Email:      email,
		Purpose:    domain.EmailCodePurpose(purpose),
		CodeSHA256: sha256,
		ExpiresAt:  expiresAt,
		Attempts:   0,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// IncrementEmailCodeAttempts HINCRBY 自增并返回新值。HINCRBY 不触碰 TTL——剩余
// 有效期保持（spec §2 映射表）。并发说明（spec §2）：键在 Get 后恰好过期的极端
// 竞态下 HINCRBY 会新建无 TTL 的孤儿 HASH，最坏多计一次 attempts（方向保守），
// 成功双删幂等——直映不引入新竞态，无需 Lua/WATCH。
func (s *Store) IncrementEmailCodeAttempts(ctx context.Context, email, purpose string) (int, error) {
	n, err := s.client.HIncrBy(ctx, key(purpose, email), "attempts", 1).Result()
	if err != nil {
		return 0, fmt.Errorf("verification: hincrby %s: %w", key(purpose, email), err)
	}
	return int(n), nil
}

// DeleteEmailCode DEL（键不存在返回 0 不报错——消费/过期清理幂等，调用方均忽略错误）。
func (s *Store) DeleteEmailCode(ctx context.Context, email, purpose string) error {
	if err := s.client.Del(ctx, key(purpose, email)).Err(); err != nil {
		return fmt.Errorf("verification: del %s: %w", key(purpose, email), err)
	}
	return nil
}
