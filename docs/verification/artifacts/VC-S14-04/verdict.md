# VC-S14-04 — 坐席外呼全链(内部 / 外线 + 能力限制)· 判定:**FAIL**

执行 2026-08-22 13:38–13:54(UTC 05:38–05:54)。app `logs/aicc-20260822-084508.log`。
**内部一型在第一步就断了**:从 wei(浏览器话机)发起的 click-to-dial **根本没有拨出去**。
外线一型因此未执行。立案 **C32**。

## 前置(现读,未沿用草案写死的号码)

```
ben=1002   wei=1008        两人 READY,两部话机均已注册
CDR 基线 6214              pstn_sim 网关 NOREG(静态网关,正常)
```

## 一、wei(浏览器话机)→ ben:**被叫从未响铃**

`POST /calls/dial {destination:1002}` 返回 **201**,`callType=INTERNAL`(C17 的判定正确),
但 owner 实测 **1002 根本没有响铃**。交换机侧全程**只有一条腿**。

FS 状态机(通道 `1b207c08-…`,即 originate 指定的 `origination_uuid`):

```
13:40:23.722  CS_NEW → CS_INIT → CS_ROUTING
13:40:23.732  Ring-Ready;entering state [calling][0]
              CS_ROUTING → CS_CONSUME_MEDIA
              entering state [proceeding][180]      ← wei 的话机在响
13:40:24.833  entering state [completing][200]
              entering state [ready][200]           ← 200 OK,sip_auto_answer 生效了
              ……此后 2 分钟停在 CS_CONSUME_MEDIA,从未进入 CS_EXECUTE
```

**通道拿到了 200,却从未到达"已应答",`&park()` 从未执行,`CHANNEL_ANSWER` 从未发出。**

而 click-to-dial 的转接正是挂在这个事件上:`outbound.go:213` 的 `arm(agentLeg, onAnswer)`,
由 `KindChannelAnswer`(`:347→362`)触发才去调 `TransferToExtension`(`:214`)。
事件不来 → 转接不执行 → **被叫号码从头到尾没有被拨过**。

佐证:应用日志 `click-to-dial transfer failed` 计数 **0** —— 不是转接失败,是**根本没被调用**。

## 二、判别实验:换原生软电话发起,一次就通

`ben(1002,原生软电话 Telephone 1.6/UDP)→ wei(1008,浏览器)`:

```
05:51:03  1002 CS_CONSUME_MEDIA                          app: 1 腿 DIALING
05:51:07  1002 CS_EXECUTE + p1878sob CS_EXCHANGE_MEDIA   app: 2 腿 TALKING/TALKING
05:51:19  正常挂断
```

**变量锁定在发起腿**:原生软电话发起 → 通;浏览器(WSS/WebRTC)话机发起 → 卡死在
`CS_CONSUME_MEDIA`。**注意 C17 于 2026-08-20 复测这条路时是通的,当时 1008 也是这部浏览器话机**
—— 故不能直接断言"WebRTC 一向如此",中间有变量(今日 wei 的话机因 VC-S13-05 被 Sign Out 后重新注册过)。
根因待定,**怀疑但未证实**:originate 里钉的 `absolute_codec_string=PCMU`(`outbound.go`)
在 DTLS/opus 的 WebRTC 腿上使媒体协商无法收官。

## 三、反向那一通的账面:S14-04 的内部一型断言**全中**

```
INTERNAL | ANSWERED | 1002→1008 | ring=1 | talk=12 | bill=0 | total=12 | agents=2
legs: [ { kind: AGENT, label: 1008, durationSec: 12 } ]
```

| expect | 实测 | |
|---|---|---|
| `call_type=INTERNAL` | INTERNAL | ✓ |
| `status=ANSWERED` | ANSWERED | ✓ |
| legs 只含被叫那一段 `AGENT\|<ben 的分机>` | 只含 `AGENT\|1008` | ✓ |
| **talk_sec 是并集不是两倍** | `talk=12`、`total=12`(两腿同一段对话) | ✓ |
| **bill_sec=0**(分机互拨无运营商计费) | `bill=0` | ✓ |

**账面这一层是好的** —— C15/C17 修复的成果确实还在。断的是它前面的信令。

## 四、能力限制(409)未能验证

hold / retrieve / transfer 的 409 断言需要一通**已建立**的 INTERNAL 通话。
第一通卡死、第二通只活了 12 秒且用于验账面,**本次未执行**,不做判定。

## 五、UI 侧:一通"打不出去"的电话被渲染成可接听的来电

owner 截图(wei 的工作台,分机 1008):顶栏 `1002 Dialling` **带一个绿色 Answer 按钮**,
左栏 `CALLING OUT / 1002 / Dialling`,右栏却标 `1002 Inbound`。
**同一通电话在同一屏上同时是"打出去的"和"打进来的"。**

app 侧那条 party 全程 `state=TALKING` —— 应用认为已接通,交换机侧却连应答都没到。

审计日志证实 owner 确实点了那个按钮:

```
05:40:25  wei  POST /calls/dial   {destination:1002}
05:49:39  wei  POST /calls/{id}/answer          ← 点击"接听"
05:51:00  ben  POST /calls/dial   {destination:1008}
05:51:02  wei  POST /calls/{id}/hangup
```

## 六、那一次点击炸出了 C24 的完整形态,且比 C24 记的更重

`05:49:39` 之后立刻出现一串,与 `05:50:5x` 又重复一遍。本窗口 **10 行 CDR,真实通话只有 2 通**:

