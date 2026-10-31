import { configure } from '@testing-library/react'

// The product's stylesheet, Tailwind and the Inter font face included: the
// browser tests measure real layout, so they need the real rules.
import '@/index.css'

// Same budget as setup.ts: the first render of a route evaluates its module
// graph in a cold browser.
configure({ asyncUtilTimeout: 5000 })

// A list page with real data scrolls, and a classic scrollbar (Windows, or a
// Mac with "always show scrollbars") takes 15px from the card. A fixture with
// a few rows would not scroll, so force the bar on the shell's scroll
// container: the layout is measured in the narrowest real case.
const forceScrollbar = document.createElement('style')
forceScrollbar.textContent = 'main { overflow-y: scroll !important; }'
document.head.appendChild(forceScrollbar)
