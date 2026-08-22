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
