# 实现方案:answer 归对账,bridge 归通话

> **2026-08-31 — 本文引用的 `docs/verification/artifacts/` 已删除。**
> 那个目录(138 个文件,原始日志/SSE 抓包/verdict)在仓库瘦身中整体移除;**判定与结论未改动**。
> 文中的 artifacts 路径按历史记录读:它们说明那次验证的证据当时收在哪里,不是现存文件。
> 要取回原件:`git log --diff-filter=D -- docs/verification/artifacts/`。


> 起因:2026-08-21 CDR 设计审查。owner 定的模型 ——
> **answer(交换机应答)用于运营商对账;bridge(双向通话建立)用于判断坐席是否真的通过话。**
> owner 已裁:**保持(hold)期间 talk 继续计。**
> 本文件是方案,未实施。

## 0. 为什么要动

两处现状用数据钉死了。

**`answered_at` 今天的语义随分支变** —— 有坐席接起时记坐席应答,没有时记主叫应答:

```
call      answer_offset(相对 started_at)  bot_sec
01a02275            29s                      17     ← 记的是坐席接起
01a02299             0s                       9     ← 记的是主叫腿应答
```

对账要的永远是面向运营商那条腿的 200 OK。同一列两种含义,不能用。

**`talk_sec` 只取第一个应答的坐席腿,转接后漏计**:

```
call 01a021f5   talk_sec = 58
                legs = [QUEUE 135s] [AGENT 1008 58s] [AGENT 1007 35s]
```

1008 讲 58 秒、1007 讲 35 秒,账上只有 58 —— 少算 35 秒。

**并且 leg-answer 本身就不等于通话。** 今天 click-to-dial 撞过 `INCOMPATIBLE_DESTINATION`
(opus vs PCMU):坐席腿正常回 SIP 200,媒体不通,没有任何真实通话。
自动应答话机(`sip_auto_answer=true`)同理 —— 话机自动接的,不代表人在听。
**bridge 才回答得了"有没有真的接通到人"。**

## 1. 三个锚点,三种用途

| 列 | 锚点 | 服务于 |
|---|---|---|
| `answered_at` + **`bill_sec`**(新) | **主叫腿** 的 `AnsweredAt` → `ended_at` | 运营商对账 |
| `talk_sec` | **坐席腿** bridge 区间的**并集** | 坐席工时、SLA |
| `status` | 有没有与坐席腿 bridge 过 | 运营指标 |

`total_sec`(started→ended)保持不变,它是话务时长不是计费时长,两者本就不同。

## 2. 数据模型

### 2.1 Party 记录 bridge 区间

`internal/telephony/call.go`,`Party` 与 `PartySnapshot` 各增一个区间列表:

```go
// BridgeSpan is one stretch during which this leg had two-way media with
// another. A leg can have several: the caller is bridged to the bot, then to
// the agent who takes over, then to the agent that one transfers to.
type BridgeSpan struct {
    OtherChannelID string
    StartedAt      time.Time
    EndedAt        time.Time // zero while the bridge is still up
}
```

`Party.Bridges []BridgeSpan` / `PartySnapshot.Bridges []BridgeSpan`(json `bridges,omitempty`)。

> `live_calls` 表在设计文档里有、库里**未实现**,快照只在内存,故新增字段无迁移代价。

### 2.2 事件处理(`internal/telephony/registry.go` `applySwitchEvent`)

| 事件 | 现状 | 改为 |
|---|---|---|
| `KindChannelBridge` | 只设 `OtherNumber`,注释写着 "is not a state change" | 仍不改 state,但**开一个区间**(已有未闭合区间则为幂等空操作) |
| `KindChannelUnbridge` | **actor 完全不处理**(只有 coordinator 拿去 detach tap) | **新增 case**:闭合当前区间 |
| `KindChannelHangup` | 设 ReleaseCause / 转 RELEASED | 额外:以 `ReleasedAt` 闭合未闭合区间 |
| `KindChannelHold` / `Unhold` | 转 HELD / TALKING | **对区间不做任何事**(owner 裁决) |

**hold 的处理必须显式,不能靠巧合。** 用 `uuid_phone_event hold` 时话机发 re-INVITE,
FreeSWITCH 是否会随之抛 `CHANNEL_UNBRIDGE` **尚未实证**(needs-FACT,见 §6)。
因此 `KindChannelUnbridge` 的 case 要写成:

```go
case KindChannelUnbridge:
    // A leg on hold is still on this conversation — the caller hears music
    // instead of a person, but the agent has not left. Owner's ruling
    // (2026-08-21): hold counts as talk. Whether the switch even unbridges on
    // hold is not something we rely on either way.
    if party.State != PartyHeld {
        closeBridge(party, ev.OccurredAt)
    }
```

这样无论交换机是否在 hold 时抛 UNBRIDGE,结果都符合裁决。

### 2.3 assemble 的三处判据(`internal/telephony/cdr.go`)

