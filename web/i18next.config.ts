import { defineConfig } from 'i18next-cli'

// 门禁=只读漂移检查（extract --dry-run --ci）。选项对齐本项目运行时契约：
// - defaultNS:false —— locales/{en,zh}.json 无 namespace 包装（i18n.ts 直接 import 为 translation 资源）
// - removeUnusedKeys:false —— ops.stats.* 等运行时动态 key 静态分析不可见，不得删除
// - sort:false —— 保持既有文件键序，避免纯格式化漂移
// - disablePlurals:true —— 项目用单 key + {{count}} 插值（无 _one/_other 变体），运行时即回退基 key
export default defineConfig({
  locales: ['en', 'zh'],
  extract: {
    input: ['src/**/*.{ts,tsx}'],
    output: 'src/locales/{{language}}.json',
    defaultNS: false,
    removeUnusedKeys: false,
    sort: false,
    disablePlurals: true,
  },
})
