import path from 'node:path'
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  server: {
    // `npm run dev` talks to a locally running shpyrd-server.
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
  build: {
    // Embedded into the server binary by ui/embed.go.
    outDir: 'dist',
    emptyOutDir: true,
  },
  test: {
    // Pure-logic unit tests (parsers, formatters); no DOM needed.
    environment: 'node',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
})
