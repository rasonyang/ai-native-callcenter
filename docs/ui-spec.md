# Agent cockpit — visual specification

Measured values for aligning `/agent` with the reference implementation
(`ui-test`, a sibling checkout under the same workspace root). Every number
here was read from `getComputedStyle` / `getBoundingClientRect` in a live
browser. Nothing in this file is inferred from source or estimated from a
screenshot; anything not actually measured is written as **未测得**.

Append to this file; do not overwrite earlier passes.

---

## 1. State triggers (phase 1)

The earlier alignment attempt compared an idle cockpit against a reference in
its on-call state, which made every conclusion invalid. Both sides must be in
the same state before any value is compared.

### Reference

The reference softphone is a simulated state machine
(`components/softphone/softphone-provider.tsx`). It exposes a development-only
handle, added for exactly this purpose:

```js
window.softphoneSimulate('idle' | 'ringing' | 'on-call' | 'acw')
```

It is installed under `import.meta.env.DEV`, clears the pending ring timers and
sets agent + call state synchronously, so a state is reached deterministically
rather than by waiting on the 12 s inbound timer. Verified live: calling it with
`'on-call'` produces the active-call card, the queue list, the live transcript
and the after-call-work panel together.

### This project

The cockpit reads a live switch, so an equivalent state cannot be reached by
waiting. `web/src/lib/dev-fixture.ts` mirrors the reference approach: it seeds
the query cache and freezes refetching, and is installed from `main.tsx` only
under `import.meta.env.DEV`.

```js
window.aiccSimulate('idle' | 'ringing' | 'onCall' | 'wrapUp')
```

Verified live: `'onCall'` renders the active-call card with its controls, the
caller card with call-type/queue/language badges and business data, the call
legs, and presence reading *On call*.

### Comparison conditions

