import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    // The Go binary embeds this directory verbatim.
    outDir: 'dist',
    emptyOutDir: true,
    // Source maps would roughly double the embedded payload for a build that
    // ships inside a container image.
    sourcemap: false,
  },
  server: {
    // `pnpm dev` proxies to a locally running tdrive so the UI can be worked
    // on without rebuilding the binary.
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
      '/dav': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
})
