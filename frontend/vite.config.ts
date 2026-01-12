import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'path';

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:7769',
        changeOrigin: true,
      },
      '/reviews': {
        target: 'http://localhost:7769',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: '../server/dist',
    emptyOutDir: true,
  },
});
