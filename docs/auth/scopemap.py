# SPDX-License-Identifier: Apache-2.0
"""§2 的 scope 映射 —— operation → scopes，以及 role → scopes 的推导。

这个文件是**推导的记录**，不是运行时代码：契约里的 89 个 security 块由它生成一次，
之后契约就是唯一真相源。留在仓库里是因为 role → scopes 不是拍脑袋定的，而是从
"今天按 rank 谁能到达"机械推出来的，而那个推导需要能被复查、被重跑。

关键事实：角色守卫是**下限**不是**匹配**（auth.Role.AtLeast 按 roleRank 比大小），
所以 requireAgentRole 不拒绝任何持有有效会话的人。推导的输入因此是可达性而不是
守卫标签。照这个构造出来的 role → scopes 按定义是行为保持的，每一处偏离都能被
下面的自检枚举出来。

    python3 docs/auth/scopemap.py     # 打印词表、role→scopes、拓宽与收窄
"""

# 词表：名 -> 说明（进契约的 x-scopes）
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

# operationId -> (scopes, 今天按 rank 的可达下限, 是否有 Bearer 支)
# 下限：AGENT = 任何有会话的人（AtLeast 是下限不是匹配）；SUP / ADMIN 同理；NONE = 任何已认证
A, S, D, N, ANON = "AGENT", "SUPERVISOR", "ADMIN", "NONE", "ANON"

OPS = {
    # 认证本身
    "login":                    ([], ANON, False),   # 匿名
    "logout":                   ([], N, False),      # 会话机制，Key 没有会话可结束
    "getMe":                    ([], N, True),       # Bearer 支返回 Key 主体

    # 坐席
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

    # 在线通话
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
    # AI_OUTBOUND 还要 calls:create:ai，那半条检查在 handler 里(一个 operation
    # 只能带一个 scope,而这条路由服务两种 kind)。初稿把下限记成 N 是漏了
    # handler 内的判定——createAICall 在基线上就要 SUPERVISOR,而本文件的
    # 自检只读路由表守卫,看不见它(owner 2026-08-31 裁定,补上第 20 个 scope)。
    "createCall":               (["calls:create"], N, True),
    "monitorCall":              (["calls:monitor"], S, True),

    # 回呼：还没发生的通话上的工作
    "listCallbacks":            (["calls:read:own"], N, True),
    "claimCallback":            (["calls:control"], N, True),
    "releaseCallback":          (["calls:control"], N, True),
    "completeCallback":         (["calls:control"], N, True),

    # 历史
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

    # 质检
    "listCallReviews":          (["quality:review"], S, True),
    "createRecordingReview":    (["quality:review"], S, False),  # P8：人的判断必须归属到人

    # 联系人
    "listContacts":             (["contacts:read"], N, True),
    "createContact":            (["contacts:write"], N, True),
    "updateContact":            (["contacts:write"], N, True),
    "deleteContact":            (["contacts:write"], N, True),

    # 配置——读
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

    # 配置——写
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
    "createWebhookSubscription":(["config:write"], D, True),   # P5 解除
    "updateWebhookSubscription":(["config:write"], D, True),
    "deleteWebhookSubscription":(["config:write"], D, True),

    # 账号
    "createUser":               (["users:write"], D, True),
    "updateUser":               (["users:write"], D, True),
    "resetUserPassword":        (["users:write"], D, True),

    # 审计
    "listAuditLogs":            (["audit:read"], D, True),

    # 新增：契约自己 serve 自己
    "getOpenAPI":               ([], ANON, False),

    # 新增：Key 管理
    "listAPIKeys":              (["keys:manage"], D, True),
    "createAPIKey":             (["keys:manage"], D, True),
    "getAPIKey":                (["keys:manage"], D, True),
    "updateAPIKey":             (["keys:manage"], D, True),
    "revokeAPIKey":             (["keys:manage"], D, True),
}

# handler 内部的授权判定,格式与 OPS 相同。
#
# 一个 operation 只能带一个 scope,但有的路由服务两件事、两个答案。这些
# 判定不在路由表上,所以本脚本的自检**看不见它们**——2026-08-31 就是这样
# 漏掉了 createAICall 的 SUPERVISOR 检查,差点把「发起外呼机器人」顺手发给
# 每个坐席。列在这里,是为了让推导包含它们,也为了下次有人加同类判定时,
# 第一反应是"要不要在这里也写一行"。
#
# 键写成 "operationId(条件)",因为它不是一个 operation——脚本打印基数时
# 与 OPS 分开计。
HANDLER_CHECKS = {
    "createCall(kind=AI_OUTBOUND)": (["calls:create", "calls:create:ai"], S, True),
}

RANK = {ANON: 0, N: 1, A: 1, S: 2, D: 3}


def all_checks():
    """OPS 与 HANDLER_CHECKS 合起来,才是这个产品实际做的全部授权判定。"""
    merged = dict(OPS)
    merged.update(HANDLER_CHECKS)
    return merged


def role_scopes():
    """R 的 grant = 所有 rank <= R 即可达的判定的 scope 之并。"""
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
    """今天够不着、改完却够得着的 (operation, 角色)。每一条都必须是被裁定过的。"""
    g, out = role_scopes(), []
    for op, (sc, floor, _) in sorted(all_checks().items()):
        if floor == ANON or not sc:
            continue
        for role, r in (("AGENT", 1), ("SUPERVISOR", 2), ("ADMIN", 3)):
            if r < RANK[floor] and set(sc) <= set(g[role]):
                out.append((op, floor, role, sc))
    return out


def narrowings():
    """今天够得着、改完却够不着的。必须为空——那是回归，不是设计。"""
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
    assert not unknown, f"词表外的 scope: {unknown}"
    print(f"词表 {len(VOCAB)} 个，operation {len(OPS)} 个，"
          f"handler 内判定 {len(HANDLER_CHECKS)} 个\n")
    for role, sc in role_scopes().items():
        print(f"{role} ({len(sc)}): {' '.join(sc)}")

    print("\n拓宽（已裁定：全部且仅为 config:read，baseline 裁定 8 修正 2）:")
    for op, floor, role, sc in widenings():
        print(f"  {op:26} 下限={floor:10} {role} 持有 {sc}")
    assert all(sc == ["config:read"] for *_, sc in widenings()), "出现了未裁定的拓宽"

    print(f"\n收窄: {narrowings() or '无'}")
    assert not narrowings(), "有角色丢了今天够得着的东西"
    print("\n自检通过。")
