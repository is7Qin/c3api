import { defineConfig } from 'i18next-cli'

// 门禁=只读漂移检查（extract --dry-run --ci）。选项对齐本项目运行时契约：
// - defaultNS:false —— locale 资源无 namespace 包装（i18n.ts 直接 import 为 translation 资源）
// - removeUnusedKeys:false —— ops.stats.* 等运行时动态 key 静态分析不可见，不得删除
// - sort:false —— 保持既有文件键序，避免纯格式化漂移
// - disablePlurals:true —— 项目用单 key + {{count}} 插值（无 _one/_other 变体），运行时即回退基 key
// - outputFormat:'ts' —— 生成 `export default {...} as const`，供 i18next 类型增广推断 key/插值类型
// 注意：动态拼接/运行时 key（nav.*、ops.stats.*、settings.* 等）静态不可见；src/locales/*.ts
//       只能增量编辑（extract 以现有文件为 key 集/键序基准），禁止删除后从零 regenerate，否则会丢动态 key。
export default defineConfig({
  locales: ['en', 'zh'],
  extract: {
    input: ['src/**/*.{ts,tsx}'],
    ignore: ['src/locales/**'],
    output: 'src/locales/{{language}}.ts',
    outputFormat: 'ts',
    defaultNS: false,
    removeUnusedKeys: false,
    sort: false,
    disablePlurals: true,
  },
})
