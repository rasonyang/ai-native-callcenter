# SPDX-License-Identifier: Apache-2.0
"""The scope map: operation -> scopes, and the derivation of role -> scopes.

This file is a *record of a derivation*, not runtime code. It generated the
contract's security blocks once; the contract has been the source of truth ever
since. It stays in the repository because role -> scopes was not decided by
opinion — it was derived mechanically from who could reach what before scopes
existed — and a derivation is worth nothing unless it can be re-checked and
re-run.

The load-bearing fact: a role guard was a *lower bound*, not a match
(auth.Role.AtLeast compares rank), so requireAgentRole turned nobody away who
held a valid session. The input to the derivation is therefore reachability,
not the guard's label. A role -> scopes map built this way is behaviour-
preserving by construction, and every departure from it is enumerated by the
self-check below.

    python3 docs/auth/scopemap.py     # prints the vocabulary, role -> scopes,
                                      # and every widening and narrowing
"""

# The vocabulary: name -> description, as it appears in the contract x-scopes.
VOCAB = {
    "calls:read:own":   "Read the live calls this subject is a party to. For a key, that is the calls of the agent named in X-AICC-Agent-ID.",
    "calls:read:all":   "Read every live call on the floor, and receive every call's events on the stream. Widens calls:read:own rather than replacing it.",
    "calls:control":    "Drive a call: answer, hold, retrieve, mute, transfer, DTMF, business data, hang up, and the callbacks that promise a call.",
    "calls:create":     "Place a call.",
    "calls:create:ai":  "Start the bot on a number: originate the customer leg and hand whoever answers to the flow published behind the DID. Required in addition to calls:create for kind=AI_OUTBOUND, because one operation carries one scope and this route serves two kinds of call with two different answers — click-to-dial is an agent's own work, starting a bot on a number is an operations decision.",
    "calls:monitor":    "Listen in on, whisper to or barge into a call in progress. Separate from calls:control because it reaches a conversation the subject is not a party to.",
    "agent:read":       "Read this subject's own agent presence and the wrap-up vocabulary.",
    "agent:act":        "Drive this subject's own agent presence: sign in and out, ready, not ready, wrap up.",
    "agent:manage":     "Supervise other agents: read the roster and force one out. Separate from agent:act, which only ever reaches the subject's own agent identity.",
    "contacts:read":    "Read the customer record book.",
    "contacts:write":   "Add, edit and remove customer records.",
    "history:read:own": "Read what happened on this subject's own calls: CDRs, recordings, transcripts and their own day's numbers.",
    "history:read:all": "Read what happened on every call, whoever took it. Widens history:read:own rather than replacing it.",
    "reports:read":     "Read the floor's aggregates: overview, per-queue and daily.",
    "quality:review":   "Read and write quality reviews of recorded calls.",
    "config:read":      "Read the platform's configuration: extensions, queues, numbers, flows, webhook subscriptions, accounts and system health.",
    "config:write":     "Change the platform's configuration, and reveal an extension's SIP password.",
    "users:write":      "Create and edit accounts, set roles and reset passwords. Separate from config:write because it is the one path to privilege.",
    "keys:manage":      "Create, disable and revoke API keys.",
    "audit:read":       "Read the audit trail.",
}

# operationId -> (scopes, the rank floor that could reach it before scopes,
#                 whether it has a bearer alternative)
# The floor is a lower bound, not a match: AGENT means anyone holding a session,
# SUPERVISOR and ADMIN likewise, NONE means any authenticated caller.
A, S, D, N, ANON = "AGENT", "SUPERVISOR", "ADMIN", "NONE", "ANON"

