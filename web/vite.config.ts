import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: {
    host: '127.0.0.1',
    proxy: {
      // 所有 API 统一收口于 /api/*（/api/admin/*、/api/user/*），前端 SPA
      // 占用 /、/user/*、/app/* 已无前缀冲突，vite 代理无需 bypass 分流。
      // target 用 localhost（::1）而非 127.0.0.1：避免 127.0.0.1:18080 被
      // 其他进程（如 mock-mfp-api.mjs）占用导致代理误入 mock。
      '/api': { target: 'http://localhost:18080', changeOrigin: true },
      // AI 代理端点 /v1/* 保持直通（与 /api 隔离）
      '/v1': { target: 'http://localhost:18080', changeOrigin: true },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
