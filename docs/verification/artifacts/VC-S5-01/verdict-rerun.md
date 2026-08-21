# VC-S5-01 — 重跑(2026-08-21 13:46,C22 修复后)· 判定:**PASS**

首跑同日 12:34 判 FAIL(见 `verdict.md`),立案 C22 并当日修复(commit `71ec949`)。
本次按原 collect 重跑,呼叫 `01a022da-67d0-7c7a-9095-b54ac0affcd1`:
18688886669 → 95001 → bot 转人工 → wei 的 1008 响 121 秒全程不接 → 主叫挂断。

## 可验半 —— 四条全中

| expect | 首跑 | 重跑 | |
|---|---|---|---|
| SSE:`QUEUE_AGENT_OFFERED` ≥1 且全程无 BRIDGED | 23,无 BRIDGED | **23,无 BRIDGED** | ✓ |
| queue_events:JOINED=1、OFFERED≥1、无 BRIDGED | JOINED=1 / OFFERED=23 / ABANDONED=1 | **同左** | ✓ |
| cdrs:`status=NO_ANSWER` | `ANSWERED` ✗ | **`NO_ANSWER`** | ✓ |
| cdrs:`missed_reason=ABANDONED_WAITING` | 空 ✗ | **`ABANDONED_WAITING`** | ✓ |
| wei 前后两次 `agent_states` 完全一致 | `READY\|` → `READY\|` | **`READY\|` → `READY\|`** | ✓ |

## C22 三处失真的复验

```
                首跑            重跑
status          ANSWERED        NO_ANSWER
missed_reason   (空)            ABANDONED_WAITING
agent_ids       23 个,去重 1    1 个,去重 1
ring_sec        0               121
bot_sec         9               12          ← bot 份额未受影响,仍在
flow_id         非空            非空
talk_sec        0               0           primary_agent_id NULL(无人接,正确)
legs            AGENT×23 / BOT 9s / QUEUE 159s   AGENT×23 / BOT 12s / QUEUE 121s
```

CDR 总数 6189 → **6190**(恰好一行);幽灵 OUTBOUND 47 → **47**(C20 仍生效,23 次派单零幽灵)。

`legs` 里仍有 23 段 `AGENT`(各 0 秒、note="did not answer")—— 这是**有意保留**的:
每段都是一次真实的派单尝试,详情页应当看得见交换机试了多少次。被去重的是 `agent_ids`
(报表按坐席归集的口径),两者口径不同。

## `ring_sec` 与 `queue_wait_sec` 同为 121 秒

不是重复计数:两者本就重叠,`ring_sec` 是 `queue_wait_sec` 的子区间
(已答样本亦然:VC-S7-03 为 54/55,95002 通话为 4/4)。本例入队后 0.1 秒就开始振铃、
一直响到主叫挂断,故两个区间几乎完全重合。

## 留证半:F3 结论不变

wei 参数 `max_no_answer=0` / `no_answer_delay_time=0`,节奏仍是
**1 次派单 → 60 秒振铃超时 → 约 0.1 秒间隔的 11 连发**,每轮 12 次。
recommended 值与落地次序见 TASKS **W2.1**。

## 仍然成立的缺口(本用例的本意)

wei 的 `agent_states` 前后完全一致 —— **RONA 在 presence、报表、坐席屏三处依旧毫无痕迹**。
C22 修的是账目(这通电话现在如实记为"无人接听、排队放弃"),**没有**修状态机:
坐席仍不会被摘出路由。那是 W2 的范围,本条留证是它的修复前基线。

`ABANDONED_RINGING` 的条件互斥错误(`missedReason` 要求 `BridgedAt≠0`,与"振铃中放弃"
语义矛盾)本次亦未动,归 W2。故本例落在 `ABANDONED_WAITING` —— 与账本 expect 一致。
