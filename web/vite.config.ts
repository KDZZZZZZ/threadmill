import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

const target = process.env.DEV_API_ORIGIN ?? 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
  build: { outDir: '../internal/cli/webassets', emptyOutDir: true, license: { fileName: 'assets/licenses.txt' } },
  server: {
    host: '127.0.0.1',
    proxy: Object.fromEntries(['/api/v1', '/healthz'].map(prefix => [prefix, {
      target, changeOrigin: true,
      // The gateway checks same-origin POSTs. Forward the trusted dev origin only.
      configure(proxy) { proxy.on('proxyReq', request => request.setHeader('Origin', target)) },
    }])),
  },
})