| | |
|---|---|
| Reference | `localhost:5174/agent` (a second dev server; `:5173` is occupied by the reference's own default port) |
| This project | `localhost:5175/agent` |
| State | `on-call` on both sides |
| Viewport | 1440 × 731 measured (see 需人工决策 — 900 not reachable with the available tooling) |
| deviceScaleFactor | 2 measured (1 not reachable with the available tooling) |

---

## 2. Measured values (phase 2)

### 2.1 Document tokens — on-call state

Read from `getComputedStyle(document.documentElement)`. Raw exports:
`docs/measure-ref-tokens.json`, `docs/measure-ours-tokens.json`.

Identical on both sides (case-insensitive hex):

`--background #FAFAFA` · `--foreground #18181B` · `--card #FFFFFF` ·
`--card-foreground #18181B` · `--popover #FFFFFF` · `--primary #4F46E5` ·
`--primary-foreground #FFFFFF` · `--muted-foreground #71717A` ·
`--destructive #DC2626` · `--border #E5E7EB` · `--input #E5E7EB` ·
`--ring #4F46E5` · `--state-available #16A34A` · `--state-oncall #4F46E5` ·
`--state-ringing #F59E0B` · `--state-acw #8B5CF6` · `--state-aux #71717A` ·
`--state-offline #D4D4D8` · `--state-breach #DC2626`

Body: `font-size 13px`, `color rgb(24,24,27)`, `background rgb(250,250,250)` —
identical on both sides.

Differences found:

| Token | Reference | This project | Status |
|---|---|---|---|
| `--muted` | `#FAFAFA` | `#f4f4f5` | **fixed** → `#fafafa` |
| `--accent` | `#FAFAFA` | `#f4f4f5` | **fixed** → `#fafafa` |
| `--chart-5` | `#71717A` | `#dc2626` | open — not used by `/agent`; changing it moves the wallboard |
| `--font-sans` | `'Inter Variable', 'Inter', system-ui, sans-serif` | same, plus CJK families | open — deliberate, see 需人工决策 |
| `--secondary`, `--secondary-foreground`, `--sidebar*` (9) | present | absent | open — unused by our components |
| `--radius-md`, `--tracking-widest`, `--blur-xs`, `--ease-in-out`, … | present | absent | Tailwind-internal, emitted only when the matching utility is used |

Token counts: reference 100, this project 87.

### 2.2 Element measurements

**未测得.** The per-element sweep (topbar container and its vertical rules,
status selector, call-info segment, icon button group, hangup button, avatar and
presence dot, left control grid, card container/title/title-aside, queue row and
its overtime value, transcript speaker labels, tab selected/unselected and
underline, interaction-history row, outcome badge, input, primary button), the
`getBoundingClientRect` set (three column widths, topbar height, main padding,
card gaps) and the interaction states (hover/focus/active/disabled) have not been
run yet.

---

## 3. Rules established from measurement

1. **State colour lives on the dot, not the text.** The reference paints the
   status label in ordinary foreground ink and lets the coloured dot carry the
   state. Red is reserved for destructive actions and overtime values.
   *(Recorded from the reference screenshots; the computed colour values behind
   it are 未测得.)*

---

## 4. 待确认 / Open questions

- Viewport height and deviceScaleFactor could not be pinned to 900 / 1 with the
  available browser tooling; measurements were taken at 1440 × 731 @2. Width —
  the dimension that drives the three-column layout — is correct.
- Whether the reference's `--muted`/`--accent` equalling `--background` is
  intentional or incidental; it was adopted here because it is what the
  reference actually computes.

---

# Pass 2 — full element sweep

Appended, not overwriting: this pass supersedes §2.2 ("未测得") above. Every
number below came from `getComputedStyle` / `getBoundingClientRect` in the
user's own Chrome, driven over the Claude-in-Chrome bridge. Raw exports live in
the session scratchpad (`measure/ref-oncall.json`, `ours-oncall-after.json`,
`ref-idle.json`, `ours-idle.json`, `ref-oncall-ix.json`, `ours-oncall-ix.json`).

## P2.0 Conditions

| | |
|---|---|
| Browser | the user's own Chrome (Claude-in-Chrome bridge), **not** an isolated Chrome for Testing |
| Reference | `localhost:5173/agent`, `window.softphoneSimulate('on-call' \| 'idle')` |
| This project | `localhost:5175/agent`, `window.aiccSimulate('onCall' \| 'idle')` |
| Viewport | **1440 × 731**, both sides, measured through the *same tab* |
| deviceScaleFactor | 2 (see 待确认) |

Two corrections to how pass 1 was run, both of which invalidated numbers:

1. The two sides were first measured in two different browser windows whose
   chrome differs by 56px, so every column height disagreed for no reason. All
   comparable values here were taken by navigating **one** tab between the two
   origins.
2. The reference's mock softphone drifts out of `on-call` on its own (its
   inbound-ring effect re-arms). Every measurement call re-runs
   `softphoneSimulate('on-call')` immediately before reading.

## P2.1 Reference — measured element table (on-call)

