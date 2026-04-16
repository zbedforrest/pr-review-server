import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'path';

// Development server configuration
const DEV_SERVER_PORT = 3000;
const BACKEND_PROXY_URL = process.env.BACKEND_URL || 'http://localhost:7769';

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: DEV_SERVER_PORT,
    proxy: {
      '/api': {
        target: BACKEND_PROXY_URL,
        changeOrigin: true,
      },
      '/reviews': {
        target: BACKEND_PROXY_URL,
        changeOrigin: true,
      },
      '/ws': {
        target: BACKEND_PROXY_URL,
        ws: true,
      },
    },
  },
  build: {
    outDir: '../server/dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
  },
});
