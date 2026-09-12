# Page ↔ web-sip-phone protocol, version 1

Date: 2026-09-12.

The agent's phone is the [web-sip-phone](https://github.com/rasonyang/web-sip-phone)
Chrome extension, and nobody configures it. This document is the contract
between the SPA served by this platform and that extension: how a page hands
the extension the credentials the platform minted, and how the extension
reports what its registration is doing. The extension lives in its own
repository and is not edited from this one; this file states what the page
sends, what it expects back, and why each requirement exists.

Nothing here is call control. The extension's own `design.md` §20 rules out
page-driven dialling, answering and hangup, and this protocol keeps that rule:
it adds credential provisioning and read-only status, and no message in either
direction starts, answers or ends a call. Call control stays where design
[01 §3](design/01-telephony.md) puts it — FreeSWITCH is the authority and the
platform drives it over ESL (`uuid_phone_event talk|hold`, auto-answer
origination), while the SPA renders what the event stream reports.

## 1. Transport

`window.postMessage`, in the same window and the same origin only.

- The page posts with `targetOrigin = window.location.origin` and never `"*"`.
- Both sides drop a message whose `event.source !== window` or whose
  `event.origin !== window.location.origin`. A credential that may be read by
  a frame the page did not put there is not a credential.
- Every message carries `source` and `protocolVersion: 1`. A receiver ignores
  a message with a `source` it does not know and a `protocolVersion` it does
  not implement, silently — a future version must be able to appear without
  breaking this one.

## 2. Presence marker

The extension's content script sets `document.documentElement.dataset.webSipPhone`
on every site it is allowed to run on. The page reads it to know whether the
extension is installed at all, and watches for it with a `MutationObserver` on
the root element's attributes, because the content script may be injected after
the page's own script has run and a single read at startup would report "not
installed" for an extension that is.

The marker answers *installed*; only a `state` message answers *working*.

## 3. Messages

### Page → extension, `source: "aicc"`

| Type | Fields | Meaning |
|---|---|---|
| `hello` | `nonce` | Is the extension there, and what does it speak. |
| `provision` | `nonce`, `sipDomain`, `wssUrl`, `account`, `a1Hash`, `expiresAt` | Register with these credentials. Replaces whatever the extension held. |
| `deprovision` | — | Forget the provisioned credentials and unregister. |

The five `provision` fields are the response body of `POST /agent/sip-session`,
passed through unchanged: `sipDomain` is the digest realm and the domain of the
address of record, `wssUrl` the WebSocket the phone connects to, `account` the
extension number, `a1Hash` the digest hash it authenticates with, and
`expiresAt` when the platform stops accepting it.

### Extension → page, `source: "web-sip-phone"`

| Type | Fields |
|---|---|
| `hello` | `nonce`, `extensionVersion`, `protocolVersion`, `extensionId` (optional) |
| `state` | `registration`, `account`, `sipDomain`, `credentialSource`, `microphone`, `error`, `provisionStatus` (optional) |

`nonce` is echoed from the `hello` that asked, so a page with more than one
outstanding question can tell the answers apart.

`extensionId` is the extension's own `chrome.runtime.id`. It is optional and
recommended: the page deep-links into `chrome-extension://<id>/options.html`
to send an agent to the site list and the microphone permission, and an
unpacked build has an id that no bundle could have carried — it differs per
machine. A page that guesses instead sends the agent to a
`chrome-extension://` origin Chrome blocks outright (`ERR_BLOCKED_BY_CLIENT`,
seen live on 2026-09-12). The page prefers the announced id, falls back to its
build-time `VITE_WEB_SIP_PHONE_ID`, and where neither exists renders no link
at all — it tells the agent to open the options page from
`chrome://extensions` instead, because a dead button teaches them the
instructions are wrong. An id is believed only when it looks like one: 32
letters in `a`-`p`.

| Field | Values |
|---|---|
| `registration` | `UNREGISTERED` \| `REGISTERING` \| `REGISTERED` \| `FAILED` |
| `credentialSource` | `NONE` \| `MANUAL` \| `PROVISIONED` |
| `microphone` | `UNKNOWN` \| `GRANTED` \| `DENIED` |
| `error` | `null` \| `REGISTRATION_FAILED` \| `WSS_LOST` \| `MIC_UNAVAILABLE` \| `MEDIA_FAILED` |
| `provisionStatus` | `ACTIVE` \| `OVERRIDDEN` \| `NONE` (optional) |

`account` and `sipDomain` are what the extension is currently registered as, so
a page can tell a registration it provisioned from one left over from a manual
configuration or another agent's session.

`provisionStatus` says what has become of the session the page provisioned,
and is absent on builds older than the field. `ACTIVE` is the held provision
being the one in use. `NONE` is nothing held. `OVERRIDDEN` is a manual account
saved in the extension's Options while a provision is still held: the manual
account is applied, the provisioned one is kept and dormant, and
`credentialSource` reads `MANUAL` with the manual account. **The page must not
provision under `OVERRIDDEN`.** A provision posted then is accepted and not
applied, while the `POST /agent/sip-session` behind it still replaces the
agent's session and flushes, server-side, the registration the manual account
is holding. The SPA shows "Manual override in extension" and links to the
Options page that set it, and offers no Re-provision — an override is undone
where it was made.

## 4. The sequence

1. A person signs in to the SPA.
2. The SPA calls `POST /agent/sip-session`. The platform generates a password
   for that agent's extension, stores `md5(account:sipDomain:password)`,
   discards the plaintext, replaces any session the agent already had and
   flushes the registration that one was holding.
3. The SPA posts `hello`, gets the extension's `hello` back, and posts
   `provision` with the response of step 2.