| element | font size/line-height | weight | color | background | radius | border | padding T/R/B/L | gap | shadow | w×h |
|---|---|---|---|---|---|---|---|---|---|---|
| `barContainer` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 0px/4px/0px/2px | 4px | none | 500.7×40 |
| `barSeparator` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(229, 231, 235) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 1×16 |
| `statusSelect` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 0px/8px/0px/8px | 6px | none | 94.7×32 |
| `statusDot` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(79, 70, 229) | 1.67772e+07px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 8×8 |
| `callInfoSegment` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/6px/0px/6px | normal | none | 224×20 |
| `callInfoIcon` | 13px/20px | 400 | rgb(79, 70, 229) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 14×14 |
| `callInfoNumber` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 109.5×20 |
| `callInfoElapsed` | 12px/16px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 34.4×16 |
| `iconButtonGroup` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 2px | none | 134×32 |
| `barMute` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 0px/0px/0px/0px | normal | none | 32×32 |
| `barHold` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 0px/0px/0px/0px | normal | none | 32×32 |
| `barDialPad` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 0px/0px/0px/0px | normal | none | 32×32 |
| `barHangup` | 13px/20px | 500 | rgb(255, 255, 255) | rgb(220, 38, 38) | 6px | 1px rgba(0, 0, 0, 0) | 0px/0px/0px/0px | normal | none | 32×32 |
| `userMenuTrigger` | 13px/20px | 500 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 0px/6px/0px/6px | 6px | none | 60×32 |
| `avatar` | 12px/16px | 500 | rgb(79, 70, 229) | oklab(0.510554 0.0279175 -0.228344 / 0.08) | 1.67772e+07px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 24×24 |
| `onlineDot` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(22, 163, 74) | 1.67772e+07px | 1px rgb(255, 255, 255) | 0px/0px/0px/0px | normal | none | 8×8 |
| `activeCallCard` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 16px/16px/16px/16px | normal | none | 320×184 |
| `controlGrid` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 6px | none | 286×70 |
| `gridMute` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(250, 250, 250) | 6px | 1px rgb(229, 231, 235) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `gridHold` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(250, 250, 250) | 6px | 1px rgb(229, 231, 235) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `gridTransfer` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(250, 250, 250) | 6px | 1px rgb(229, 231, 235) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `gridConference` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(250, 250, 250) | 6px | 1px rgb(229, 231, 235) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `gridKeypad` | 13px/20px | 500 | rgb(24, 24, 27) | rgb(250, 250, 250) | 6px | 1px rgb(229, 231, 235) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `gridHangup` | 13px/20px | 500 | rgb(255, 255, 255) | rgb(220, 38, 38) | 6px | 1px rgba(0, 0, 0, 0) | 0px/10px/0px/10px | 6px | none | 91.3×32 |
| `cardContainer` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 320×435 |
| `cardTitle` | 12px/16px | 500 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 66.4×16 |
| `cardTitleAside` | 12px/16px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 51.8×16 |
| `queueRow` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/16px/0px/16px | 8px | none | 318×36 |
| `queueRowWait` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 48×20 |
| `queueRowOvertime` | 13px/20px | 400 | rgb(220, 38, 38) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 48×20 |
| `transcriptSpeakerBot` | 12px/20px | 500 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 4px | none | 80×40 |
| `transcriptBotLine` | 13px/20px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 418×40 |
| `transcriptSpeakerCustomer` | 12px/20px | 500 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 4px | none | 80×20 |
| `transcriptCustomerLine` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | normal | none | 244.7×20 |
| `tabsList` | 13px/20px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 3px/16px/3px/16px | 20px | none | 538×32 |
| `tabSelected` | 13px/20px | 500 | rgb(79, 70, 229) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 2px/4px/2px/4px | 6px | rgba(0, 0, 0, 0) 0px 0px 0px 0px, rgba(0, 0, 0, 0) 0px 0px 0px 0px, rgba(0, 0, 0, 0) 0px 0px 0px 0px, rgba(0, 0, 0, 0) 0px 0px 0px 0px, rgba(0, 0, 0, 0) 0px 0px 0px 0px | 124.8×24 |
| `tabIdle` | 13px/20px | 500 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 6px | 1px rgba(0, 0, 0, 0) | 2px/4px/2px/4px | 6px | none | 52.5×24 |
| `historyRow` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/16px/0px/16px | 12px | none | 538×36 |
| `outcomeBadge` | 12px/16px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 1.67772e+07px | 1px rgb(229, 231, 235) | 0px/8px/0px/8px | normal | none | 121.6×20 |
| `textarea` | 13px/20px | 400 | rgb(24, 24, 27) | oklab(0.927576 -0.000511676 -0.00576836 / 0.5) | 6px | 1px rgb(229, 231, 235) | 8px/10px/8px/10px | normal | none | 246×64 |
| `selectTrigger` | 13px/20px | 400 | rgb(113, 113, 122) | rgba(0, 0, 0, 0) | 6px | 1px rgb(229, 231, 235) | 8px/8px/8px/10px | 6px | none | 246×32 |
| `primaryButton` | 13px/20px | 500 | rgb(255, 255, 255) | rgb(79, 70, 229) | 6px | 1px rgba(0, 0, 0, 0) | 0px/10px/0px/10px | 6px | none | 246×32 |
| `contactCard` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 16px/16px/16px/16px | normal | none | 540×80 |
| `wrapUpCard` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 16px/16px/16px/16px | normal | none | 280×246 |
| `statsCard` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 6px | 1px rgb(229, 231, 235) | 16px/16px/16px/16px | normal | none | 280×190 |
| `header` | 13px/20px | 400 | rgb(24, 24, 27) | rgb(255, 255, 255) | 0px | 0px rgb(229, 231, 235) | 0px/24px/0px/24px | normal | none | 1220×48 |
| `leftCol` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 16px | none | 320×635 |
| `centerCol` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 16px | none | 540×635 |
| `rightCol` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 0px/0px/0px/0px | 16px | none | 280×635 |
| `page` | 13px/20px | 400 | rgb(24, 24, 27) | rgba(0, 0, 0, 0) | 0px | 0px rgb(229, 231, 235) | 24px/24px/24px/24px | 16px | none | 1220×683 |

