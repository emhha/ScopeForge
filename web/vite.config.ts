import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物直接输出到 internal/api/webdist,由 go:embed 打包为单二进制。
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: '../internal/api/webdist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/ws': { target: 'ws://127.0.0.1:8080', ws: true },
      '/health': 'http://127.0.0.1:8080',
    },
  },
})