```go
// 现在                                    改为
answered = 第一个 AnsweredAt != nil 的坐席腿   agentTalk = 所有坐席腿 bridge 区间的并集
cdr.Status = answered != nil ? ANSWERED     cdr.Status = len(agentTalk) > 0 ? ANSWERED
cdr.AnsweredAt = answered.AnsweredAt        cdr.AnsweredAt = originator.AnsweredAt  ← 永远主叫腿
cdr.TalkSec = talkEnd - answered.AnsweredAt cdr.TalkSec = agentTalk 总时长
cdr.RingSec = answered.AnsweredAt - Created cdr.RingSec = 首个 bridge 起点 - 该腿 CreatedAt
                                            cdr.BillSec = ended - originator.AnsweredAt  ← 新
```

`PrimaryAgentID` 改为**第一个有 bridge 区间**的坐席腿的 agent。

bot 分支(`!soughtAPerson`)不变 —— 纯 bot 电话仍是 ANSWERED,`bill_sec` 照样从主叫腿应答起算
(那正是对账要的:bot 接的电话运营商一样计费)。

**并集而非求和**:协商转接(consult)期间两条坐席腿可能同时在线,求和会重复计。
分机互拨(1007 拨 1008)两条腿都有 `AgentID`,同一段时间在两个 party 上各记一份,并集收成一段。

### 2.4 bot 桥接与人工桥接如何区分(owner 2026-08-21 追问)

**不在 bridge 事件上区分 —— 区分依据是"这段区间挂在谁身上"。**

```go
split()      // agentLegs 取 p.AgentID != nil
isBotLeg()   // 目的号 == DID 且方向 outbound;AgentID 恒为 nil
```

两个判据结构上互斥(分机号永远不等于 DID)。所以:

- 记录时**不看对端**,只记 `OtherChannelID`;
- 判定时**只读坐席腿自己的区间**。bot 的区间挂在 bot 腿的 party 上,`split()` 本就滤掉它;
  主叫腿同时有两段(桥 bot、桥坐席),而我们从不读主叫腿的区间。

成立前提:**坐席腿在本系统中永不与 bot 腿桥接** —— bot 服务完主叫后转走的是*主叫*,
随即自己那条腿挂断;坐席只会与主叫(或 consult 时的另一坐席)桥接。

这样还消掉一个时序风险:`CHANNEL_BRIDGE` 到达时对端 party 可能尚未入册(C20 的残留竞态)。
若分类依赖当场查对端,偶尔会失败;按区间归属在 `assemble()` 时分类,那时每条腿的身份都已确定。

### 2.5 bot_sec 改由 bot 腿的 bridge 区间算(顺带补上审查①)

bot 腿的 bridge 区间是 `[bot 应答, 主叫被转走]` —— **终点正是主叫真正离开 bot 的时刻**,
包含"我这就帮您转人工"那句话与 PLAYBACK_DONE 的等待,也就是每通稳定漏掉的 6–9 秒。

因此 `bot_sec` 不必改 `stampBotShare` 的盖章时机(那要动 aicall 的时序,风险不小),
直接取 bot 腿区间即可。审查①由此从"待裁口径"变为随本方案一并解决。

**回落**:bot 腿区间为空时(例如 bot 腿未被入册),仍用 `snap.Bot.Sec` 的通道变量值。
两者都在时以区间为准,并在差值 > 2s 时记 WARN —— 这就是 §6 自洽校验的第一个具体用例,
且不依赖 F13。

## 3. Schema 与契约(spec-first,顺序不可颠倒)

### 3.1 迁移 `internal/store/migrations/00014_cdr_bill_sec.sql`

```sql
-- +goose Up
ALTER TABLE cdrs ADD COLUMN bill_sec int NOT NULL DEFAULT 0 CHECK (bill_sec >= 0);
UPDATE cdrs SET bill_sec = GREATEST(0, EXTRACT(EPOCH FROM (ended_at - answered_at))::int)
 WHERE answered_at IS NOT NULL;

-- +goose Down
ALTER TABLE cdrs DROP COLUMN bill_sec;
```

按 CLAUDE.md 的规矩,**迁移未跑过就不算评审过**:`internal/store/migrate_test.go` 要补一条
**含历史行**的 fixture —— 既有 `answered_at` 非空的行必须回填出正确值,空的填 0。

历史行注意:C22 之前 `answered_at` 记的是坐席应答,所以回填出的 `bill_sec` 对**转接类历史行偏小**
(少了 bot 那一段)。这一点要在迁移注释里写明:**回填是尽力而为,不是重算**;
准确的 `bill_sec` 从本次改动生效之后才有。

### 3.2 契约 `docs/openapi.json`

1. `components.schemas.CDR` 增 `billSec`(integer,进 `required`——响应加字段不是 breaking)
2. 给 `answeredAt` / `talkSec` / `billSec` / `totalSec` 各补 `description`,把四者的口径写进契约,
   这是本次审查暴露出的真正缺口:**四个时长字段今天在契约里一句说明都没有**