OPS = {
    # Authentication itself.
    "login":                    ([], ANON, False),   # anonymous
    "logout":                   ([], N, False),      # a session mechanism: a key has no session to end
    "getMe":                    ([], N, True),       # the bearer alternative answers with the key as subject

    # Agents.
    "getAgentPresence":         (["agent:read"], A, True),
    "getAgentWrapUp":           (["agent:read"], A, True),
    "listDispositions":         (["agent:read"], N, True),
    "listAgents":               (["agent:manage"], S, True),
    "agentLogin":               (["agent:act"], A, True),
    "agentLogout":              (["agent:act"], A, True),
    "agentReady":               (["agent:act"], A, True),
    "agentNotReady":            (["agent:act"], A, True),
    "agentWrapUp":              (["agent:act"], A, True),
    "forceLogoutAgent":         (["agent:manage"], S, True),

    # Live calls.
    "listMyCalls":              (["calls:read:own"], A, True),
    "listWaitingCalls":         (["calls:read:own"], A, True),
    "streamEvents":             (["calls:read:own"], N, True),
    "listCalls":                (["calls:read:all"], S, True),
    "answerCall":               (["calls:control"], A, True),
    "holdCall":                 (["calls:control"], A, True),
    "retrieveCall":             (["calls:control"], A, True),
    "muteCall":                 (["calls:control"], A, True),
    "unmuteCall":               (["calls:control"], A, True),
    "transferCall":             (["calls:control"], A, True),
    "sendCallDTMF":             (["calls:control"], A, True),
    "patchUserData":            (["calls:control"], A, True),
    "hangupCall":               (["calls:control"], N, True),
    # AI_OUTBOUND additionally requires calls:create:ai, and that half of the
    # check lives in the handler: one operation carries one scope, and this
    # route serves two kinds of call. The first draft recorded the floor as N,
    # which missed the in-handler decision — createAICall asked for SUPERVISOR
    # on the baseline, and this script only reads the routing table's guards,
    # so it could not see it. (Owner ruling 2026-08-31, adding the 20th scope.)
    "createCall":               (["calls:create"], N, True),
    "monitorCall":              (["calls:monitor"], S, True),

    # Callbacks: work on a call that has not happened yet.
    "listCallbacks":            (["calls:read:own"], N, True),
    "claimCallback":            (["calls:control"], N, True),
    "releaseCallback":          (["calls:control"], N, True),
    "completeCallback":         (["calls:control"], N, True),

    # History.
    "listMyCDRs":               (["history:read:own"], A, True),
    "getMyDay":                 (["history:read:own"], A, True),
    "getCallTranscript":        (["history:read:own"], N, True),
    "listCallRecordings":       (["history:read:own"], N, True),
    "getRecordingAudio":        (["history:read:own"], N, True),
    "listCDRs":                 (["history:read:all"], S, True),
    "getCDR":                   (["history:read:all"], S, True),
    "listCallQueueEvents":      (["history:read:all"], S, True),
    "getReportOverview":        (["reports:read"], S, True),
    "getReportQueues":          (["reports:read"], S, True),
    "getReportDaily":           (["reports:read"], S, True),

    # Quality review.
    "listCallReviews":          (["quality:review"], S, True),
    "createRecordingReview":    (["quality:review"], S, False),  # P8: a human judgement is attributed to a human

    # Contacts.
    "listContacts":             (["contacts:read"], N, True),
    "createContact":            (["contacts:write"], N, True),
    "updateContact":            (["contacts:write"], N, True),
    "deleteContact":            (["contacts:write"], N, True),

    # Configuration: reading.
    "listQueues":               (["config:read"], S, True),
    "listQueueAgents":          (["config:read"], S, True),
    "listExtensions":           (["config:read"], D, True),
    "listDIDs":                 (["config:read"], D, True),
    "listFlows":                (["config:read"], D, True),
    "getFlow":                  (["config:read"], D, True),
    "listUsers":                (["config:read"], D, True),
    "getSystemHealth":          (["config:read"], D, True),
    "listWebhookSubscriptions": (["config:read"], D, True),
    "getWebhookSubscription":   (["config:read"], D, True),
    "listWebhookDeliveries":    (["config:read"], D, True),

    # Configuration: writing.
    "createExtension":          (["config:write"], D, True),
    "updateExtension":          (["config:write"], D, True),
    "deleteExtension":          (["config:write"], D, True),
    "revealExtensionPassword":  (["config:write"], D, True),
    "createQueue":              (["config:write"], D, True),
    "updateQueue":              (["config:write"], D, True),
    "deleteQueue":              (["config:write"], D, True),
    "staffQueue":               (["config:write"], D, True),
    "unstaffQueue":             (["config:write"], D, True),
    "createDID":                (["config:write"], D, True),
    "updateDID":                (["config:write"], D, True),
    "deleteDID":                (["config:write"], D, True),
    "createFlow":               (["config:write"], D, True),
    "updateFlowDraft":          (["config:write"], D, True),
    "publishFlow":              (["config:write"], D, True),
    "createAgent":              (["config:write"], D, True),
    "updateAgent":              (["config:write"], D, True),
    "deleteAgent":              (["config:write"], D, True),
    "createWebhookSubscription":(["config:write"], D, True),   # P5 lifted
    "updateWebhookSubscription":(["config:write"], D, True),
    "deleteWebhookSubscription":(["config:write"], D, True),

    # Accounts.
    "createUser":               (["users:write"], D, True),
    "updateUser":               (["users:write"], D, True),
    "resetUserPassword":        (["users:write"], D, True),

    # Audit.
    "listAuditLogs":            (["audit:read"], D, True),

    # New: the contract serves itself.
    "getOpenAPI":               ([], ANON, False),

    # New: key management.
    "listAPIKeys":              (["keys:manage"], D, True),
    "createAPIKey":             (["keys:manage"], D, True),
    "getAPIKey":                (["keys:manage"], D, True),
    "updateAPIKey":             (["keys:manage"], D, True),
    "revokeAPIKey":             (["keys:manage"], D, True),
}

