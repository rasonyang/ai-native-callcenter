# /agent — feature inventory (phase 0)

Taken before any code was touched this round, so that nothing already wired can
be lost. The reference is the sibling `ui-test` checkout (its `/agent` route,
`components/softphone/*`, `components/dial-pad.tsx`).

Two hard facts frame everything below:

1. The reference is a **static mock**. Its softphone is a simulated state
   machine (`softphone-provider.tsx`); its transfer, conference and in-call
   keypad buttons are rendered `disabled` or wired to a no-op. It is a source
   of *visual* truth, never of behavioural truth.
2. This project drives a real switch. The endpoint list below is the complete
   set of call-control operations that exist in `docs/openapi.json`:
   `answer` · `hold` · `retrieve` · `hangup` · `transfer` · `dial` ·
   `calls/mine`, plus presence `login/logout/ready/not-ready/presence`.
   There is **no** mute, **no** DTMF-send, **no** conference endpoint, and
   `GET /calls` (the only live-queue source) requires SUPERVISOR.

---

## 1. What `/agent` implements today

| # | UI entry point | Handler | Backend call | File |
|---|---|---|---|---|
| 1 | Topbar "Sign in" button | `SignIn` → `usePresenceActions().signIn` | `POST /agent/login` | `components/softphone-bar.tsx:62` |
| 2 | Topbar presence chip → menu → "Go ready" | `PresenceControl` → `ready` | `POST /agent/ready` | `components/softphone-bar.tsx:125` |
| 3 | Topbar presence chip → menu → Break / Lunch / Training | `notReady` | `POST /agent/not-ready` | `components/softphone-bar.tsx:134` |
| 4 | Topbar presence chip → menu → "Sign out" | `signOut` | `POST /agent/logout` | `components/softphone-bar.tsx:145` |
| 5 | Topbar time-in-state timer | `useElapsedSec(enteredAt)` | derived in browser | `lib/agent.ts:89` |
| 6 | Topbar "Answer" button (ringing) | `useCallActions().answer` | `POST /calls/{callId}/answer` | `components/softphone-bar.tsx:181` |
| 7 | Topbar hold / retrieve toggle | `hold` / `retrieve` | `POST /calls/{callId}/hold` · `/retrieve` | `components/softphone-bar.tsx:193` |
| 8 | Topbar transfer menu (input + submit) | `transfer` | `POST /calls/{callId}/transfer` | `components/softphone-bar.tsx:202` |
| 9 | Topbar hangup button | `hangup` | `POST /calls/{callId}/hangup` | `components/softphone-bar.tsx:214` |
| 10 | Topbar caller number + call timer | `CallControls` | `GET /calls/mine` + browser clock | `components/softphone-bar.tsx:177` |
| 11 | Left card "Accept" (ringing) | `answer` | `POST /calls/{callId}/answer` | `routes/_app.agent.index.tsx:93` |
| 12 | Left card "Decline" | `hangup` | `POST /calls/{callId}/hangup` | `routes/_app.agent.index.tsx:102` |
| 13 | Left card hold / retrieve | `hold` / `retrieve` | `/hold` · `/retrieve` | `routes/_app.agent.index.tsx:112` |
| 14 | Left card transfer popover | `transfer` | `/transfer` | `routes/_app.agent.index.tsx:140` |
| 15 | Left card hangup | `hangup` | `/hangup` | `routes/_app.agent.index.tsx:124` |
| 16 | Dial card: number input + "Dial" | `useMutation(callApi.dial)` | `POST /calls/dial` | `routes/_app.agent.index.tsx:193` |
| 17 | Dial card: keypad popover `0-9 * #` | `KeypadButton` → appends to the input | none (composes a number) | `routes/_app.agent.index.tsx:238` |
| 18 | Callbacks card (open list) + "View all" link | `useCallbacks('OPEN')` | `GET /callbacks` | `routes/_app.agent.index.tsx:272` |
| 19 | Caller card: number, dialled DID, badges, `userData` | `CallerCard` | `GET /calls/mine` | `routes/_app.agent.index.tsx:311` |
| 20 | Journey card: one row per party, live per-leg timer | `JourneyCard`/`PartyRow` | `GET /calls/mine` | `routes/_app.agent.index.tsx:373` |
| 21 | Wrap-up card: ACW countdown + "Complete wrap-up" | `usePresenceActions().ready` | `POST /agent/ready` | `routes/_app.agent.index.tsx:447` |
| 22 | Presence card: status pill, extension, time-in-state | `usePresence` | `GET /agent/presence` | `routes/_app.agent.index.tsx:413` |
| 23 | SSE subscription (app-wide, drives every card above) | `useEventStream` → `applyToCache` | `GET /events` (EventSource) | `lib/use-event-stream.ts:46`, mounted at `routes/_app.tsx:29` |
| 24 | Stream health indicator in the topbar | `StreamIndicator` | derived from the EventSource | `components/app-shell.tsx:170` |

