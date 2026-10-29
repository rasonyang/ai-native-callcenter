import { configure } from '@testing-library/react'

// The product's stylesheet, Tailwind and the Inter font face included: the
// browser tests measure real layout, so they need the real rules.
import '@/index.css'

// Same budget as setup.ts: the first render of a route evaluates its module
// graph in a cold browser.
configure({ asyncUtilTimeout: 5000 })
