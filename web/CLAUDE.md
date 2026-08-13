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

## Topbar breadcrumb (all pages)
- Pattern: Role / Section / Detail — e.g. "Admin / Bot Flows / early_collections".
- Every segment except the last is a LINK with a FIXED target (never history
  back): Role → the role index route (/agent, /supervisor, /admin);
  Section → its sidebar-nav route (e.g. Bot Flows → /admin/bots).
- The last segment is the current location: font-medium, never a link.
  Detail segments show the entity id/name verbatim.
- Interaction must distinguish clickable from non-clickable:
  clickable segments = text secondary at rest, accent on hover;
  current segment = text primary + medium, no hover change;
  "/" separators = muted at 50% (weaker than both).
- The breadcrumb derives from the sidebar nav config (src/lib/nav.ts);
  every new page must be reachable from a nav item so the breadcrumb
  resolves correctly.
