import '@testing-library/jest-dom/vitest'
import { configure } from '@testing-library/react'

// The default 1000ms is the budget for one findBy* to settle. On a cold CI
// runner the first render of the heaviest route file is also the first time
// its module graph is evaluated, and that alone has taken longer than the
// budget (2026-09-10: the cockpit's first test failed at 1082ms on a run
// whose environment setup took eight seconds). A wider budget changes
// nothing a test asserts, only how long it is willing to wait for it.
configure({ asyncUtilTimeout: 5000 })
