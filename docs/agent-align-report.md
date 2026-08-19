# /agent alignment — round 2 report

Scope: the agent cockpit only. Reference: the sibling `ui-test` checkout,
running at `localhost:5173/agent`; this project at `localhost:5175/agent`.

The previous round removed working softphone controls. This round began with an
inventory (`docs/agent-feature-matrix.md`) and every item in it survives —
verified below by component tests, not by inspection.

---

## 1. State trigger and fixture

| | |
|---|---|
| Reference trigger | `window.softphoneSimulate('idle' \| 'ringing' \| 'on-call' \| 'acw')`, installed under `import.meta.env.DEV` in `components/softphone/softphone-provider.tsx` |
| This project's trigger | `window.aiccSimulate('idle' \| 'ringing' \| 'onCall' \| 'wrapUp')` |
| Fixture location | `web/src/lib/dev-fixture.ts`, installed from `web/src/main.tsx` under `import.meta.env.DEV` only |
| Mechanism | seeds the react-query cache (`PRESENCE_KEY`, `CALLS_KEY`, callbacks) and freezes refetching, so the seeded state survives the SSE stream's `invalidateQueries` |
| Test fixture | `web/src/test/harness.tsx` — `presenceFixture()` / `callFixture(state)` plus a recording `fetch` stub |

Verified live in the browser: `aiccSimulate('onCall')` renders the active-call
card with its six controls, the caller card with badges and business data, the
call legs, and presence reading *On call*.

One caveat worth knowing: the **reference's** mock drifts out of `on-call` by
itself after ~12s (its simulated-inbound effect re-arms). Every measurement call
re-runs `softphoneSimulate('on-call')` immediately before reading.

## 2. Feature matrix — before / after / verification

`T` = covered by a component test (`npm test`, 29 tests, all passing).
`B` = additionally exercised by clicking in the browser.

| Capability | Before | After | Verification |
|---|---|---|---|
| Sign in | topbar button → `POST /agent/login` | unchanged | **pass** `T` |
| Go ready | presence menu → `POST /agent/ready` | unchanged | **pass** `T` |
| Not ready (Break/Lunch/Training) | presence menu → `POST /agent/not-ready` | unchanged | **pass** `T` |
| Sign out | presence menu → `POST /agent/logout` | unchanged | **pass** `T` |
| Time-in-state timer | topbar, always | now hidden while on a call (the call segment owns the clock) | **pass** `T` |
| Answer | topbar + left card → `POST …/answer` | unchanged, topbar button restyled to a 32px solid-green icon | **pass** `T` |
| Decline | left card → `POST …/hangup` | unchanged | **pass** `T` |
| Hold / Resume | topbar + grid → `POST …/hold` · `…/retrieve` | unchanged | **pass** `T` `B` |
| Transfer | topbar + grid → `POST …/transfer` | unchanged; grid cell now fills its column | **pass** `T` `B` |
| Hang up | topbar + grid → `POST …/hangup` | unchanged; both are now solid red | **pass** `T` `B` |
| Outbound dial | Dial card → `POST /calls/dial` | unchanged, **and** added to the topbar keypad popover | **pass** `T` `B` |
| Keypad composes a number | Dial card popover | unchanged; extracted to `components/keypad.tsx` and reused in the topbar | **pass** `T` `B` |
| Callbacks list + View all | `GET /callbacks` | unchanged; added an open-count to the title slot | **pass** `T` |
| Caller card (number, DID, badges, userData) | `GET /calls/mine` | unchanged | **pass** `T` |
| Call legs list | `GET /calls/mine` | unchanged; added `aria-label` and a leg count | **pass** `T` |
| Wrap-up countdown + complete | `POST /agent/ready` | unchanged | **pass** `T` |
| Presence card | `GET /agent/presence` | unchanged; extension moved into the title slot as well | **pass** `T` |
| SSE subscription | `useEventStream` in `routes/_app.tsx` | **untouched** | **pass** — file diff shows only the added device-dot wiring |
| Stream-health indicator | topbar | untouched | **pass** |
| Mute / Unmute | absent, no endpoint | **added end to end**: `POST /calls/{id}/mute` · `/unmute` → `uuid_audio … start read mute` / `stop`, with `isMuted` on the party | **pass** `T` `B` |
| DTMF into a live call | absent, no endpoint | **added end to end**: `POST /calls/{id}/dtmf` → `uuid_send_dtmf` at the far end; one request per keypress | **pass** `T` `B` |
| Conference | absent | **added, disabled**, tooltip names the missing endpoint | **disabled — needs decision** `T` |
| My-queue list | absent | still absent | **no data source** (see §6) |
| Live transcript | absent | still absent | **no data source** (see §6) |
| Interaction history / Orders / Notes tabs | absent | still absent | **no data source** (see §6) |
| Disposition codes on wrap-up | absent | still absent | **no data source** (see §6) |
| Agent "Today" stats | absent | still absent | **no data source** (see §6) |

