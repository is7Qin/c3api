# internal/testsupport — 测试基础设施

> 根 `AGENTS.md` 覆盖项目级约定。**跑任何 PG 集成测试前先读这里。**

## 子包

| 目录 | 作用 |
|---|---|
| `pgtest/` | 每测试一个隔离库（从单一已迁移模板 `CREATE DATABASE ... TEMPLATE` 克隆）。详见 `pgtest/AGENTS.md` |
| `pgtest/pgrepo` | 数据层钩子注册（`RegisterDataLayerHooks`），供 pgtest 建模板时使用 |
| `pgtestctl/` | CLI 入口：`precheck` / `sweep` |
| `racetest/` | 竞态检测辅助（`-race` 相关工具） |

## 标准前置流程

```bash
export TEST_DATABASE_URL='postgres://postgres:c3api@localhost:15432/c3api_test?sslmode=disable'
export TEST_REQUIRE_PG=1

go run ./internal/testsupport/pgtestctl precheck    # 连接 / 权限 / 模板
go run ./internal/testsupport/pgtestctl sweep       # 清理残留模板库

go test -p 4 -timeout 25m -count=1 ./internal/... ./cmd/... ./pkg/...
```

## 陷阱

- **`-p 4`，不要 `-p 8`**：`TestPoolBuildAllocCeilings` 在 `-p 8` 下偶发失败，**基线可复现**
  （既有 flake，与本仓库的逻辑无关）。
- **缺 `TEST_DATABASE_URL` 时 `pgtest` 自身会红**（`is not a full DSN`）——前置条件缺失，不是 bug。
- 异常中断会留下 `c3api_test_template_<hash>`，用 `sweep` 清。

## 架构位置

本包**允许** import `internal/db` / `internal/repository`（allow 矩阵里显式登记 `internal/testsupport` 的
白名单，+ `testFixtureAllow` 让测试侧还能取夹具包）。这是测试基础设施的合理特权，不是运行期分层。
