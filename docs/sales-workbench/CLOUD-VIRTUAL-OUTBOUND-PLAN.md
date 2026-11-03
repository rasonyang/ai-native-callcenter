# Mainland China cloud-number outbound architecture (conditional)

Status: **architecture decision and procurement gate only**. No production trunk, real-customer phone numbers, call initiation, or real-call authorization is part of this change.

## Decision

Use **carrier-provided, verified business caller ID and an explicitly approved cloud telecom route**, not physical mobile phones, SIM banks, spoofed numbers, or randomly generated virtual numbers. Reuse this repository's **FreeSWITCH + Go outbound dialer + AI realtime provider** architecture where the licensed carrier actually permits SIP and this precise use case.

The customer ultimately receives a **real PSTN/mobile call**; “cloud/virtual dialing” describes the transport and implementation, not a simulated call.

## Mandatory business-use gate (before spending on a trunk)

Business under review: invitations to management-consulting courses / industry events for contacts who previously attended courses or interacted with the company. Such an invitation may be a commercial-marketing call; previous attendance alone does not mean consent.

Regulatory considerations, to be checked against the current business-specific facts by the company's counsel and the proposed basic telecom carrier:

- MIIT Notice 工信部信管〔2020〕81号 restricts the licensed call-center-business model from providing commercial-marketing outbound calls, and subjects agreed immediate callbacks/information consultations to stringent controls. The same Notice excludes some **own-customer consultation-only** operations from the business-license definition; that exception is **not** blanket permission for bulk marketing or AI campaigns. Official source: https://wap.miit.gov.cn/jgsj/xgj/scgl/art/2020/art_2530ce74ed1d4e1c899e032b1318e5ea.html
- Personal Information Protection Law requires appropriate processing basis, scope and notice; marketing by automated decision-making requires a convenient refusal mechanism. Official source: https://www.cac.gov.cn/2021-08/20/c_1631050028355286.htm
- AI-generated audio and synthetic-content labeling rules should be assessed for the exact service; default conversation design explicitly identifies AI at call start. Official source: https://www.cac.gov.cn/2025-03/14/c_1743654684782215.htm
- If any mainland customer's PII, audio, transcript, or metadata leaves China, evaluate the PIPL outbound-transfer obligations and applicable 2024/2026 CAC implementing rules. Official source: https://www.cac.gov.cn/2024-03/22/c_1712776611775634.htm

**Approval conditions:** a written legal classification of the *specific activity*, confirmation of the company's regulatory role (own-use vs regulated call-center operator), documented subscriber permissions, and written acceptance of the described use case and technical connection method by the carrier. Without all approvals, do not connect a PSTN outbound gateway.

## Technical sequence (only after the approval gate)

```text
Sales workspace
  -> campaign list import (E.164 +86; proof reference)
  -> contact eligibility API (purpose + consent + DNC + time/frequency + approved trunk)
  -> dialer originator
  -> FreeSWITCH
  -> licensed mainland carrier SIP trunk / approved virtual business caller ID
  -> customer's +86 mobile number (a real telephone call)
  -> on answer: existing AI realtime voice provider
  -> recording/transcription + disposition/lead intent + manual callback queue
```

A pure “voice-notification API” that plays a prerecorded or synthesized clip after answer **does not equal a bidirectional conversational SIP trunk**. Verify two-way live media, caller-ID authentication, codec, SIP/network conditions, webhooks, consent purpose and recordings before selecting a vendor. Do not assume a provider's product page advertising a feature means the specific marketing/AI use is authorized.

## Outbound controls (server enforced, no bypass via UI/API keys)

Required additions, OpenAPI contract first, before touching production dialing:
- Contact consent/appointment evidence record: contactId, asserted phone, provenance/source, eventId, consent purpose/scope, channel, exact consent wording/version, occurredAt, expiresAt, withdrawalAt, proofRef and reviewer.
- Independent `contactEligibility` query and a **mandatory originate-time** decision across *all* dialing endpoints; deny by default.
- Campaign business-purpose classification and a carrier approval record; a generic campaign name is not authorization.
- Global DNC suppression and revocation propagation across all campaigns; immediate stop-on-refusal semantics.
- Strict local timezone schedule, capped attempts, explicit operator kill switch, caller ID verification, audit of every allowed and denied attempt.
- AI identity announcement at call start and recording/AI disclosure reviewed by counsel.
- Role-restricted recording/transcript storage, PII minimization, retention and deletion.
- Explicit separation of consented informational callbacks from promotional calls.

Existing repository code supplies call campaigns, CSV/XLSX import, dedupe, DNC, retries/windows and lead-intent tags, but does **not yet prove** mandatory evidence-backed consent eligibility before originate. Treat this as a release blocker, not a cosmetic form field.

## Operational procurement checklist

Send **this exact business description** to a mainland licensed operator or authorized business-cloud telecom supplier:

1. The enterprise does *management consulting/course and event invitations*, and has historical customer relationships. Does the proposed service permit that specific activity, under which licensing/own-use classification, and in which geographies?
2. Is machine-generated two-way interactive voice allowed? Are SIP trunks to customer-managed FreeSWITCH permitted, or only a provider-hosted managed solution?
3. What registered caller ID and legal telecom number resource will display? What permits, filings, caller-ID verification and carrier routing controls apply?
4. Which proof of customer opt-in or requested callback is mandatory? What are prescribed hours, concurrency, retries and do-not-call processes?
5. Does the provider allow a **test-only** employee-controlled number before production? Can it restrict its trunk to an allowlist?
6. Where are call recordings and voice/AI processing done? Which privacy, cross-border, retention and security terms apply?
7. What is the full cost schedule (number rental, concurrent SIP channels, connected minutes, setup, recording, AI audio usage, taxes)?

Reject any supplier proposing SIM banks, caller-ID spoofing, unregistered number pools, personal phones, or hidden marketing routes.

## Test progression and exit criteria

**T0: offline** - Synthetic contacts and synthetic SIP/media only; prove all disallowed contacts are blocked, DNC/revocation are sticky, no calls outside time windows, and no carrier configured. Build/test, import, QA, UI.

**T1: controlled line test** - Only after written legal/operator approval; one employee/test-partner mobile explicitly consented, approved caller ID and allowlist at the carrier. One call, low concurrency, recorded end-to-end outcome, immediate stop capability.

**T2: limited approved service** - Only the approved *non-prohibited* business purpose, small consented group, no batch promotion beyond permission. Monitor DNC, QA, billing and incident response; ensure live customers can reach a human or request no more calls.

**T3: production** - Security hardening, China data-transfer assessment, separate prod DB with no demo seed, signed DPA and carrier contract, backup/retention and legal review of the final script.

If marketing invitations are not permitted over this channel, **do not switch to a disguised “callback” purpose**. Keep the CRM, AI script simulation, appointment scheduling and genuine customer-initiated consultation support, using lawful alternative acquisition methods for sales invitations.

## Hosting

For preview, the previously prepared Singapore VPS setup can show *synthetic* UI data. For mainland customer phone numbers, recordings and transcripts, assess **mainland-hosted storage/processing** first. Do not put live customer PII on the overseas demo VPS merely because it is technically available.

## Current status

- Planned architecture only; no actual trunk selection, API credential or phone number imported.
- Original code is open-source; all deployment/calling tests must be executed before any public rollout.
- Relevant code: `internal/dialer/`, `internal/campaign/`, `deploy/carrier.md`, `web/src/components/campaign-import.tsx`.
