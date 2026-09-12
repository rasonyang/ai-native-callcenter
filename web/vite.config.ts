import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { writeFileSync } from 'node:fs'
import path from 'node:path'

// `go:embed all:dist` (web/embed.go) matches nothing when dist/ holds no file,
// and a go:embed that matches nothing is a compile error — the go CI job cannot
// build at all. web/dist/.gitkeep is tracked to keep it non-empty. `emptyOutDir`
// deletes that placeholder on every build, and the next `git add -A` stages the
// removal, which is how the go job broke twice. Write it back once the bundle is
// on disk, so a built tree is never a tree that stages its deletion.
function keepDistPlaceholder(): Plugin {
  // Resolved, not assumed: outDir is relative to the Vite root, which is the
  // working directory, and not necessarily this file's directory.
  let outDir = ''
  return {
    name: 'aicc:keep-dist-placeholder',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir)
    },
    closeBundle() {
      writeFileSync(path.join(outDir, '.gitkeep'), '')
    },
  }
}

// The SPA is embedded into the Go binary from web/dist; in development Vite
// serves it and proxies the API to the Go process on :8080. Everything is
// plain HTTP: TLS is a deployment concern, never a dev-loop step.
export default defineConfig({
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react(),
    tailwindcss(),
    keepDistPlaceholder(),
  ],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, './src') },
  },
  server: {
    port: 5173,
    proxy: {
      // Same-origin in development so the session cookie and the event
      // stream behave exactly as they do behind the embedded build.
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
