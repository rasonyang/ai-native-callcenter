# Mainland China customer follow-up: compliance gates and test plan

**Status:** design and acceptance plan only. **No real outbound calls are authorized.**
**Audience:** a small consulting-training sales team whose contacts previously attended a course, event, or interacted with the business.
**Scope:** China mainland (+86) telephone numbers; synthetic test data first.

## Regulatory baseline (review with counsel and the intended carrier)

1. MIIT, *Notice on Strengthening Call Center Business Management* (工信部信管〔2020〕81号): call-center business must not provide commercial marketing outbound calling services. Subscriber-agreed immediate callbacks and information consultation have distinct restrictions and carrier-side controls. Previous attendance is not a blanket exception.
   - https://www.miit.gov.cn/jgsj/xgj/wjfb/art/2020/art_5252be6730fa47ebbfd726c58648e733.html
2. PRC Personal Information Protection Law: use a lawful basis for contact data; informed consent where applicable must be voluntary, specific, recorded and revocable; support objection to automated marketing; use vendor contracts and risk assessment as appropriate.
   - https://www.cac.gov.cn/2021-08/20/c_1631050028355286.htm
3. Deep synthesis and 2025 AI-generated content labeling requirements: assess applicability to realtime synthetic voice and recordings; the safest UX is to identify the AI assistant clearly at the beginning.
   - https://www.cac.gov.cn/2025-03/14/c_1743654684782215.htm
4. If model, recording, CRM, or hosting infrastructure is overseas, complete data-flow mapping and assess mainland-to-overseas personal-information transfer requirements before using real records.

**Important:** marketing opt-in does not make a prohibited type of call-center commercial marketing outbound activity permissible. Counsel and the selected carrier must approve the **specific** business use case and access arrangement in writing.

## Segmentation and routing

| Segment | Evidence | Default action |
|---|---|---|
| A. Customer requested an information callback | Timestamped request with subject, purpose, phone and channel; carrier approves callback use | Only scoped callback after review and consent validation; never auto-activate on import |
| B. Existing course attendee with generic marketing opt-in | Original opt-in evidence, covering business and channel | Hold for legal and carrier channel review; do not presume bulk AI calling is allowed |
| C. Attendee or historic contact, no verifiable call permission | Previous class attendance/CRM note only | No AI outbound; use otherwise lawful client-initiated or other permitted contact workflow |
| D. Revoked consent, refused, complained, DNC | Refusal/opt-out, irrespective of source | Global suppression; no future auto or manual campaign attempts from the system |

Segment classification is not inferred from a model or contact tag alone. Approval is evidence-based and owned by a person.

## Existing implementation observed

- Import from CSV/XLSX, validation and deduplication: `web/src/components/campaign-import.tsx`, `internal/campaign/contacts.go`.
- Global do-not-call and lead intent `DO_NOT_CALL`: `internal/campaign/settle.go`, `internal/dialer/admit.go`.
- Campaign-specific calling window and outer hours: `internal/campaign/window.go`, `schedule.go`.
- Configurable retry controls, capacity, pause/breaker: `internal/campaign/settle.go`, `internal/dialer/admit.go`.
- Carrier gateway separation: `deploy/carrier.md`, config key `AICC_OUTBOUND_ENDPOINT`.
- Current sales manager homepage `web/src/routes/_app.admin.index.tsx` provides dashboard views; it is not a compliance console.

## Missing or not yet verified: release blockers

- Immutable per-contact evidence of opt-in / requested callback, purpose, authorization channel, collector, timestamp, consent version, scope, expiration, revocation and proof reference.
- Backend enforcement (not only UI filtering) that blocks dialing without a qualifying, currently valid and business-purpose-matched authorization **and** a carrier-approved line/use case.
- Revocation or DNC enforced across campaigns, queues and all callers, including after import and immediately before originate, with concurrency/race tests.
- AI disclosure at call start; recording notification and refusal handling reviewed for applicable law.
- Auditability of who approved the outreach purpose, what evidence was reviewed, and what dialer rule admitted the call.
- Privacy impact assessment, vendor processing and outbound data transfers reviewed before live customer data, especially if hosted in Singapore.
- Secure recording/transcript retention, least-privilege access and deletion workflows.
- Evidence-based calling window, attempt caps and incident kill switch with written carrier limits.
- Defined separation between information callbacks and course/event sales invitations.

