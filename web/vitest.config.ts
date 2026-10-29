import { configDefaults, defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import path from 'node:path'

// The app's Vite config (Tailwind, the dev proxy, the build's own plugins) is
// deliberately not reused here. The router's codegen is: `routeTree.gen.ts` is
// not committed, and the route tests navigate the generated tree, so it has to
// exist before they run, including on a fresh checkout where nothing was built.
// Code splitting stays off; a test imports the routes whole.
export default defineConfig({
  plugins: [tanstackRouter({ target: 'react' }), react()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, './src') },
  },
  // src/test/hostile.ts reads field limits from ../docs/openapi.json.
  server: { fs: { allow: ['..'] } },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    // *.layout.test.tsx needs a real browser's layout engine (vitest.browser
    // .config.ts, `npm run test:browser`). Under jsdom every width is zero, so
    // such a test would pass vacuously or fail for the wrong reason.
    exclude: [...configDefaults.exclude, 'src/**/*.layout.test.{ts,tsx}'],
  },
})