4. The extension registers over `wssUrl` and posts `state` with
   `registration: REGISTERED` and `credentialSource: PROVISIONED`.
5. FreeSWITCH emits `sofia::register`. The platform sets `isDeviceRegistered`
   and `deviceAccount` for the agent, and the change reaches every subscriber
   as an `AGENT_*` event (`SseAgentPresencePayload`).
6. The phone chip in the softphone bar shows ready, and `POST /agent/ready` is
   now something the platform will accept.

`DELETE /agent/sip-session` reverses it: the credentials stop being accepted,
the registration is flushed from the switch, and the page posts `deprovision`.

Signing out of the page does the same, with presence first. `POST /auth/logout`
signs the agent out of presence, then revokes the SIP session and flushes the
registration, then ends the web session. The order is not cosmetic: the flush
makes the switch emit `sofia::unregister`, and an agent who is READY when their
phone disappears is moved to `NOT_READY` with reason `DEVICE_LOST` — the right
story for a phone that died, and a misleading one for a person who pressed Sign
out. Ending presence first leaves that unregister with nobody READY to report
on. An agent who was already signed out of presence is in the state this asks
for, and the phone is revoked just the same.

Two failures are ordinary and both are visible. A registration lost while the
agent is READY moves them to `NOT_READY` with reason `DEVICE_LOST`, because an
agent with no phone cannot be sent a call. A second login for the same agent
elsewhere issues a new session, which displaces this one — the page says "phone
moved to another browser" rather than showing a registration that is quietly
no longer valid.

## 5. Security properties

- **The plaintext password never leaves the server, and is never stored.** It
  is generated, hashed into `a1Hash` and discarded inside the one request; no
  response, log or table holds it.
- **The hash is bound to the realm.** `a1Hash = md5(account:sipDomain:password)`
  is only usable against a challenge for `sipDomain`. The internal profile
  challenges with `challenge-realm=auto_from`, so the realm is exactly the
  domain the phone puts in its `From` header, which is the `sipDomain` it was
  issued.
- **It expires with the person.** `expiresAt` is the web session's expiry, so
  the phone is signed in for as long as the agent is and not a minute longer.
- **One active session per agent.** Issuing a session replaces the previous one
  and flushes its registration (`sofia profile <profile> flush_inbound_reg <account>@<domain>`), so
  a credential handed to a browser that is no longer in use stops working the
  moment a new one is issued.
- **The credential stays in the browser that was given it.** Same-window,
  same-origin `postMessage` is the only path; nothing is written to
  `localStorage`, a URL or a request to a third party.
- **The static extension password no longer authenticates a registration.**
  `extensions.password` and `GET /extensions/{extensionId}/password` remain for
  API compatibility; the switch accepts only an active session's a1-hash.

## 6. What the extension has to do, and why

| # | Requirement | Why |
|---|---|---|
| R1 | The content script sets the presence marker and answers `hello` on **every** site it is allowed to run on. | A page cannot detect an extension that only answers on some of them; a silent site is indistinguishable from a missing extension, and the onboarding card would tell a person to install what they already have. |
| R2 | `state` is sent once after `hello` and again on **every** change. | The page holds no timer and polls nothing. A state that is only sent on request means a chip that is right when the page loads and wrong for the rest of the shift. |
| R3 | `options.html` is a `web_accessible_resource`, together with the deep links `?site=<hostname>` — the Allow Sites section, pre-filled with that hostname — and `#microphone`, the microphone test. | Without it a page cannot link to the extension's own options at all, and the onboarding card's two remaining steps ("allow this site", "allow the microphone") become instructions to go and find a settings page. |
| R4 | A stable extension ID: a `key` in the manifest, or a Web Store listing. | The ID is part of every `chrome-extension://` deep link. Done 2026-09-12: the manifest carries the Web Store public key, so the store copy and an unpacked build share the id `dkhaojcfjdcdpldokeokajkmambkbacp`, which is the SPA's build constant; `VITE_WEB_SIP_PHONE_ID` overrides it for a differently keyed build. Chrome refuses to run the store copy and an unpacked copy side by side. |
| R5 | Provisioned credentials are not persisted beyond the session, and `PROVISIONED` wins over `MANUAL`. | The credential is session-bound by design (§5); an extension that wrote it to disk would outlive the expiry it was given. Preferring the provisioned one means an agent who once typed credentials in by hand still gets the phone the platform issued, without being asked to clear anything. The exception is deliberate and is reported: saving a manual account in Options while a provision is held applies the manual one and reports `provisionStatus: OVERRIDDEN`, which is how the page knows to leave it alone rather than mint over it. |
| R6 | Registration uses the a1-hash against realm `sipDomain`, taking the realm from what was provisioned rather than deriving it from the WebSocket URL. | The WebSocket host and the SIP domain are the same value in the default deployment and different ones behind a proxy; the hash only verifies against the realm it was computed for. |
| R7 | No call control arrives over this channel, and none is accepted. | The extension's `design.md` §20. Answer, hangup and hold stay the platform's, driven over ESL, and a page that could dial would be a second authority over the same phone. |

## 7. Versioning

`protocolVersion` is `1`. A change that adds a message type or an optional
field keeps the version; a change to the meaning of an existing field or the
removal of one raises it. Both sides ignore what they do not recognise, so an
extension speaking version 1 and a page speaking version 2 degrade to nothing
happening rather than to a half-provisioned phone.

## See also

- [design/01-telephony.md](design/01-telephony.md) §3 and §5 — the registration
  path and the Lua directory handler that serves the a1-hash.
- [design/05-frontend.md](design/05-frontend.md) §4 — the softphone bar and the
  onboarding card.
- [../deploy/README.md](../deploy/README.md) — what an operator does, including
  the Chrome policy path for a managed fleet.