3. `make api-lint` → `make api-generate`(提交 `api.gen.go` 与 `web/src/generated/api.ts`)
   → `make api-breaking BASE=main`
4. 前端 CDR 详情页加一行"计费时长",与"通话时长"并列

## 4. 顺带被这次改动治好的

| 编号 | 原缺陷 | 为何被治好 |
|---|---|---|
| C22 残留 | 判据用 leg-answer,自动应答/编解码不通会误判为已接通 | 改用 bridge |
| C17 残留 | 坐席外呼的 `talk_sec` 从坐席自己那条自动应答的腿起算,偏大 | bridge 才是被叫真正接起的时刻 |
| 审查①  | `bot_sec` 截到工具触发而非主叫真正离开 bot,每通漏 6–9 秒 | 改由 bot 腿 bridge 区间算(§2.5) |
| 审查②(部分) | 五个时长无自洽校验 | `bill_sec` 有了**第二个独立来源**可对账,见 §6 |

**未被治好、仍待裁的**:审查③(`missed_reason` 词表只覆盖队列,51 行未接说不出理由;
`status=BUSY` 有常量有数据无产出方)。与本方案正交,可独立推进。

## 5. 影响面与重跑清单

**测试**:`internal/telephony/cdr_test.go` 里 10 处 `AnsweredAt` 构造的 fixture 都要补 bridge 区间;
C22 的两条回归测试要改判据。新增测试至少四条:
① 转接的 `talk_sec` = 两段并集(以 01a021f5 的 58+35 为原型);
② 坐席腿应答但从未 bridge → `NO_ANSWER`(以 `INCOMPATIBLE_DESTINATION` 为原型);
③ hold 期间 talk 继续计(owner 裁决的钉子);
④ `bill_sec` 从主叫腿应答起算,与坐席何时接起无关。

**账本重跑**(断言涉及 status/talk_sec/answered):
`VC-S1-01`(ledger.yaml:148)、`VC-S4-01`(:82)、`VC-S3-03`(:238)、`VC-S7-03`(:565)。
四条现均为 PASS,预期改动后仍 PASS —— 但 `talk_sec` 的数值会变(转接类变大),
故属**重跑复核**而非改写 expect。`VC-S7-02` 需新增 `talk_sec = 两段之和` 的断言。

**近期 6 行真实 CDR** 作为改动前后的对照样本,已存于 `artifacts/`。

## 6. needs-FACT(实施前要落实)

| # | 事实 | 结论 |
|---|---|---|
| F12 | `uuid_phone_event hold` 是否触发 `CHANNEL_UNBRIDGE` | **否**(2026-08-21 实证)。事件序列是 `CHANNEL_HOLD` → `CHANNEL_UNHOLD`,中间无 UNBRIDGE;唯一那次 UNBRIDGE 是 bot 腿在转接时离开。owner 的"hold 计入 talk"因此是自然行为,§2.2 那道 `party.State != PartyHeld` 防护是保险而非承重 —— **保留**,因为它把裁决写进了代码,而不是让账目依赖交换机的某个未言明的行为 |
| F13 | `variable_billsec` / `variable_answer_stamp` 是否出现在 `CHANNEL_HANGUP_COMPLETE` | **是**(2026-08-21 实证)。主叫腿挂断带着 `answer_stamp` / `answer_epoch` / `billsec` / `billmsec` / `duration` / `start_stamp` / `end_stamp` / `progress_media_stamp`。计费数字**有了第二个独立来源** |
| F14 | 协商转接(consult)期间两条坐席腿是否真会同时 bridge | 待 consult 进入验收范围;不卡 —— 并集算法两种情况都对 |

F13 已落实,自洽校验**已实现**(commit 见下):`assemble()` 收尾比对自算的 `bill_sec`
与交换机的 `billsec`,不一致记 WARN 并把两个值都落进 `tech`。**不改数,只让它说话。**

首份实测的差都是 1 秒,且来源清楚:

| | 我们算的 | 交换机的 | 差因 |
|---|---|---|---|
| `bill_sec` | 103 | `billsec=104`(`billmsec=103570`) | 我们截断,FS 四舍五入 |
| `talk_sec` | 80 | 坐席腿 `billsec=81` | 应答到桥接之间的间隙 —— **这正是本次改动要区分的那件事** |
| `bot_sec` | 14 | bot 腿 `billsec=15` | 同上 |

因此校验的容差应为 **±2 秒**,超出才 WARN。

## 7. 实施顺序

1. F13 抓一通电话的 hangup 变量表(顺带 F12)—— 零成本,搭下一通验收电话即可
2. 契约先行:`docs/openapi.json` → `make api-generate`
3. 迁移 + `migrate_test.go` 的历史行 fixture(**跑过才算评审过**)
4. `Party.Bridges` + 事件处理 + `assemble()` 判据
5. 单测(§5 四条 + 既有 fixture 补齐)
6. 前端 CDR 详情页加"计费时长"
7. 账本四条重跑复核 + S7-02 补断言
