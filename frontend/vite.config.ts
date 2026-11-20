import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 构建产物输出到 ../web/dist，Go 用 go:embed 把它打进二进制，
// 部署时还是一个 Go 进程，不用另外起 Node 服务。
// 开发模式用 vite dev server，/api 代理到本地 Go 服务。
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: {
    outDir: '../web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