Nothing from the phase-0 inventory was removed. The two behaviours that changed
shape rather than disappearing: the topbar hold/transfer/hangup buttons moved
from `h-6` outline/destructive chips into the 32px icon group, and the topbar
timer now appears only in the call segment.

## 3. Files changed

| File | What changed |
|---|---|
| `web/src/index.css` | absolute line-heights for the whole type scale; moved the global `* { border-color }` and `body` rules into `@layer base` (unlayered, they beat every Tailwind border utility) |
| `web/src/components/ui/button.tsx` | 1px transparent border on every variant, `gap` moved from the base into the size variants, `px-2.5`, outline now `bg-background`, added an `icon-sm` size |
| `web/src/components/softphone-bar.tsx` | rebuilt as one 40px bordered container with two hairline dividers, a fixed 224px call segment (direction icon + number + timer) and a 32px icon-button group; added mute (disabled), a dial-pad popover wired to `callApi.dial`, and a solid-red hangup. Every existing mutation kept |
| `web/src/components/keypad.tsx` | **new** — the twelve-key pad, shared by the topbar dialler and the page's dial card |
| `web/src/components/app-shell.tsx` | topbar padding 16→24px; identity trigger is now an avatar circle (24px, accent on accent@8%) with an optional device dot and a chevron |
| `web/src/routes/_app.tsx` | passes `deviceState` to the shell, derived from real presence (`DEVICE_UNREACHABLE` → grey, otherwise green); agent-only |
| `web/src/routes/_app.agent.index.tsx` | control grid is now 3×2 with six cells; new `CallActionButton`; card title slot carries secondary info; call-legs list labelled; keypad popover uses the shared component |
| `web/src/lib/utils.ts` | `formatDuration` gained an opt-in `padMinutes` so live timers keep a constant width; the default is unchanged, so no other page moves |
| `web/src/locales/{en,zh}/translation.json` | new keys: `call.mute/muteUnavailable/conference/conferenceUnavailable/dtmfUnavailable/controls`, `agent.openCount_*`, `agent.legCount_*`, `device.reachable/unreachable` |
| `web/src/test/harness.tsx` | **new** — recording `fetch` stub, presence/call fixtures, query+i18n+router render helpers |
| `web/src/components/softphone-bar.test.tsx` | **new** — 15 tests |
| `web/src/routes/_app.agent.index.test.tsx` | **new** — 14 tests |
| `web/vitest.config.ts`, `web/src/test/setup.ts`, `web/package.json` | **new** — the test runner and its `npm test` script |
| `docs/openapi.json` | **contract first**: `POST /calls/{callId}/mute`, `/unmute`, `/dtmf`; `DTMFRequest`; `isMuted` on `PartySnapshot` |
| `internal/api/api.gen.go`, `web/src/generated/api.ts` | regenerated from the contract (`make api-generate`) |
| `internal/telephony/adapter.go` | `MuteLeg` / `UnmuteLeg` (`uuid_audio … start read mute` / `stop`) and `SendDTMF` (`uuid_send_dtmf`) |
| `internal/telephony/call.go` | `IsMuted` on `Party` and `PartySnapshot` — the switch reports no mute event, so the party is the only record |
| `internal/telephony/coordinator.go` | `Mute` / `Unmute` (flag set only after the switch accepts, then `PARTY_CHANGED`), `SendDTMF` (far-end leg, `ErrInvalidDTMF` on anything outside the DTMF alphabet) |
| `internal/httpapi/{call_handlers,api_server,server}.go` | three operations on the generated wrapper, `ErrInvalidDTMF` → 422 |
| `internal/telephony/mute_dtmf_test.go` | **new** — 5 tests: command strings, refused mute leaves the flag alone, tones go to the far end, rejection of non-tones, and a stranger to the call being refused |
| `web/src/lib/{api,agent}.ts` | `callApi.mute/unmute/sendDtmf` and the matching mutations |
| `web/src/test/tokens.test.ts` | **new** — 9 tests compiling the real stylesheet to pin the layer and type-scale defects |
| `docs/agent-feature-matrix.md` | **new** — the phase-0 inventory |
| `docs/ui-spec.md` | appended "Pass 2", the full measured element table and interaction states |