```
05:40:23 INTERNAL  NO_ANSWER 1008->1002       ag=1  ← 卡死那通(被 kill)
05:49:14 OUTBOUND  NO_ANSWER 1008->1002       ag=1
05:49:45 OUTBOUND  ANSWERED  1002->voicemail  ag=1  talk=9   ★
05:49:45 INTERNAL  NO_ANSWER 1002->(空)       ag=0  ORIGINATOR_CANCEL
05:49:58 INBOUND   NO_ANSWER 1002->1002       ag=1  ORIGINATOR_CANCEL
05:49:58 OUTBOUND  NO_ANSWER 1008->1002       ag=1  ORIGINATOR_CANCEL
05:50:21 OUTBOUND  NO_ANSWER 1008->1002       ag=1
05:50:52 OUTBOUND  ANSWERED  1002->voicemail  ag=1  talk=9   ★
05:50:52 INTERNAL  NO_ANSWER 1002->(空)       ag=0  ORIGINATOR_CANCEL
05:51:05 INTERNAL  ANSWERED  1002->1008       ag=2  talk=12  ← 真实的那通
```

★ 两行 **`1002→voicemail` 被记成 `ANSWERED`、`talk_sec=9`、并挂在一个坐席名下(`ag=1`)**。

**这是 C24 尚未记下的一层**:C24 记的是"未接的 click-to-dial 产生 3 行多余 CDR"(数量问题),
实测还有**质量问题** —— 语音信箱的问候语被计成**一通已接通的坐席通话**。
后果:它会进 `callsHandled`、进 `talkSec`、进队列/坐席报表,
**把从来没人说过话的 9 秒算成坐席工时**。VC-S13-03 的 my-day 与 VC-S13-04 的报表都吃这个数。
已回写 C24。

## 七、判定与残留

**FAIL** —— 主断言(坐席能把电话拨出去)不成立。
账面断言(第三节)全中,但那是靠原生软电话绕过缺陷才取到的,不足以让本例转绿。
外线一型与 409 能力限制未执行。

环境:0 通道、0 活呼叫,wei 的 ACW 已由 owner 收掉。
**未清理的残留**:本窗口 8 行幽灵 CDR 留在库里(属 C24,按既有约定不手工删)。

---

# 重跑 2026-08-22 18:22–19:08 · 判定改为:**PASS**

首次执行(13:38)因 **C32** 在第一步就断了 —— 从浏览器话机发起的 click-to-dial 拨不出去。
今日的交换机侧改造(aicc context 全链、profile context、`TransferToExtension` 目标 context)
之后重测,C32 不再复现,本例得以完整执行。

## 内部一型:wei(1008,浏览器)→ ben(1002)

```
18:22:49  wei 腿 CS_EXECUTE(首次诊断时死在 CS_CONSUME_MEDIA)
          FS 日志有真实 Transfer;应用 click-to-dial transfer failed 计数 = 0
```

| expect | 实测 | |
|---|---|---|
| `call_type=INTERNAL` | INTERNAL | ✓ |
| `status=ANSWERED`,legs 只含被叫段 `AGENT\|<ben 的分机>` | `legs=[{AGENT, 1002, 245s}]` | ✓ |
| **talk_sec 是两条坐席腿的并集,不是两倍** | `talk=245`、`total=249`(不是 490) | ✓ |
| **bill_sec=0**(分机互拨无运营商计费) | `bill=0`,`answered_at` 有值 | ✓ |
| hold / retrieve / transfer 均 **409 OPERATION_NOT_ALLOWED_FOR_CALL_TYPE** | 三者皆 409,body 为 "this is not available on an internal call" | ✓ |
| mute **202** | `202`(unmute 亦 202) | ✓ |

**C18 至此首次获得现场验证** —— 此前只有单元测试。CDR 6259 → 6260,恰好 1 行,零幽灵。

## 外线一型:wei(1008)→ 18688886669(owner 于 11:08 经工作台拨出)

审计确认走的是本用例的接口:`11:08:29 POST /api/v1/calls/dial {"destination":"18688886669"}`。

| expect | 实测 | |
|---|---|---|
| `call_type=OUTBOUND`,legs 含 **TRUNK** 段 | `legs=[{TRUNK, 18688886669, 6s}]` | ✓ |
| **bill_sec 从被叫应答起算,故 bill_sec ≤ total_sec 恒成立** | `bill=6`、`total=6`、`ring=2`、`talk=6` | ✓ |
| `tech.switchBillSec` 与 `bill_sec` 相差 ≤2 秒 | `switchBillSec=8` vs `bill=6`,**差 2** | ✓ |

**2026-08-21 曾出现的 `bill_sec=23 > total_sec=21` 不可能值未重现** —— 计费锚点的修复现场确认。

## 执行注记(owner 提供,已回写用例)

**外呼 pstn_sim 必须提前让 owner 准备接听。** 被叫手机不在线时,交换机侧收到的是
`480 Temporarily Unavailable` 或 `NO_USER_RESPONSE`,拿到的是不可判定的结果而非失败证据 ——
本次因此白跑了三拨,其中一拨还让我一度错判为"中继坏了"(见 C32/verdict-retest.md 的更正)。

## 判定

**PASS。** 两型的账面与能力限制逐条成立。本例的价值正如起草时所写:
把 C15/C17/C18 与计费锚点固化成回归断言 —— 这些都是改一行就会静默退化的东西,
而今天它们经历了一整轮交换机侧改造,靠这条用例才确认没有退化。
