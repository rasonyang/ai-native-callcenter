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

---

## 重跑 —— 2026-08-23 16:04(W2/W2.1 落地后;材料改用 95002 / support-zh / 中文)

### 第一次尝试:信号选错了,现场把它揪出来

首版消费 `agent-status-change` → `On Break`。看起来像"消费交换机的决定",实际上
**`On Break` 只是一个词,而应用镜像 presence 用的正是同一个词**,两者到达时长得一模一样。

```
15:50:12  Agent agent-wei Origination Canceled : NO_ANSWER    ← 漏接 #1
15:50:12  Agent agent-wei sleeping for 60 seconds             ← no_answer_delay_time 生效
          no_answer_count=1，max_no_answer=2 未满 → 交换机根本没摘人
15:51:30  Updated Agent agent-wei set status = On Break       ← 我们自己镜像下去的
wei 状态历史:NOT_READY/SYSTEM 落在 07:56:21Z ——【重启那一刻】,不是那通电话
```

后果比"测不出来"更糟:**每次重启,设备状态尚未观测到的坐席被镜像成 On Break,
回声立刻返回,于是每人都因"从未派给他的电话"被摘出路由** —— 与 C39 同源。
两道守卫理论正确、实际无用:那一刻 presence 确实 READY,话机确实可达。

**改用"派单失败 + 原因 NO_ANSWER"**:这个信号我们自己产生不出来 ——
交换机只在**真的响过某人**之后才报;原因把"被无视的话机"与"根本接不了的话机"分开
(`USER_BUSY` 即今早 C37 那 42 次、`ORIGINATOR_CANCEL` 是队列自己撤回),都不是坐席的过失。
**一次漏接即摘人**,交换机的 `max_no_answer=2` 留作兜底。

留下的诊断日志是分出这件事的唯一手段 —— **静默 decline 与"根本没被调用"长得一模一样**。

### 第二次:三边一致,12 毫秒

```
16:04:08.094  a delivered call rang out unanswered  agent=agent-wei queue=support-zh cause=NO_ANSWER
16:04:08.096  wei → NOT_READY / SYSTEM          （+2ms）
16:04:08.106  AGENT_NOT_READY 上流              （+10ms）
交换机        agent-wei status=On Break  no_answer_count=1
CDR           95002 | NO_ANSWER | ABANDONED_WAITING | ring_sec=59 | bot_sec=12 | agent_ids=1
queue_events  JOINED=1  OFFERED=1  ABANDONED=1  无 BRIDGED
```

首跑时这里是"前后完全一致 state=READY" —— 交换机摘了人、应用不知道、下次镜像又把他推回去。
现在交换机**通知**、应用**决定**、应用**发布**、应用**镜像回去**,三边一致。

`missed_reason=ABANDONED_WAITING` 也对:主叫是在振铃结束之后才挂的,
而"他离开那一刻那次振铃是否还活着"正是今天 W2 给 `ABANDONED_RINGING` 补上的判据。

### 未过的一条:振铃时长仍是 59 秒,不是 15 秒

`ring_sec=59`,交换机侧 `16:03:08.7 → 16:04:08.0`。
`agent-originate-timeout=15` 已写进 `aicc_xml.lua` 的 `<settings>` 并随 XML 下发,
**但 mod_callcenter 只在模块装载时读 settings** —— `reloadxml` 不会让它重读。
这是一处"看起来已生效、实际没有"的配置,正是本轮验收要抓的那类。

已执行 `reload mod_callcenter` 并重启应用重建坐席与配员
(三个坐席 `max_no_answer=2` 回位、三条 tier 回位)。**该条待下一通电话确认。**

### 判定

**暂不判 PASS。** RONA 全链的断言全部成立,但本轮修订时写进 expect 的
"每次振铃约 15 秒"一条**实测不成立**,原因已定位并已处置,待一通确认电话。

### 确认通话(16:09)与最终判定

第三通只为确认振铃时长。**仍是 59.7 秒**(`16:09:07.33 → 16:10:07.02`,CDR `ring_sec=59`),
`xml_locate` 显示交换机确实拿到了 `agent-originate-timeout=15`,模块也已重新装载。
已立案 **C41**,该条降为留证。

**PASS** —— RONA 全链的断言两次成立;振铃时长一条不因它判 FAIL,也不假装它已经生效。

### C41 收尾(2026-08-23 17:14):绕开那个参数,而不是让它生效

把 `agent-originate-timeout` 同时写进 `<settings>` 与每个 `<queue>` 并重载,**第三次实测仍是 59 秒**。
四条替代解释全部排除(参数名在模块字符串表里、`<settings>` 确实被读——同块的 `odbc-dsn` 在生效、
`xml_locate` 证明交换机拿到了值、模块确实重新装载过),故判定该参数在本版本 mod_callcenter 上不起作用。

改走自己控制得了的路:振铃时长挂在坐席自己的拨号串上 ——
`contact={leg_timeout=15}user/1008@192.168.31.55`。

```
17:14:13.115  Setting outbound caller_id_name        ← 起振
17:14:28.003  Agent agent-wei Origination Canceled   ← 14.9 秒（此前 59.7）
CDR           95002 | NO_ANSWER | ABANDONED_WAITING | ring_sec=14
17:14:28.050  a delivered call rang out unanswered → wei NOT_READY/SYSTEM
```

本条 expect 里那条振铃时长随之**由留证转回断言**。
