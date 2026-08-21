# VC-S6-01 — 排队中主叫放弃 · 判定:**PASS**

执行 2026-08-21 15:39 与 15:46(两通;首通因抓流窗口过期只缺 SSE 一条,第二通补齐)。
前置:wei 置 `NOT_READY/BREAK`,交换机侧实证 `agent-wei status=On Break`,
support-en 队列无可用坐席(uiagent 为 Available 但不在该队列 tier)。

主呼叫 `01a02348-f8cd-7084-bcf5-06c344a2f86d`:18688886669 → 95001 → bot 转人工 →
保持音中等待 → 主叫挂断。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| SSE:`QUEUE_LEFT` 1 条,`payload.cause="Cancel"`、`waitSec≈10` | 1 条,`cause="Cancel"`、`cancelReason="BREAK_OUT"`、**`waitSec=14`** | ✓ |
| SSE:`QUEUE_COUNT`(waiting=0) | 末条 `{"queueName":"support-en","waiting":0}` | ✓ |
| queue_events:JOINED 后 ABANDONED,`wait_ms` 8000–15000 | `JOINED\|0` → `ABANDONED\|14000` | ✓ |
| cdrs:`NO_ANSWER` / `ABANDONED_WAITING` / `queue_wait_sec` 8–15 | `NO_ANSWER\|ABANDONED_WAITING\|13` | ✓ |
| `/calls/waiting`:items 长度=0(无幽灵等待者) | `count=0` | ✓ |

`failure_looks_like`(主叫挂断后等待名单仍挂着一个已离开的人)**未发生**。

首通 `01a02343` 的三条非 SSE 断言亦全部通过(`ABANDONED\|13000`、`NO_ANSWER\|ABANDONED_WAITING\|12`、
`count=0`),两通互为佐证。

## collect 缺陷(回修建议)

collect 第 6 条用主管会话取 `/calls/waiting`,**永远取不到**:

```
supervisor http=403 {"error":{"code":"FORBIDDEN","message":"this account is not an agent"}}
wei        http=200 {"count":0}
```

这正是既有立案 **C12**(`/calls/waiting` 拒绝主管)。在 C12 修复前,该条 collect 必须改用坐席会话:

```diff
-      - "curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq '.items | length'"
+      # C12:该端点拒绝主管(403 FORBIDDEN "this account is not an agent"),
+      # 修复前只能用坐席会话取。
+      - "curl -s -b /tmp/vc-wei.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq '.items | length'"
```

## 顺带:新口径(plan-cdr-anchors)的第二、三份实证

```
status         NO_ANSWER              ← 运营:没人接到
missed_reason  ABANDONED_WAITING
answered       t                      ← 对账:交换机确实应答了
bill_sec       28    total_sec 28
tech           {"switchBillSec": 29}  ← 差 1 秒,在 ±2 容差内,未告警(实证)
ring_sec       0                      ← 本通 wei 全程 On Break,无派单腿,故为 0(queue_events 无 OFFERED)
talk_sec       0
bot_sec        14                     ← 旧印记 9,bridge 区间 14,WARN 已触发
```

**同一行上"这通电话没接到"与"这通电话要付钱"第一次各自成立、互不干扰** ——
这正是本次改动的目的。首通 `01a02343` 更进一步:`bill_sec=30` / `tech.switchBillSec=30` /
ESL 探针独立抓到的 `variable_billsec=30`,**三方零差**。

`bot_sec` 的 WARN 每通都触发(印记 9 vs 区间 14),差值稳定 5 秒 —— 即 CDR 设计审查①记录的
"bot 最后一句话不在任何一列里",现已计入。
