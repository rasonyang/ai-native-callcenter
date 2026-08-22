# VC-S13-01 — 坐席看到的等待名单只是自己配员的队列 · 判定:**PASS**

执行 2026-08-22 09:49–09:56(UTC 01:49–01:56)。app `logs/aicc-20260822-084508.log`。
owner 拨了三通(两轮),最终以**两路真正同时保持**取证。

## 前置布置(与账本 precondition 的一处**有意偏离**)

| | |
|---|---|
| switch tier list | `agent-wei`→support-en + support-zh;`agent-ben`→support-zh **only** |
| DB `queue_agents` | wei→support-en + support-zh;ben→support-zh |
| 两坐席状态 | **均置为 On Break**(账本原写"两人均 READY") |

**偏离理由(已验证,非猜测)**:端点取队列走 `ListQueuesForAgent`
(`internal/store/sql/telephony.sql:75-80`),SQL 是 `JOIN queue_agents ON agent_id = $1`
—— **纯配员,不含任何可用性条件**。坐席是 READY 还是 On Break 不可能影响本端点的返回。
而留 wei 为 READY 只会引入一个竞态:派单腿会响他的话机 5 分钟,一旦被接起,
主叫 bridge 掉就离开等待名单(`waiting.go` 的 `KindQueueBridgeStart` → `queueLeft`),
取证窗口当场消失。置 On Break 去掉竞态,不削弱任何断言。**已回写账本。**

## 逐条对照(阶段 C:两路同时等待,01:55:36Z)

switch 侧两条队列各有 1 名 member。两位主叫**号码不同**(…669 / …660),
因此不存在"同一行渲染两次"的可能。

| expect | 实测 | |
|---|---|---|
| wei(配员两条队列):support-en 与 support-zh 各 1 | `[{support-en,…669},{support-zh,…660}]` | ✓ |
| ben(只配员 zh):**只有 support-zh 那一条**,support-en 对他不可见 | `[{support-zh,…660}]` —— **恰好一条** | ✓ |
| supervisor:两条都在 | `[{support-en,…669},{support-zh,…660}]` | ✓ |
| 三者对 support-zh 那条的 fromNumber 一致 | 三者 fromNumber 全为 `18688886660` | ✓ |

**补强(账本没要求,但值得钉)**:三个视角对那一条的 **callId 与 queueId 也完全相同**
(`01a0272d-50ed-…` / `019ffaae-9bf6-…`)—— 不只是号码碰巧一样,是同一个等待者对象
经三条不同的取数路径(`WaitingCalls(queueIDs)` ×2、`AllWaitingCalls()` ×1)呈现。

`failure_looks_like`(ben 也看到 support-en 的等待者,坐席被展示了自己接不到的电话)
**未发生**。

## 为什么必须两路同时 —— 这一段是本次执行的方法论收获

第一轮 owner 是**先挂第一通、再拨第二通**,于是取到两段单等待者的快照:

```
阶段 A(仅 support-en 在等):wei=[support-en]  ben=[]           sup=[support-en]
阶段 B(仅 support-zh 在等):wei=[support-zh]  ben=[support-zh] sup=[support-zh]
```

A 已经证明**过滤生效**(ben 眼前有等待者却看不见),B 已经证明**ben 不是瞎的**
(他确实看得见自己那条)。两段合起来,本用例的结论其实已经成立。

**但有一条 A+B 永远测不到**:wei 同时配员两条队列,`Coordinator.WaitingCalls` 收的是
**一个 queueID 列表**(`waiting.InQueues(queueIDs)`)。每段只有一个等待者时,
"列表里两个 id 能否正确并集"这件事根本没有被执行到 —— 一个只返回列表首个 id 的实现
在 A 和 B 里都会表现得完全正确。**多队列坐席的核心恰恰是这个并集。**
故请 owner 重做为两路同时,阶段 C 才是判定所依据的那次。

**账本的 human step 要改**:原文"若只有一路主叫,则先后拨两次、第一次不挂断"给了
一个**做不到的退路** —— 一路主叫无法同时保持两通。本机的正解是用第二部话机
(分机 **1007** 已注册且不绑任何坐席,`sip:1007@192.168.31.55:53300`),或两路外部主叫。

## 顺带发现:collect 第 1 条取的不是端点用的那个源

collect 第 1 条读 **switch 的 `callcenter_config tier list`**,而端点读的是
**DB 的 `queue_agents`**。两者当场就不一致:

```
DB      : amy|support-en   ben|support-zh   chen|support-en   wei|support-en   wei|support-zh
switch  :                  agent-ben|support-zh                agent-wei|support-en  agent-wei|support-zh
```

DB 里的 `amy`、`chen` 在交换机的 tier list 里没有(两人 LOGGED_OUT;
mod_callcenter 只列已加载的 —— 与 **C1** 同一机制)。
wei 与 ben 两条恰好两侧一致,**本次断言因此不受影响**,但取证该取端点真正查的那个源。
**已在账本 collect 补上 DB 一条,switch 那条降为旁证。**

## 附:members 命令要带域名

`callcenter_config queue list members support-en` 返回空的 `+OK`;
必须写 `support-en@192.168.31.55` 才列出 17 列成员表。本用例的 collect 未用到该命令,
记此供其他用例参考。

## 收尾与顺带复验(01:57:42Z,两通挂断后)

```
等待名单        → 0
两通 CDR        01a0272c… 95001 NO_ANSWER ABANDONED_WAITING bot_sec=13 queue_wait_sec=145
                01a0272d… 95002 NO_ANSWER ABANDONED_WAITING bot_sec=15 queue_wait_sec=123
窗口内 CDR 总数  2 行,恰为两通:…669→95001、…660→95002
```

两条顺带的复验(非本用例断言,但同一批数据免费给出):

- **C22 的修复在场**:两通都是"bot 先接 → 转队列 → 无人应答 → 主叫放弃",
  状态是 `NO_ANSWER` / `ABANDONED_WAITING` 而**不是** `ANSWERED` ——
  C22 修复前这类电话在报表里全线不可见,bot_sec>0 就被无条件判 ANSWERED。
  且 bot_sec(13/15)仍然保留,没有因为最终无人应答而丢账。
- **无幽灵行**:窗口内恰好 2 行,C20/C24 那类"每次派单/跌落各自成行"的多余 CDR
  一条都没有(本次没有派单腿,两坐席均 On Break,故只压到了 C20 的一半)。

执行后已把 wei 恢复为 **READY**(布置时置的 On Break 是测试设置,不留痕)。