## Synthetic test matrix (no real network calls)

Use entirely synthetic contacts and a simulated carrier. Verify:

1. Missing consent evidence => outbound cannot be activated, even when campaign status is RUNNING.
2. Invalid, expired or wrong-purpose consent => no originate, with a recorded reason.
3. Explicitly revoked consent => all pending retries become ineligible before any next attempt.
4. Global DNC => blocked even when re-imported to another campaign.
5. 'Do not call again' recognized during a simulated conversation => prompt stops, persisted DNC, no redial.
6. Duplicate +86 contact rows => no duplicate attempts.
7. Off-hours and excluded days => no attempt until an approved local window.
8. Retry exhausted, carrier unavailable, and no `AICC_OUTBOUND_ENDPOINT` => no real call, safe pause.
9. AI identifies itself as an automated assistant in the first statement; never pretends to be a human or a Huawei employee unless legitimately authorized and true.
10. Transcript, recording and lead analysis access follow role policy; deletion/retention work as documented.
11. Structured report separates answered calls, marketing interest, opt-out, callback request, and confirmed appointment; no fabricated conversion.
12. 1 call at a time initially. Verify an emergency 'stop all outbound' action stops scheduling and in-flight handling as specified.

**Pass criterion:** zero unauthorized originates; zero DNC redials; zero calls outside reviewed windows; traceable gate decision for every attempted origination.

## Approved first real-line test (only after legal and provider approval)

- Use one number controlled by a consenting employee or test partner, not a commercial lead.
- Use a verified carrier-assigned caller ID; caller ID spoofing, forwarding services or hidden commercial trunk routes are not acceptable.
- Document the precise purpose, permitted test numbers, provider restrictions and cancellation path.
- Run one manually authorized call; verify SIP audio, AI disclosure, recording consent, DNC, callback record and CDR.
- Keep `AICC_OUTBOUND_ENDPOINT` unset on the demo environment until the entire approval checklist is signed.

## Carrier due-diligence questionnaire

Ask the **licensed mainland provider** for written replies:
1. Is AI-generated outbound voice permitted for the specifically described request/consultation callback? What about invitations to training courses?
2. Which business-license, contract, consent, numbers/recording evidence, and caller ID documents are required?
3. May the provider supply a lawful SIP trunk to a self-managed FreeSWITCH system? What registration or whitelisting is required?
4. Which prefixes/DIDs, outbound uses, geography, time limits, concurrency, retries and anti-harassment measures are permitted?
5. Where are audio and metadata hosted? What processing, retention, incident response and audit terms apply?
6. Are test-only calls to company-owned numbers supported before production activation?
7. What features are categorically prohibited and what is the written service agreement governing the use case?

Never select a provider whose sales materials claim it can bypass supervision, alter caller ID, or use personal mobile SIM pools for bulk calling.

## Implementation plan, gated

**Phase 0:** counsel/carrier use-case classification. Finish synthetic script and analytics with no carrier configured.

**Phase 1:** OpenAPI-first contract additions for authorization evidence, eligibility reason, revocation and audit; DB migrations only after reviewed schema. All server originate paths call the same gate.

**Phase 2:** UI for classification/approval and consent evidence, not merely an import checkbox; internal simulated test suite and QA.

**Phase 3:** if written approval, separately configured trunk, one manually approved controlled test call, cost and retention monitoring.

**Phase 4:** production hardening with China-local data strategy, carrier agreement, handling scripts, monitoring and incident procedure. If the proposed commercial invite is not permitted, do not enable it; keep the platform as CRM, manual follow-up and customer-requested consultation support.

The existing GitHub fork is a development artifact, not a live deployment, and this plan does not change runtime dialing behavior.
