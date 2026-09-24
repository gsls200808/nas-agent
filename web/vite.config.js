import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时 /api 代理到后端 8080；构建产物由 Go embed 进二进制
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true
      }
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true
  }
})
