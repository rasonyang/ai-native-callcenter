# Design 05 — Frontend (SPA)

## 1. Stack & principles

React 19 + TypeScript, Vite, TanStack Router (file-based, code-split) + TanStack Query 5, Tailwind CSS 4 (CSS-first config), shadcn/ui `radix-nova` style on the consolidated `radix-ui` package, lucide icons, recharts, Inter variable font, oxlint. **Visual contract = ui-test verbatim**: its `index.css` tokens are copied (13px base scale 12/13/14/16/20, 6px universal radius, single accent `#4F46E5`, borders-not-shadows, the 7 `--state-*` colors used only as dots/pills/thin bars, `tabular-nums` everywhere), its shell (220px grouped sidebar + 48px fixed-target breadcrumb topbar), its patterns (36px table rows, hand-rolled `border bg-card p-4` cards, Sheet-drawer edit/inspect, Dialog only for short creates, Popover-confirm destructive actions, "line" Tabs variant). Dark mode stays unimplemented (as in ui-test). No freelancing. Identifier and file naming follow [07-naming.md](07-naming.md) §3: TS properties mirror JSON byte-for-byte, string-literal unions instead of `enum`, no `I` prefix, kebab-case file names — all already ui-test practice.

## 2. Routes (mirror ui-test; deltas noted)

`/login` (new — ui-test lacked auth; role comes from the session, the role `<Select>` is dropped). Agent: `/agent` cockpit (3-column, built to ui-test's spec incl. conditional RingingBanner/Transcript/WrapUp cards), `/agent/calls` (own CDR list — was placeholder), `/agent/callbacks` (new, replaces voicemail placeholder). Supervisor: `/supervisor` wallboard, `/supervisor/agents`, `/supervisor/queues` (staffing drawer), `/supervisor/quality`. Admin: `/admin` overview+health, `/admin/users`, `/admin/routing` (queues), `/admin/bots` + `/admin/bots/$flowId` designer (JSON editor + SVG graph + inspector + Validate/Publish), `/admin/trunks`, `/admin/dids` (new — ui-test folded numbers into trunks), `/admin/cdr`, `/admin/reports`, `/admin/audit`. Root `/` → role-appropriate default. ui-test's remaining agent placeholders (contacts/schedule/preferences) stay out of MVP.

## 3. Data layer

TanStack Query owns all server state (queries keyed by resource; mutations invalidate). **One `EventSource`** (app-level provider) feeds: (a) targeted `queryClient.setQueryData` patches for live entities (calls, agent states, queue counts, wallboard); (b) the softphone bar's call state machine; (c) transcript append streams (per-call buffer). `reset` event → invalidate live queries + re-tail. Realtime tickers (elapsed timers) run client-side off `since` timestamps, per ui-test.

## 4. Softphone bar & cockpit integration

`softphone-provider` implements ui-test's `ISoftphoneAdapter`-shaped contract but backed by real REST + SSE (states: AgentState × NotReadyReason, CallState DIALING/RINGING/TALK/HELD; ACW countdown from `wrap_up_until`). Ringing → popup + Answer button → `POST /calls/{id}/answer` (backend does `uuid_phone_event talk`; the extension answers — the SPA never touches audio). Extension registration health renders from `device.*` events (dot in the bar; "device unreachable" banner drives the `sip-float` visual role from ui-test).

## 5. i18n (mandate)

`i18next` + `react-i18next`, languages `en` (default) / `zh`, detection: user preference (settings) → browser. Namespaced flat keys: `common.*, nav.*, auth.*, agent.*, supervisor.*, admin.*, cdr.*, flows.*, errors.<CODE>, states.<STATE>, reasons.<REASON>, callTypes.<TYPE>` (enum-valued keys use the SCREAMING_SNAKE values verbatim, e.g. `states.READY`, `callTypes.INBOUND`, `errors.SWITCH_DOWN`). Every call surface (cockpit banner, popup, CDR rows) badges the call's `callType` so agents instantly see whether the customer called in or was called. Rules: zero hardcoded user-visible strings in components (oxlint rule + review checklist); API errors rendered via `errors.<CODE>` with `params` interpolation; dates/numbers/durations via `Intl` with the active locale; flow *content* stays bilingual data (`{en,zh}`) rendered by the Designer's language toggle, independent of UI locale. Translation files `web/src/locales/{en,zh}/*.json`; missing-key CI check.

## 6. Build & embedding

`web/` builds to `web/dist` → `go:embed` in `internal/httpapi` (SPA fallback serving, immutable asset caching). Dev: Vite on :5173 proxies `/api` → :8080, so the browser always sees one origin and the session cookie and event stream behave exactly as they do in the embedded build.

**Dev topology behind the operator's Caddy** (`~/workspaces/github/proxy/Caddyfile`, outside this repo): `app.aicc.test` → Vite :5173, `api.aicc.test` → aicc :8080, `ws.aicc.test` → FreeSWITCH wss :7443 (the agent phone's registration path, 01 §3). The SPA keeps calling its own origin's `/api`, so no CORS or cross-origin cookie handling is needed — `api.aicc.test` exists for direct API access, not for the SPA. When the SPA is served over TLS, set `AICC_SECURE_COOKIES=true`. The extension's Allow Sites must include the SPA origin.
