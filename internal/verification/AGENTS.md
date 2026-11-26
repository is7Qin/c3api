# internal/verification — 邮箱验证码的 Redis 存储

> 根 `AGENTS.md` 覆盖项目级约定。spec 2026-08-25-emailcode-redis-migration §2。

## 职责

`store.go` 的 `Store` / `New`：一个 `(purpose, email)` 一个 HASH，四命令直映
`service.EmailCodeStore`，**零 Lua**。

## 协议

```
key    c3api:emailcode:<purpose>:<email>
fields code_sha256 / attempts / updated_at(unixmilli)
expiry EXPIREAT <expires_at>
```

## 不变量

- **原生 TTL 取代 PG 版 `expires_at` 列的判定职责**：过期键由 Redis 删除 ⇒ `Get` 返回 `(nil, nil)`
  ⇒ 服务层走 "code invalid" 分支。**不要**在 Go 里再判一次时间。
- **`code_sha256` 不是明文**：验证码不落明文。
- `attempts` 计数在 Redis 内自增（防爆破），不要搬到内存。

## 验证

```bash
go test ./internal/verification/ -count=1
```