**SIP/ESL note.** No browser code in this project touches SIP or RTP. Audio,
registration and the microphone belong to the `web-sip-phone` Chrome
extension; every control above expresses *intent* over REST and FreeSWITCH
executes it over ESL. That is by design and is not a gap.

## 2. Reference UI elements, mapped

| Reference element | Here |
|---|---|
| Floating draggable softphone bar (`GripVertical` handle, `fixed z-50`) | **missing** — ours is docked in the topbar. Not adopted: see 需人工决策. |
| Bar: status select (dot + label + elapsed + chevron) | **present** (topbar) |
| Bar: vertical separators around the call segment | **missing** |
| Bar: call-info segment (direction icon + peer + elapsed) | **partial** — number + timer, no direction icon, no fixed-width segment |
| Bar: mute / unmute | **missing, and no backend** |
| Bar: hold / retrieve | **present** |
| Bar: dial-pad popover (number input + keypad + Call) | **missing from the bar** (exists in the page's Dial card) |
| Bar: solid-red hangup icon button | **present but styled `variant=destructive`**, not solid red |
| Bar: ACW panel (timer + "+30s" extend) | **partial** — countdown is on the page's wrap-up card; no extend (no endpoint) |
| Ringing banner (name, number, Accept / Decline) | **present** as the left card's ringing state |
| Active-call card, 3×2 control grid | **partial** — ours is a 1×3 grid (hold, transfer, hangup) |
| — mute | **missing, no backend** |
| — hold / retrieve | **present** |
| — transfer | **present** (ours is wired; the reference's is disabled) |
| — conference | **missing, no backend** |
| — in-call keypad | **missing**; the reference's own popover is a no-op (`onDigit={() => {}}`) |
| — hangup (solid red) | **present** |
| "My queue" card (waiting callers, live wait timer, red past 120 s) | **missing, no data source for an agent** (`GET /calls` is SUPERVISOR-only; `QUEUE_COUNT`/`QUEUE_JOINED` are declared in the contract but never published) |
| Contact card (name, tags, company, last contact) | **partial** — we show number/DID/badges/`userData`; there is no contact store |
| Live transcript card | **missing, no data source** (`BOT_TRANSCRIPT` is declared but never published) |
| Tabs: Interaction History / Orders / Notes | **missing, no data source** (no interaction, order or note store) |
| After-call work: disposition category + code + note + Complete | **partial** — countdown + Complete are real; disposition/notes have no endpoint |
| "Today" stats (calls handled, AHT, ACW avg, occupancy) | **missing** — `GET /reports/*` exists but is SUPERVISOR-scoped |
| Topbar avatar + chevron identity menu | **present** (initials + role text, no avatar circle styling parity) |
| SIPFloat headset button with green registration dot | **missing** — ours is a `StreamIndicator` (deliberate: see the comment at `app-shell.tsx:162`) |

## 3. Conclusions

| Capability | Reference | This project | Conclusion |
|---|---|---|---|
| Answer | mock | `POST /calls/{id}/answer` | **keep** |
| Hangup / decline | mock | `POST /calls/{id}/hangup` | **keep** |
| Hold / retrieve | mock | `/hold` · `/retrieve` | **keep** |
| Transfer | rendered `disabled` | `/transfer`, wired | **keep** — ours is strictly better; do not copy the disabled state |
| Mute | mock toggle | none at inventory time | **built this round** — `POST /calls/{id}/mute` · `/unmute`, `uuid_audio` |
| Conference | rendered `disabled` | none | **needs UI, rendered disabled** — no backend capability |
| In-call DTMF send | popover, no-op | none at inventory time | **built this round** — `POST /calls/{id}/dtmf`, `uuid_send_dtmf`. Ours works; the reference's is a no-op |
| Keypad → compose an outbound number | in the bar's dial popover | in the page's Dial card | **needs wiring** — add the same popover to the topbar bar, reusing `callApi.dial` |
| Outbound dial | mock | `POST /calls/dial` | **keep** |
| Presence switch (ready / not-ready / sign out) | mock | four endpoints | **keep** |
| Time-in-state / call timers | mock | browser-derived | **keep** |
| Queue subscription (my queue list) | mock array | none for an agent | **reference-only, no data source** |
| SSE subscription | none | `GET /events`, app-wide | **keep** — reference has no equivalent |
| Live transcript | mock array | none | **reference-only, no data source** |
| Interaction history / Orders / Notes | mock | none | **reference-only, no data source** |
| Disposition codes on wrap-up | mock selects | none | **reference-only, no data source** |
| Agent "Today" stats | mock | SUPERVISOR-only endpoints | **reference-only, no data source** |
| Callbacks list | none | `GET /callbacks` | **keep** — ours only |
| Call journey (per-party legs) | none | `GET /calls/mine` | **keep** — ours only |
| Stream-health indicator | none | ours | **keep** — ours only |

### Rule adopted for this round

A control the reference shows but this project cannot execute is rendered in
the reference's shape and **disabled**, with a `title` naming the reason. No
fabricated endpoint, no silent omission, and nothing already wired is removed.

---

## 4. Outcome

Round-2 results — before/after per capability, the verification each one got,
and the decisions still open — are in `docs/agent-align-report.md` §2 and §6.
Summary: all 24 capabilities inventoried above survive. Mute and in-call DTMF,
listed here as having no backend, were built end to end (contract → switch
adapter → UI) and are wired. Conference remains present-and-disabled: it needs
a consult-then-merge design, not one endpoint. Five reference panels stay
absent for want of a data source.

## 5. Round 3 — the agent workspace (2026-08-19)

Four of those five now have a data source, because the platform grew one. The
row "no data source" was never a property of the design; it was a list of
things nobody had built yet.

| Capability | Then | Now |
|---|---|---|
| My queue (waiting callers, live wait, red past the target) | `GET /calls` is SUPERVISOR-only; `QUEUE_COUNT`/`QUEUE_JOINED` declared and never published | `GET /calls/waiting` scoped to the agent's staffing + `QUEUE_JOINED`/`QUEUE_LEFT`/`QUEUE_COUNT` published from `telephony`. Red is the queue's own `slaThresholdSec`, not 120s in the browser |
| Disposition codes on wrap-up | no vocabulary, no endpoint | `dispositions` (seeded by the migration with the four the owner named), `GET /dispositions`, `POST /agent/wrap-up`, `wrap_ups` in the ledger, and the filing on the agent's own CDR rows. **Required**, and after-call work ends when it is filed rather than on a timer (owner directive 2026-08-19) |
| Contact card (name, tags, company, last contact) | number/DID/badges only; no contact store | `contacts` + full CRUD; the cockpit looks the caller up by exact number, and `/agent/contacts` is the book |
| Agent history ("My Calls", a placeholder in the reference) | `GET /cdrs` is SUPERVISOR-scoped | `GET /cdrs/mine`, the agent taken from the session and never from a parameter |
| Interaction history / Orders / Notes tabs | no store | **still deliberately absent** — that is a CRM, and the recommendation in the report's §6(d) stands |
| Agent "Today" stats | SUPERVISOR-scoped reports | `GET /reports/me` — calls handled, AHT, ACW average, occupancy — now the cockpit's right column, in place of the Presence card the softphone bar already duplicated |
| Live transcript | declared, never published | built in the transcription milestone; the cockpit's centre column carries both phases |
| Call journey (per-party legs) — ours, not the reference's | present | **removed**: with Contact answering who is on the phone and the transcript answering what is being said, a list of legs beside them was the switch's view of a call rather than the agent's |
