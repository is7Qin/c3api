# internal/ — Go 后端分层地图

> 根 AGENTS.md 覆盖项目级约定/命令；本文件只讲 internal 各包职责与接线。深度主题见 `.omo/memory/topics/`。

## 包职责一览（文件数）

| 包 | 数 | 职责 |
|---|---|---|
| `ent/` | 164 | **生成物**。手写 schema 在 `ent/schema/*.go`（19 实体）；usage_logs/err_logs/usage_stats 三表被 migrate hook 排除，DDL 归 PartitionRepo 管 |
| `repository/` | 70 | Repository 门面实现 service.Store；`WithTx`(txDriver) 让 ent 构建器与裸 SQL 同事务；OpenPG 自动补 `lock_timeout=5s`、MaxConnLifetime=30m |
| `proxy/` | 69 | /v1/* 网关热路径（见根 CODE MAP） |
| `handler/` | 68 | 根包=管理面(/api/admin, oapi-codegen)；`user/`=用户面(/api/user)；`httpface/`=错误写出叶子包。**没有 v1 子包——/v1 在 proxy** |
| `service/` | 51 | 一资源一文件；`errors/` 叶子包 5 哨兵 |
| billing(13) scheduler(13) domain(12) rule(12) usage(10) protoconv(8) sdkbridge(8) pricing(8) notify(7) server(4) auth(4) invalidate(4) credential(2) config(2) snapshot(2) worker(2) | | 领域件 |

## 分层与请求流

```
chi(internal/server/server.go)
 ├─ /api/admin/* → adminAuth(静态token或platform_admin JWT，快照角色覆盖claims) → handler.AdminAPI
 ├─ /api/user/*  → user.Router(register/login公开，其余RequireJWT) → user.UserAPI
 └─ /v1/*        → inflightLimiter → proxy.AIRouter → guardPipeline(key快照鉴权零DB)
三面共用 httpface.WriteJSON/WriteServiceErr ←→ service.Service(哨兵错误) ←→ repository.Store
写库成功后 → inv.*(本地去抖) + publish(NOTIFY c3api_invalidate)
```

- **domain 是万能依赖汇**：60+ 文件引用，业务层(scheduler/proxy/service)只依赖它取共享类型
- **Service 注入模式**：不逐仓注入——一个复合 `Store` 接口(repository.Repository 实现)；构造 `New(store, sched, inv, pub, ruleReload, keys, log)`；循环依赖用 Set* 事后回填(SetLocalDispatcher/SetPriceFetcher/SetUsageSnapshotter)；读路径状态=4 个 atomic.Pointer 快照(settings/pricing/imagePrice/functionPrice)
- **角色模型仅两级** platform_admin|user；空表首个注册者自动 platform_admin；JWT HS256 TTL 24h，快照 status fail-closed

## 常驻 Worker（注册序=反序排空序，main.go:476-486）

| Worker | 循环/配置键(默认) | 要点 |
|---|---|---|
| invalidate | 200ms 窗口(硬编码) | trailing-edge 合并脏标记；reloadAll 矩阵在 invalidate.go:12-27 |
| scheduler | `scheduler.sync_interval`(30s) | 选号快照重建 |
| rule-engine | 事件通道+1min 窗口清理 | 有界 chan 满则丢弃计数 |
| usage | `usage.flush_interval`(500ms)+`quota_flush_interval`(10s) | 按 userID 分片 8 worker；毒行二分隔离 |
| errlog | `usage.errlog_flush_interval`(500ms) | exemptQ(必存)/rejectQ(风暴采样)双队列，来源不可串 |
| pricing-sync | settings `price_sync_cron`(默认 0 3 * * *) | 启动即拉一次+gronx cron |
| retention | 每小时 | DROP 过期日分区+预建今明；redemption_uses 有界 DELETE≤5000/轮 TTL 固定 90d |
| stats-agg | `usage.stats_agg_interval`(5m，0=禁用) | 双区间分离：watermark 只推进到 T=now−lag，绝不推进到重算上界 R1；advisory lock 单写者 |
| billing(条件) | `billing.flush_interval`(1s)+`balance_refresh_interval`(10s) | 账本游标消费（F2/F2-opt）：排空式循环+单取批面（零价行内存路由）；≤64 用户/chunk 单事务扣减+标记（SET LOCAL sync_commit=off）；毒行 chunk 折半→行级二分隔离；lag ≥1s 节流（Close 排空绕过） |
| notify | 阻塞 LISTEN | **专用 pgx.Conn 非池连接**(池回收会静默丢订阅)；断线退避 1s→30s，重连必 FullRefresh |
| auth-sync | 60s 兜底 Auth.Reload | cmd/server/auth_sync.go |

排空惯用法（billing 与 usage 两包各自声明，有意重复）：loopDone + baseCtx/baseCancel + flushMu 作在途屏障 + inflightAbandonGrace 500ms。

## NOTIFY 失效流

`c3api_invalidate` 单通道；Change 九字段(users/templates/clients/multipliers/keys/settings/rules/groups[]/src)。发布端 >6KB 降级(丢 Groups 置 Templates=true)；Src=hostname-pid-6B随机数 防容器 pid 撞车自吞。cmd/server/dispatcher.go 做 Change→Debouncer 映射；**settings 例外走同步 ReloadSettings**（预算要读到新 N）。计费扣费路径永不发 NOTIFY。

## 数据不变量（动 repository/billing/usage 前必读）

1. FEFO：temp_balances 按 expires_at ASC(NULLS LAST=永久最后)；行级条件 UPDATE→余额条件扣→无条件透支→用户缺失跳过但仍记日志
2. `cost == Σ logs.Cost` 经分块保持：逐行累加，禁止按比例公式(整数截断会归零)
3. 幂等键 `(request_id, created_at)` 唯一索引；COMMIT 孤儿重试撞 23505 = 成功；检测必须 errors.As 全链匹配
4. lock_timeout=5s 的由来：一次 pg_dump 长事务卡死 FEFO UPDATE→8 个 flush worker 全挂→全局计费停摆；statement_timeout 故意不设(杀 ScanStats 大窗口)
5. ent 迁移永不做分区表 DDL(atlas 无法 diff)；列定义 Go 事实由 TestUsageLogColumnDefsMatchCreateDDL 家族锚定
6. 新鲜库 watermark 初始=now−lag 而非 epoch（防全史扫描+撞已 DROP 分区）

## 配方：新增管理端点

1. `openapi/openapi.yaml` 加路径（勿打 user tag）
2. `go generate ./internal/handler` 重生成 api.gen.go
3. AdminAPI 方法实现：严格 decode → svc 调用 → httpface 写出；列表参数过 ClampLimit(200)
4. service 层：校验返回哨兵；写成功后 inv.* + publish；repo 错误走 mapRepoErr
5. 需新持久化：扩 service.go 对应 Store 子接口 → repository/<x>_repo.go 实现
6. 路由与鉴权零代码（HandlerWithOptions BaseURL=/api/admin 自动挂载）
7. 测试：handler 用 fakestore fakes；PG 测试按根 COMMANDS 起 test-compose