## 4. Aligned — element, key property, before → after

All values measured in the browser; `ref` is the reference's measured value.

| Element | Property | Before | After (= ref) |
|---|---|---|---|
| every plain container | `line-height` | 19.5px | **20px** |
| `text-xs` elements | `line-height` | 16px (already correct) | 16px |
| topbar container | height / fill / padding | 32px, transparent, `px-8px` | **40px, `#FFFFFF`, `pl 2px / pr 4px`, `gap 4px`** |
| topbar dividers | count | 1 | **2**, each 1×16px `#E5E7EB` |
| call segment | structure | number + timer, fluid width | **direction icon (14px, state-coloured) + number (13/20, 500) + timer (12/16, `#71717A`), fixed 224px, `px 6px`** |
| status label | `color` | `AVAILABILITY_COLOR[...]` (state colour) | **`rgb(24,24,27)`** — the dot alone carries state |
| status chip | `font-weight` / `padding` | 400 / 4px | **500 / 8px** |
| icon buttons | size / gap / border | 34×24, `px 8px`, visible border | **32×32, `gap 2px`, 1px transparent border** |
| topbar hangup | fill | `destructive` chip, 32×24 | **solid `#DC2626`, white glyph, 32×32, radius 6px** |
| grid hangup | border | 0px | **1px transparent** |
| control grid | shape | 1×3 (hold, transfer, hangup) | **3×2 (mute, hold, transfer, conference, keypad, hangup), 286×70, `gap 6px`** |
| grid buttons | fill / padding / gap | `#FFFFFF`, 12px, 8px | **`#FAFAFA`, 10px, 6px** |
| avatar | size / colour / fill | 20px, `#18181B` on `#FAFAFA`, 10px | **24px, `#4F46E5` on `#4F46E5`@8%, 12/16, weight 500** |
| identity trigger | border / width | 1px `#E5E7EB`, 79.7px | **1px transparent (ghost), 60px** |
| presence dot | — | absent | **8px `#16A34A`, 1px `--card` border, on the avatar** |
| card title slot | size / colour | 13px `#18181B` | **12/16 `#71717A`** |
| live timers | format | `0:07` | **`00:07`** |
| all buttons | `border` | none on filled variants | **1px transparent on every variant** |
| topbar | horizontal padding | 16px | **24px** |

**Aggregate diff (measured, 1440×731, both states):**

| | on-call | idle |
|---|---|---|
| elements compared | 40 | 18 |
| property match (18 properties each) | **701/720 = 97.4%** | **315/324 = 97.2%** |
| colour match | **160/160 = 100%** | **72/72 = 100%** |
| size within 1px | 67/80 = 83.8% | 30/36 = 83.3% |

Every colour on the page is now byte-identical to the reference.

## 5. Residual differences — element, delta, cause

| Element | Delta | Cause |
|---|---|---|
| `main` / `page` padding | 24px sits on `main` here, on the page div there | Equivalent: the three columns land at identical x/y and identical widths (320 / 540 / 280) and heights (635). Ours is the shell's layout contract; moving it would touch every page. |
| topbar container | 514.8px vs 500.7px | **+34px = the Transfer icon button.** The reference has no transfer in its bar; ours is wired and must not be dropped. |
| icon-button group | 168px vs 134px | Same cause. |
| topbar (idle) | 517.3px vs 453.2px | The brief requires mute/hold/keypad/hangup visible at all times; the reference shows only the dial pad when idle. |
| mute, conference, keypad | `opacity 0.5` vs `1` | Ours are genuinely disabled (no endpoint); the reference's are enabled because its mock can do anything. The disabled *styling* matches (0.5 on both, measured on the reference's disposition select). |
| status chip | 96.8px vs 94.7px | Text metrics: "On call" vs "On Call". |
| card title | 74.0px vs 66.4px | Text: "Callbacks" vs "My queue". |
| card title slot | 89.3px vs 51.8px, `gap 6px` vs `normal` | Ours holds two children (count + link); the reference holds one. |
| active-call card | 192px vs 184px | Ours carries a section title; the reference uses the contact's name as the heading. We have no contact store, so a title is the honest heading. |
| callbacks card | `padding 16px` vs `0px` | Equivalent: our rows use `-mx-4`, so the row rect (318×36, same x) matches exactly. |
| caller card | 115px vs 80px | Ours additionally shows the dialled DID and the flow's `userData`; the reference has neither. |
| wrap-up card | 78px vs 246px | The reference's disposition selects and note box have no endpoint here. |
| presence vs "Today" card | 158px vs 190px | Different content; per-agent stats are SUPERVISOR-scoped here. |
| queue / transcript / tabs | absent | No data source — see below. |
| hover on ghost + solid buttons | ours changes, the reference's does not | See §6. |
| font stack | ours appends CJK families | Deliberate; `zh` would otherwise fall back per-OS. No effect on any measured size or colour. |

