import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import path from 'node:path'

// The layout tests: the same route components and API stubs as the jsdom suite,
// run in headless Chromium, because jsdom computes no layout and so cannot see
// a table wider than its card (#363, #378). `npm run test` never loads this.
//
// Unlike vitest.config.ts this one carries the Tailwind plugin: the tests load
// src/index.css (see src/test/browser-setup.ts), and a layout measured without
// the product's stylesheet and its Inter font says nothing about the product.
export default defineConfig({
  plugins: [tanstackRouter({ target: 'react' }), react(), tailwindcss()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, './src') },
  },
  server: {
    // Vite's default allow-list is the workspace root, web/. The hostile-value
    // generator reads field limits straight from docs/openapi.json, one level
    // up, so they cannot drift from the contract.
    fs: { allow: ['..'] },
  },
  test: {
    globals: true,
    setupFiles: ['./src/test/browser-setup.ts'],
    include: ['src/**/*.layout.test.{ts,tsx}'],
    browser: {
      enabled: true,
      provider: 'playwright',
      headless: true,
      // Playwright launches headless Chromium with --hide-scrollbars, which
      // measures every card 15px wider than a Windows browser, or a Mac with
      // "always show scrollbars", gives it. Keep the classic scrollbar.
      instances: [{ browser: 'chromium', launch: { ignoreDefaultArgs: ['--hide-scrollbars'] } }],
      // A failed run writes nothing into the tree; the assertion message names
      // the element and its measured size, which a screenshot would not.
      screenshotFailures: false,
    },
  },
})
