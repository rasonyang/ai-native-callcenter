# VC-S13-05 — 话机失联 → 坐席被摘除 → 恢复 · 判定:**FAIL**

执行 2026-08-22 10:50–11:32(UTC 02:50–03:32)。app `logs/aicc-20260822-084508.log`。
应用侧三条断言全中;**交换机侧与事件流两条不成立**,立案 **C28**。

## 第一轮作废:关标签页杀不掉这部话机

按用例原文"**直接关掉 wei 的软电话标签页**"执行后,**注册纹丝不动**:

```
03:03:59Z  EXPSECS(83)     ← 正在倒数
03:05:08Z  EXPSECS(608)    ← 跳回去了,续注册了
Call-ID  lauqo1ravlohvj8k8n82   ← 与失联前完全一致
Contact  sip:6cml0pp5@… transport=ws  源端口 51897   ← 一致
Agent    SIP.js/0.21.2       Ping-Status: Reachable
```

**同一个 SIP.js 客户端在续期**,不是新客户端。软电话是 Chrome **扩展**,
SIP 注册活在扩展的后台上下文里,**关标签页不等于设备离线**。
此轮应用侧报 READY/isRegistered=true 是**对的**,不构成缺陷 —— 是用例的人工步骤写错了。
**已改**:人工步骤改为"扩展内 Sign Out / Clear Account,或禁用扩展",
并注明 `sleep 30` 也不成立(注册到期是**分钟级**:实测 EXPSECS 起始 600 秒量级)。

## 第二轮(扩展内 Sign Out):逐条对照

| expect | 实测 | |
|---|---|---|
| registrations 中 1008 消失 | `reg=0` | ✓ |
| roster `availability=DEVICE_UNREACHABLE`、`isRegistered=false` | 正是如此 | ✓ |
| **`state` 仍为 READY** | `state=READY` | ✓ |
| SSE `DEVICE_UNREGISTERED` ≥1 | **0** | ✗ |
| switch 侧 `agent-wei` **不再是 Available** | **仍是 `Available`**,持续观察 60+ 秒未追上 | ✗ |

恢复后:roster 回到 `READY`/`isRegistered=true`,注册恢复 ✓。

**取证的一处不足(如实记)**:SSE 抓流 `--max-time 1800` 起于 02:50:19,**到期于 03:20:19**,
而重注册发生在约 03:31 —— **晚于到期**,故"恢复那一刻的事件"本次没有抓到,不做断言。
不过同一代码路径在窗口内产出过两条(见下),恢复侧的事件形态由它们佐证。

## 立案 C28:根因一个函数,后果两条

`internal/agents/service.go:371-396` 的 `ObserveDevice`:

```go
// …更新 devices[] 与 live presence 的 IsRegistered / IsDeviceInService…
if profile, err := s.store.AgentProfile(ctx, agentID); err == nil {
    s.publish(ctx, events.TypeDeviceInService, profile, snapshot)   // :394
}
```

### ① 交换机镜像**根本没被调用** —— 队列继续往死话机派单

全函数没有任何一处触及交换机侧状态。应用知道这部话机没了(`DEVICE_UNREACHABLE`),
mod_callcenter 不知道,`agent-wei` 永远停在 `Available`。
后果就是用例 `failure_looks_like` 写的那句:**每一通排队电话都会被派给一部不存在的话机**,
振铃到超时再重派;主管墙上看到"有人在线却没人接",而坐席早已下班。
(本次窗口内两坐席都不在队列上,未产生实际误派;这一条是按机制与状态判定的。)

### ② 掉线时发出的事件,**类型是反的**

抓到的三条 `DEVICE_IN_SERVICE`:

```
seq=9200420  02:54:22Z  availability=READY                ← 续注册
seq=9200421  03:04:16Z  availability=READY                ← 续注册
seq=9200422  03:10:37Z  availability=DEVICE_UNREACHABLE   ← 掉线
```

第 394 行**两个方向发同一个类型**,于是掉线那一条顶着 `DEVICE_IN_SERVICE` 的名字,
载荷里写着 `DEVICE_UNREACHABLE` —— **一个自相矛盾的事件**。
任何按 `type` 过滤的消费者都会被误导;只有忽略类型、去读载荷的才拿得对。

这比 W7 的记法更糟:events.md 把 `DEVICE_UNREGISTERED` / `DEVICE_REGISTERED` 列为
"十个零生产者"之一,读起来像"还没实现";**实测是发错了一个**,契约里那两个类型
一次都没出现过(`DEVICE_UNREGISTERED=0`、`DEVICE_REGISTERED=0`)。
建议把这条实测结论带给 **W7**:它要做的不是"补一个生产者",而是**把现有的这个改对**。

## 我起草时的错(与产品无关)

`SSE DEVICE_UNREGISTERED ≥1` 被我写成了**硬断言**,而它是 W7 名下的零生产者类型 ——
本该像 S1-02 的 CUSTOMER 行那样写成**留证条款**。已改为留证,并把本次实测的
0 / 0 / 3 记为 W7 落地前的旧行为基线。