# Authorization decisions made inside a handler, in the same shape as OPS.
#
# One operation carries one scope, but some routes serve two things with two
# different answers. Those decisions are not on the routing table, so this
# script's self-check *cannot see them* — which is how the SUPERVISOR check in
# createAICall was missed on 2026-08-31, nearly handing every agent the ability
# to launch outbound bot campaigns. They are listed here so the derivation
# includes them, and so that the next person adding one of these asks first
# whether it needs a line here too.
#
# The key is written "operationId(condition)" because it is not an operation:
# the script counts these separately when it prints cardinalities.
HANDLER_CHECKS = {
    "createCall(kind=AI_OUTBOUND)": (["calls:create", "calls:create:ai"], S, True),
}

RANK = {ANON: 0, N: 1, A: 1, S: 2, D: 3}


def all_checks():
    """OPS and HANDLER_CHECKS together are every authorization decision made."""
    merged = dict(OPS)
    merged.update(HANDLER_CHECKS)
    return merged


def role_scopes():
    """A role's grant is the union of the scopes of every decision it could reach."""
    out = {}
    for role, r in (("AGENT", 1), ("SUPERVISOR", 2), ("ADMIN", 3)):
        s = set()
        for op, (scopes, floor, _) in all_checks().items():
            if floor == ANON:
                continue
            if RANK[floor] <= r:
                s.update(scopes)
        out[role] = sorted(s)
    return out

def widenings():
    """(operation, role) pairs newly reachable. Every one must have been ruled on."""
    g, out = role_scopes(), []
    for op, (sc, floor, _) in sorted(all_checks().items()):
        if floor == ANON or not sc:
            continue
        for role, r in (("AGENT", 1), ("SUPERVISOR", 2), ("ADMIN", 3)):
            if r < RANK[floor] and set(sc) <= set(g[role]):
                out.append((op, floor, role, sc))
    return out


def narrowings():
    """Pairs that stop being reachable. Must be empty: that is a regression, not a design."""
    g, out = role_scopes(), []
    for op, (sc, floor, _) in sorted(all_checks().items()):
        if floor == ANON:
            continue
        for role, r in (("AGENT", 1), ("SUPERVISOR", 2), ("ADMIN", 3)):
            if r >= RANK[floor] and not set(sc) <= set(g[role]):
                out.append((op, floor, role))
    return out


if __name__ == "__main__":
    unknown = set(s for sc, _, _ in all_checks().values() for s in sc) - set(VOCAB)
    assert not unknown, f"scopes outside the vocabulary: {unknown}"
    print(f"{len(VOCAB)} scopes, {len(OPS)} operations, "
          f"{len(HANDLER_CHECKS)} in-handler decisions\n")
    for role, sc in role_scopes().items():
        print(f"{role} ({len(sc)}): {' '.join(sc)}")

    print("\nWidenings (ruled on: all of them, and only config:read — baseline ruling 8, revision 2):")
    for op, floor, role, sc in widenings():
        print(f"  {op:26} floor={floor:10} {role} holds {sc}")
    assert all(sc == ["config:read"] for *_, sc in widenings()), "a widening nobody ruled on"

    print(f"\nNarrowings: {narrowings() or 'none'}")
    assert not narrowings(), "a role lost something it could reach before"
    print("\nSelf-check passed.")