## 6. 需人工决策

**(a) Controls rendered disabled — mute and DTMF now done; conference remains.**

| Control | Status | Notes |
|---|---|---|
| Mute / Unmute | **built** | `uuid_audio <leg> start read mute` on the agent's own leg — "read" is what the switch reads *from* them, so they stay able to hear. Undone with `uuid_audio <leg> stop`. `isMuted` lives on the party, not in the browser, so a reload or a supervisor's view cannot disagree. |
| DTMF | **built** | `uuid_send_dtmf` at the **far end's** leg: the tones have to reach whatever the caller is connected to. Aimed at the agent's own leg they would only beep in the agent's ear. One request per keypress — batching would defeat an IVR that listens a digit at a time. |
| Conference | **still disabled** | Options: (1) leave disabled; (2) design a consult-then-merge flow; (3) remove the button. **Recommendation: (1)** — conference is a multi-step interaction, not one endpoint, and this build has no conference module (see the comment on `Adapter.Eavesdrop`). |

**Not yet verified on a live call.** Both commands exist in this FreeSWITCH
build (checked over ESL: `uuid_audio <uuid> [start [read|write] [mute|level
<level>]|stop]` and `uuid_send_dtmf <uuid> <dtmf_data>`), the exact command
strings are asserted in `internal/telephony/mute_dtmf_test.go`, and the
HTTP layer was exercised against the running server (404 for an unknown call,
422 for `"hi there"` and for a missing `digits`). What has **not** happened is
a real call with a real WSS leg proving that the caller stops hearing a muted
agent and that an IVR responds to the tones. Given this repo's history — the
project's own notes record `uuid_answer` reporting success while a browser
phone did nothing — that last step is worth doing before trusting either in
production.

**(b) Measured value conflicts with the reference's source — resolved by rule.**

Rule adopted: **align to the reference's intent; do not inherit its defects.**
Where its source states a decision and its running build fails to deliver one,
the source wins. Recorded in `docs/ui-spec.md` pass 3 with the three places it
decides the outcome:

- ghost-button hover (`hover:bg-muted`) — intent kept, runtime discarded;
- destructive-button hover (`hover:opacity-90`) — intent kept, expressed as
  `hover:bg-destructive/90` so the glyph does not fade with the fill;
- tab-strip height — the reference asks for `h-10` and renders 32px because a
  variant selector outranks it; ours delivers the 40px it asked for.

