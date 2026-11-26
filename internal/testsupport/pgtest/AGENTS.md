# internal/testsupport/pgtest — PG 集成测试的隔离数据库夹具

> 根 `AGENTS.md` 覆盖项目级约定。**跑 PG 测试前必读**。

## 职责

`pgtest.go` 的 `Clone` / `CloneEmpty` / `DSN` / `OpenPool` / `Precheck` / `RegisterDataLayerHooks` /
`SweepStaleTemplates` / `TemplateName`：给每个 PostgreSQL 集成测试**它自己的隔离库**，从单一已迁移
模板用 `CREATE DATABASE ... TEMPLATE` 克隆。

租一个小库是**文件级拷贝**，比每个测试重放整套 ent 迁移便宜得多，也消除了过去逼整个套件串行
（`go test -p 1`）的共享 schema 争用。

## 正确用法

```bash
export TEST_DATABASE_URL='postgres://postgres:c3api@localhost:15432/c3api_test?sslmode=disable'
export TEST_REQUIRE_PG=1

go run ./internal/testsupport/pgtestctl precheck    # 连接 / 权限 / 模板检查
go run ./internal/testsupport/pgtestctl sweep       # 清理残留模板库（失败后可重跑）
go test -p 4 -timeout 25m -count=1 ./internal/... ./cmd/... ./pkg/...
```

## 不变量 / 陷阱

- **`TEST_DATABASE_URL` 必须是完整 DSN**。缺变量时 `internal/testsupport/pgtest` 自身会红
  （`TEST_DATABASE_URL is not a full DSN`）——这是**前置条件缺失**，不是代码 bug。
- **并行度用 `-p 4`，不要 `-p 8`**：`TestPoolBuildAllocCeilings` 在 `-p 8` 下会偶发失败，
  **已在基线代码复现**（既有 flake）。失败先单跑该包确认。
- **残留模板要 sweep**：异常中断会留下 `c3api_test_template_<hash>`。
- 子包 `pgrepo` 注册数据层钩子；`pgtestctl` 是 CLI 入口。

## 验证

```bash
go run ./internal/testsupport/pgtestctl precheck
go test ./internal/testsupport/pgtest/ -count=1
```
