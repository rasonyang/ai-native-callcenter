# VC-S5-01 — RONA(坐席不接) · 判定:**FAIL**(CDR 收官条款)

执行 2026-08-21 12:34–12:38。呼叫 `01a02299-b24b-7c84-98c2-b4347c2f2885`:
18688886669 → 95001 → bot 转人工 → wei 的 1008 响 159 秒全程不接 → **主叫挂断**。

## 可验半

| expect | 实测 | |
|---|---|---|
| SSE:`QUEUE_AGENT_OFFERED` ≥1 且全程无 BRIDGED | **23** 条 OFFERED;SSE 只出现 JOINED/COUNT/OFFERED/LEFT,**无 BRIDGED** | ✓ |
| queue_events:JOINED=1,OFFERED ≥1,无 BRIDGED | `JOINED=1`、`OFFERED=23`、`ABANDONED=1`,**无 BRIDGED** | ✓ |
| wei 前后两次 `agent_states` 完全一致 | 前 `READY\|`(空 reason)→ 后 `READY\|` | ✓ |
| cdrs:`status=NO_ANSWER`、`missed_reason=ABANDONED_WAITING` | **`ANSWERED`、`missed_reason` 为空** | ✗ |

最后一条不满足 → **FAIL**。

## 为什么 status=ANSWERED:expect 写在 C11 修复之前

`cdr.go:246` 的分支:

```go
} else if snap.Bot.Sec > 0 || (!isAgentPlaced && originator != nil && ...) {
    // The bot answered, or the call never sought a person at all.
    cdr.Status = store.CDRStatusAnswered
```

`snap.Bot.Sec > 0` **无条件**判 ANSWERED —— 不问这通电话之后是否进了队列、是否有人接。

**这条分支在 C11 修好之前打不到**:转接呼叫的 `Bot.Sec` 恒为 0(那正是 C11),
于是一路落到 `else` 分支给出 `NO_ANSWER` + `missedReason`。账本的 expect 正是照着那个
**由缺陷造成的**行为写的。C11 修复让 `Bot.Sec` 真正到达,这条分支第一次生效,
把一通"bot 接了、转队列、无人应答、主叫放弃"的电话记成了 `ANSWERED`。

必须说清楚:**是 C11 的修复让这个缺陷显形的**,不是它制造的。原本就存在的语义错误
("bot 接了 = 这通电话被接了,之后发生什么都不算")一直被 `Bot.Sec=0` 掩盖着。

**影响面**:本部署所有入站电话都先经 bot,因此**队列放弃在报表里将全线不可见** ——
`status` 恒 ANSWERED、`missed_reason` 恒空。这比账本原先钉的"RONA 三处无痕"更严重:
连放弃本身都记不出来。

## 另外两处失真(同一行 CDR)

```
agent_ids       23 个元素,去重后 1 个   ← 每次派单重试各追加一次,无去重
legs            AGENT × 23(各 0 秒) + BOT 9s + QUEUE 159s = 25 段
ring_sec        0        ← 响了 159 秒,一秒没记
talk_sec        0        primary_agent_id  NULL
queue_wait_sec  159      ✓(未 bridge,joined→left,正确)
```

`cdr.go:206-210` 的循环对每条坐席腿 `append(cdr.AgentIDs, *leg.AgentID)`,不做去重;
`RingSec` 只在 `answered != nil` 时才算(cdr.go:238),没人接就永远是 0。

**这两处是 C20 修复后才暴露到这一行上的**:此前 23 条派单腿各自成幽灵 call,
这一行看不到它们(代价是 23 条幽灵 CDR)。**本次幽灵计数 47 → 47,一条没长** ——
C20 依然生效,23 次派单零幽灵。

→ 三处合并立案 **C22**。

## 留证半:F3 已落实

`callcenter_config agent list` 实证 wei 的参数:
`max_no_answer=0`、`no_answer_delay_time=0`、`wrap_up_time=0`、`reject_delay_time=0`、`busy_delay_time=0`
—— 即**永不标记未应答、零延迟无限重派**。

实测节奏(23 次 OFFERED,跨度 122.0 秒):

```
gaps_sec = [60.08, 0.07, 0.08, 0.11, 0.08, 0.11, 0.08, 0.08, 0.11, 0.09, 0.09,
            60.09, 0.07, 0.08, 0.09, 0.10, 0.08, 0.12, 0.07, 0.12, 0.08, 0.11]
```

形态是:**1 次派单 → 60 秒振铃超时 → 约 0.1 秒间隔的 11 连发 → 再 60 秒 → 再 11 连发**。
每轮 12 次,与 VC-S7-02 观察到的 12 条幽灵 CDR 同源。

这就是 `failure_looks_like` 描述的场景在真实参数下的样子:一部没人守的话机可以把队列
吸干,而 presence、报表、坐席屏三处**确实**毫无痕迹(wei 的 agent_states 前后一模一样)。
本条留证是 W2(D7② RONA 全链实现)的修复前基线。
