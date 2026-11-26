# pkg/cryptox — 密钥生成

> 根 `AGENTS.md` 覆盖项目级约定。

## 职责

`cryptox.go` 的 `NewGroupKey` / `NewManagementKey`：生成带前缀的随机密钥
（`crypto/rand` + `encoding/hex`，**零其它依赖**）。

## 不变量

- **必须用 `crypto/rand`**，禁用 `math/rand`。
- 前缀是标识的一部分（`ck-` 客户端 key / `mk-` 管理 key）：`auth` 侧按前缀先判分支，
  失败不回退（见 `internal/auth/AGENTS.md` 的管理 key 分支）。

## 验证

```bash
go test ./pkg/cryptox/ -count=1
```