`1.67772e+07px` is how Chrome reports `calc(infinity * 1px)`, i.e. a full pill.
`oklab(0.510554 0.0279175 -0.228344 / α)` is `#4F46E5` at alpha α.

## P2.2 Type scale — the systemic finding

The reference sets **absolute** line heights; Tailwind's defaults are ratios
tuned to a 14px `sm`, which against a 13px base give 18.57px:

| utility | size | line-height |
|---|---|---|
| `text-xs` | 12px | 16px |
| `text-sm` | 13px | 20px |
| `text-base` | 14px | 20px |
| `text-lg` | 16px | 24px |
| `text-xl` | 20px | 28px |

Plain containers (no `text-*` class) inherit from `body`; the reference's body
resolves to 13px/20px, ours resolved to 13px/19.5px because Preflight's `1.5`
ratio applied. Both are fixed in `web/src/index.css`.

## P2.3 Interaction states (measured, real pointer)

Hover was produced with a real pointer move and confirmed with
`element.matches(':hover')` before reading. Focus was produced with
`element.focus()`. `disabled` was read from genuinely disabled controls.

| control | state | reference | this project |
|---|---|---|---|
| bar icon button (ghost) | rest | color `#18181B`, bg transparent, 1px transparent border, radius 6px | identical |
| bar icon button (ghost) | hover | **no change measured** | bg → `#FAFAFA` (`--muted`) |
| bar icon button (ghost) | focus | `box-shadow` ring token present, all layers transparent | identical shape |
| hangup (solid) | rest | color `#FFFFFF`, bg `#DC2626`, radius 6px | identical |
| hangup (solid) | hover | **no change measured** | bg → `oklab(0.577107 0.191166 0.0987778)` (`#DC2626` @ 90%) |
| grid button (outline) | rest | color `#18181B`, bg `#FAFAFA`, border 1px `#E5E7EB` | identical |
| grid button (outline) | hover | no change (`--muted` == `--background`, so the reference's own hover is a no-op) | identical |
| any control | disabled | `opacity: 0.5` (measured on the reference's genuinely-disabled disposition select) | `opacity: 0.5` |
| primary button | hover | bg → `oklab(0.510554 0.0279175 -0.228344)` | not applicable (this page has no filled primary button in the on-call state) |
| list row | rest / hover | no background change either side | identical |

**Conflict, reference source vs reference runtime.** The reference's `Button`
declares `hover:bg-muted` (ghost) and `hover:opacity-90` (the hangup buttons),
but neither produces any computed change in the running app — verified with the
pointer confirmed on the element. The same `hover:bg-muted` *does* work on the
reference's sidebar links (transparent → `#FAFAFA`), so the utility is built and
functional; something with higher precedence neutralises it on `[data-slot=button]`.
This project keeps its hover feedback. See 需人工决策 in the alignment report.

## P2.4 Rules established (extends §3)

1. **State colour lives on the dot.** Measured: the reference's status chip
   label is `rgb(24,24,27)` (ordinary foreground) with `font-weight: 500`; only
   the 8px dot carries `--state-*`. Fixed here — the label no longer takes
   `AVAILABILITY_COLOR`.
2. **Red is for destruction and for breach.** Measured uses of `#DC2626` in the
   reference: the hangup button's fill, and a queue row's wait value once it
   passes 120s (`queueRowOvertime`). Nothing else. A full-page sweep of this
   project found `var(--state-breach)` on: the hangup buttons (correct), inline
   error text under the dialler and the sign-in button (correct — a failure),
   and `AVAILABILITY_COLOR.DEVICE_UNREACHABLE` (correct — it is a fault dot, not
   a label). No further violations.
3. **The bar is one bordered container**, 40px tall, radius 6px, 1px `#E5E7EB`,
   white fill, `gap: 4px`, `padding: 0 4px 0 2px`, divided by 1×16px `#E5E7EB`
   hairlines into: status chip · fixed 224px call segment (`padding: 0 6px`) ·
   icon-button group (`gap: 2px`, 32×32 buttons).
4. **The card title line carries a secondary slot** at its end, 12px/16px
   `#71717A`, matching the title's own ink.
5. **Identity is an avatar**: 24px circle, `#4F46E5` on `#4F46E5`@8%, 12px/16px
   weight 500, inside a ghost trigger with `gap: 6px` and a 16px chevron.
   The reference's green presence dot (8px, `#16A34A`, 1px `--card` border)
   sits on its SIPFloat button; this project has no SIPFloat, so the same dot
   moves onto the avatar and is driven by real device state.

## P2.5 Idle state

Same conditions, `idle` on both sides. 13 of 19 comparable elements match
exactly on every measured property. The six that differ are listed in the
alignment report's residual table; none is a colour or a type value.

## P2.6 待确认 (updated)

- `deviceScaleFactor` is 2 and cannot be forced to 1 through the Claude-in-Chrome
  bridge. It does not affect any value here: `getComputedStyle` and
  `getBoundingClientRect` both report CSS pixels.
- Viewport height is 731, not 900: the display's `screen.availHeight` is 874, so
  a 900px viewport is not reachable on this machine. Width — the dimension that
  drives the three-column layout — is exactly 1440 on both sides.

---

# Pass 3 — the alignment rule, and what it excludes

**The target is the reference's intent, not its runtime.** Where the reference's
source expresses a decision and its running build fails to deliver it, the
decision is what gets copied. A measurement that disagrees with the reference's
own source is evidence of a defect there, not a specification.

Three places where this decides the outcome:

| Where | Reference source (intent) | Reference runtime (measured) | Adopted here |
|---|---|---|---|
| Ghost button hover | `hover:bg-muted hover:text-foreground` | no computed change at all | **the intent** — `--muted` on hover. The same utility works on the reference's own sidebar links (`transparent → #FAFAFA`, measured), so the build is capable of it and something with higher precedence is neutralising it on `[data-slot=button]`. |
| Destructive button hover | `hover:opacity-90` over an inline red fill | no computed change | **the intent, expressed as `hover:bg-destructive/90`** — the same "recede slightly under the pointer", without fading the glyph along with the fill. |
| Tab strip height | `className="h-10 …"` on `TabsList` | **32px** — the variant selector `group-data-horizontal/tabs:h-8` outranks the plain `h-10` passed in | **the intent** — 40px. Our `TabsList` sets `h-10` with no competing variant, so it delivers what the reference asked for. |

Not defects, and therefore adopted as measured:

- `--muted` equals `--background` (`#FAFAFA`). Both are written out explicitly
  in the reference's `:root`, so the near-invisible hover on a white card is a
  deliberate quietness, not an accident.
- `opacity: 0.5` on disabled controls — source and runtime agree.

## P3.1 Guard

The two systemic defects this alignment uncovered were both in the token layer
and neither is observable in a component test — jsdom implements no `@layer`,
so a layered rule does not apply there at all. `web/src/test/tokens.test.ts`
compiles `src/index.css` with Tailwind's own compiler and asserts on the real
stylesheet instead:

- the universal `border-color` default resolves inside `@layer base`, and no
  unlayered universal rule exists at all — an unlayered `*` outranks every
  layered utility regardless of specificity, which is what silently disabled
  every border-colour utility in the product;
- `text-xs/sm/base/lg/xl` carry the absolute line-heights 16/20/20/24/28, and
  `body` carries an absolute 20px rather than Preflight's 1.5 ratio.

The guard was verified by reintroducing the defect: moving the `*` rule back
out of `@layer base` fails two of its assertions, and restoring it passes them.
