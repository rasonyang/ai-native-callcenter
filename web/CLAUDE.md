# Design System — NON-NEGOTIABLE. Read before writing any UI code.

## Aesthetic reference
This app must look like a product from the Linear / Vercel / Stripe family:
quiet, dense, precise, professional. It is an operations tool used 8 hours a
day, NOT a marketing site.

## Tokens (define once in globals.css as CSS variables; never invent new values)
- Background:        #FAFAFA (app) / #FFFFFF (surfaces/cards)
- Border:            #E5E7EB (1px solid; borders instead of shadows everywhere)
- Text primary:      #18181B
- Text secondary:    #71717A
- Accent (single):   #4F46E5 — used ONLY for primary actions, active nav, focus rings
- Semantic (call states only):
  - Available #16A34A · On-call #4F46E5 · Ringing #F59E0B
  - ACW/Wrap-up #8B5CF6 · AUX/Break #71717A · Offline #D4D4D8 · SLA-breach #DC2626
- Radius: 6px (cards, inputs, buttons), 9999px (status pills only)
- Shadow: none, except dropdowns/modals (shadow-md)
- Font: Inter. Sizes allowed: 12 / 13 / 14 / 16 / 20 px ONLY. Base = 13px.
  All numbers (timers, KPIs, phone numbers) use tabular-nums.
- Spacing: 4px grid. Page padding 24px. Card padding 16px. Gap between cards 16px.

## Density rules (this is an ops console — dense by default)
- Table row height 36px, cell text 13px, header 12px uppercase tracking-wide text-secondary
- Sidebar width 220px, nav item height 32px, icon 16px
- KPI stat cards: label 12px secondary on top, value 24px semibold below, delta 12px
- Buttons: h-8 (32px) default, h-9 for primary page actions only

## Never do (hard bans)
- No gradients, no emoji in UI, no hero sections, no marketing copy
- No colored card backgrounds; color appears only in pills, dots, and small accents
- No shadows on cards, no centered text in tables, no skeleton rainbow palettes
- No more than ONE accent color; never use accent for decoration
- No 16px+ body text, no airy landing-page spacing

## Consistency rule
After the first page is approved, every new page MUST reuse its exact patterns:
same page-header component, same table component, same card component. Never
re-implement a variant.

## Navigation is partitioned by role, not ranked
- `src/lib/nav.ts` gives every item the exact set of roles it belongs to
  (`roles: Role[]`), never a floor. An administrator configures the platform,
  a supervisor watches the floor, an agent takes calls — the sidebar is a job
  description, not a seniority ladder.
- ADMIN: Overview, Agents & Phones, Extensions, Queues & Routing, Numbers,
  Bot Flows / CDR, Reports, Audit Log.
  SUPERVISOR: Wallboard, Agents, Queues / CDR, Reports.
  AGENT: Dashboard, My Calls, Contacts, Callbacks.
  CDR and Reports are the only overlap, and only between the two senior roles.
- The route guard uses the same sets (`requireRole(user, ...roles)`): a page
  hidden from the menu but reachable by URL is half a rule. A refused visitor
  is sent to their own `roleHomeFor(role)` — never towards the door that just
  closed, which is how a redirect loop starts.
- `src/lib/nav.test.ts` pins all three menus literally. Adding a page means
  adding it there too.

## Topbar breadcrumb (all pages)
- Pattern: Group / Section / Detail — e.g. "Manage / Bot Flows / novanet_support".
- The first segment is the **sidebar group** (Workspace, Supervise, Manage,
  System), which is the word the reader just clicked, not their role. A role
  name there claims something the page is in no position to claim, and on a
  screen two roles share (the ledger) it claims it wrongly.
- Every segment except the last is a LINK with a FIXED target (never history
  back): Group → the first page in it **this reader may open** (a group has no
  page of its own, and the answer differs by role — never link somewhere they
  would be bounced from); Section → its sidebar-nav route (Bot Flows →
  /admin/bots), so a detail page is always one click from its list.
- The Detail segment is the record's own name or id, supplied by the page
  through `useNameThisPage()` (`src/lib/breadcrumb.tsx`) — it cannot be derived
  from a path that carries a uuid. A page still loading passes undefined and
  the trail simply stops at the section.
- The last segment is the current location: font-medium, never a link.
  Detail segments show the entity id/name verbatim.
- Interaction must distinguish clickable from non-clickable:
  clickable segments = text secondary at rest, accent on hover;
  current segment = text primary + medium, no hover change;
  "/" separators = muted at 50% (weaker than both).
- The breadcrumb derives from the sidebar nav config (src/lib/nav.ts);
  every new page must be reachable from a nav item so the breadcrumb
  resolves correctly.