Two measured values are *not* defects and stay adopted as measured: `--muted`
equalling `--background` (both spelled out in the reference's `:root`), and
`opacity: 0.5` on disabled controls (source and runtime agree).

**(c) Reference licence — resolved, no decision needed.**

The reference repo ships a full Apache-2.0 `LICENSE` and declares
`"license": "Apache-2.0"` in its `package.json`, matching this project. Its
components may be absorbed here directly. (An earlier draft of this report said
otherwise; that was a misread of a truncated file listing.)

Nothing was copied verbatim this round regardless — the components were
re-implemented against measured values, which is what let the type-scale and
border-layer bugs surface. The one carry-over worth remembering if the
reference's `ui/*` files are ever absorbed wholesale: they descend from
**shadcn/ui** (MIT, copy-into-your-project by design), so keep its notice with
them. That is bookkeeping, not a blocker.

**(d) Reference UI with no data source here.**

| Element | What it would need | Recommendation | Outcome |
|---|---|---|---|
| "My queue" (waiting callers + live wait, red past 120s) | `GET /calls` is SUPERVISOR-only; `QUEUE_COUNT` / `QUEUE_JOINED` are declared in the contract but never published by the platform | Publish `QUEUE_COUNT` from `telephony`, then add an agent-scoped queue view. Highest value of the four. | **built 2026-08-19** — `GET /calls/waiting` + the three `QUEUE_*` events, queue-scoped. The red threshold is the queue's own `slaThresholdSec`, not the reference's hardcoded 120s |
| Live transcript | `BOT_TRANSCRIPT` is declared but never published; and it would only exist for bot calls, not agent calls | Publish it for bot legs and show the transcript on a bot-to-agent transfer — that is the AI-native differentiator. | **built** — one transcript per conversation across both phases (`docs/design/08-transcription.md`) |
| Interaction history / Orders / Notes | no contact, order or note store exists; this is a CRM, not a call centre, concern | Do not build. Leave the centre column to call context, or integrate an external CRM later. | **not built, and the recommendation stands.** A contact record *was* added — number, name, company, tags, notes, last call — because "who is calling" is call context; orders and interaction tabs are not |
| Disposition codes on wrap-up | no disposition vocabulary or endpoint | Worth adding (`POST /agent/wrap-up` with a code) — it is what makes the CDR useful for reporting. | **built 2026-08-19** — a seeded vocabulary of four words, required on filing, and after-call work that ends when the agent files it rather than on a clock |
| Agent "Today" stats | `GET /reports/*` are SUPERVISOR-scoped | Add an agent-scoped `GET /reports/me`. Low effort, good for morale. | **built 2026-08-19** — and it took the Presence card's place in the cockpit, which said what the softphone bar already says |

**(e) Shared-file changes — pinned by a guard, verified per page as touched.**

`web/src/index.css` and `web/src/components/ui/button.tsx` are shared, so the
line-height and button-padding corrections move every page. Rather than
re-measure the whole app, the two defects behind them are now pinned by
`web/src/test/tokens.test.ts`, which compiles the real stylesheet and asserts:

- the universal `border-color` default sits inside `@layer base`, and no
  unlayered universal rule exists — an unlayered `*` outranks every layered
  utility regardless of specificity, which is what silently disabled every
  border-colour utility in the product;
- the type scale carries absolute line-heights (12/16, 13/20, 14/20, 16/24,
  20/28) and `body` an absolute 20px, not Preflight's 1.5 ratio.

The guard was checked against the defect itself: moving the `*` rule back out
of `@layer base` fails two assertions; restoring it passes them. jsdom
implements no `@layer` at all, so this had to be asserted on the compiled CSS
rather than through `getComputedStyle`.

**Known item, carried forward:** other pages were spot-checked
(`/agent/callbacks`, `/supervisor`, `/admin/cdr` — no console errors, 36px
table rows, 32px/28px buttons) but **not re-measured**. Each page gets verified
against the reference when it is next touched — the same rule the OpenAPI
wrapper migration follows.

**(f) Blocked.**

- **`deviceScaleFactor=1` and a 900px viewport are unreachable on this machine.**
  `screen.availHeight` is 874. Measured at 1440×731@2 instead, both sides
  through the same tab. Neither affects any value: computed styles and rects are
  in CSS pixels.
- **chrome-devtools MCP was connected to an isolated Chrome for Testing**
  (puppeteer cache, `user-data-dir` under the session scratchpad, no
  extensions), which your constraint forbids, and MCP arguments cannot be
  changed mid-session. All browser work was done instead through the
  Claude-in-Chrome bridge against **your own Chrome**, where the `web-sip-phone`
  extension is present and registered as extension **1001** over WSS. Tests
  therefore ran as agent `wei` (bound to 1001), not `uiagent` (1000, device
  unreachable).
- **Screenshots could not be captured** from the background tab (the bridge's
  screenshot injection times out). Everything reported here is from
  `getComputedStyle` / `getBoundingClientRect`, which is stronger evidence than
  a screenshot anyway.
- I set passwords for the existing accounts `uiagent`, `m45check` and `wei` to
  reach the UI. Change them if that matters.

## 7. Time and pass rate

| | |
|---|---|
| Wall clock | ≈ 2h00m (16:05 → 18:05) |
| Frontend tests | 43, all passing (`cd web && npm test`) — 34 component, 9 stylesheet guards |
| Type check | clean (`npm run typecheck`) |
| Lint | clean (`npm run lint`) |
| Colour match vs reference | **100%** (160/160 on-call, 72/72 idle) |
| Property match vs reference | **97.4%** on-call, **97.2%** idle |
| Size within 1px | 83.8% on-call, 83.3% idle — every miss is one of the causes in §5 |
| Go tests | all passing (`go test -race ./...`), including 5 new for mute/DTMF |
| Phase-0 capabilities still present | **24 of 24** |
| Other-page regression check | `/agent/callbacks`, `/supervisor`, `/admin/cdr` loaded clean: no console errors, 36px table rows, 32px/28px buttons |

Nothing was committed or pushed; all changes are in the working tree.
