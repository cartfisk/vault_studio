import { defineConfig } from 'vite'
import viteReact from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { fileURLToPath, URL } from 'node:url'
import refractionDisplacementMapPlugin from './vitePlugin'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [
    tanstackRouter({
      target: 'react',
      autoCodeSplitting: true,
    }),
    viteReact(),
    tailwindcss(),
    refractionDisplacementMapPlugin(),
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    allowedHosts: [
      "vault.dubstep.win"
    ],
    host: true, // Listen on all network interfaces
    port: 3000,
    proxy: {
      '/api': {
        // Point the dev server at a real backend with:
        //   VITE_PROXY_TARGET=http://<unraid-ip>:8081 npm run dev
        // Proxying (rather than calling the backend directly) keeps the browser
        // on one origin, which sidesteps both CORS and the Secure/SameSite
        // cookie rules the deployed server enforces.
        target: process.env.VITE_PROXY_TARGET || 'http://localhost:8080',
        changeOrigin: true,
        ws: true, // Enable WebSocket proxying
        timeout: 300000, // 5 minutes for large exports
        proxyTimeout: 300000,
      },
    },
  },
})
