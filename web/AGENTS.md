# web/ — React 控制台

> 技术栈：React 18.3 + TS + Vite 8 + Tailwind v4(CSS-first 无 config) + shadcn 风格组件(@base-ui/react 底座，非 Radix) + react-query v5 + react-router v7。oxlint 非 ESLint。无前端测试设施——门禁就是 `tsc -b && vite build` 过。

## 结构

```
src/
├── App.tsx          # 唯一路由文件：/app(管理台12路由,RequireAdmin) + /user(用户台6路由)，无文件路由
├── components/      # 45 文件：顶层17共享件 + ui/(24 shadcn基元) + codex-import/(4)
├── lib/             # 14 文件：auth/i18n/format/use-debounced + api/(client.ts+schema.d.ts) + codex-import/(纯解析)
├── locales/         # en.json zh.json (i18next)
└── pages/           # 管理台平铺 pages/*.tsx；用户台 pages/user/*.tsx（目录镜像 URL）
```

## 命令

```bash
pnpm install --config.node-linker=hoisted   # 本机必须 hoisted（24H2 junction 失效）
pnpm run dev        # :5173，代理 /api 和 /v1 → localhost:18080
pnpm run build      # tsc -b && vite build → dist/ → go:embed
pnpm gen:api        # openapi/openapi.yaml 变更后必跑，重生成 src/lib/api/schema.d.ts
```

## 关键约定

- **schema.d.ts 是生成物**：改 API 后不跑 gen:api = 类型静默漂移；导入带显式扩展名 `from './schema.d.ts'`
- **响应字段保持 Go PascalCase**（ID/Name），查询参数 snake_case（template_id/next_cursor）——client.ts 有意不做 camelCase 转换，别"修"
- **单登录态**：platform_admin JWT 同时通吃两台控制台；`lib/auth.ts` 单一 userAuth store 支撑 api(/api/admin) 与 userApi(/api/user) 两个 ApiClient 实例
- **AppShell 不重挂载**：/app ↔ /user 切换只换 Outlet，sidebar/topbar 常驻（App.tsx:45 注释）
- **401 全局拦截**：QueryCache/MutationCache onError 统一清 token 跳登录页，页面级不写 401 处理

## 数据获取

- QueryClient 默认 `retry: 0`、`refetchOnWindowFocus: false`
- **日志分页不走 react-query**：自定义 `useCursorLogs`（components/use-cursor-logs.ts）键集状态机——cursor 链 + 已访页缓存 + 代际守卫防串台；仅两个消费者 pages/logs.tsx 与 pages/user/logs.tsx
- `keepPreviousData` 只活在 logs.tsx 的筛选候选下拉查询里（315-345 行），主分页路径已弃用
- **三种分页并存**（新页面先对后端确认是哪种）：limit/offset（模板/账号/组/用户）、cursor 键集（用量/错误日志，CursorPage<T>={rows,next_cursor}）、page/page_size（兑换码/价格/临时额度）
- Dashboard 轮询：overview 30s、users-top 10s

## 嵌入约束

`cmd/server/embed.go` 声明 `//go:embed all:dist`：`pnpm run build` 必须先于 `go build` 成功，否则二进制内嵌空 FS（服务可起但无 UI）。dist 目录列表被服务端刻意封禁。
