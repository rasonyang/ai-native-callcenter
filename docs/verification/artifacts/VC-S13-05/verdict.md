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

---

## 重跑 —— 2026-08-22 12:01–12:09Z(C28 已修,commit `6b79d4c`)

**判定:PASS**(首轮 FAIL 的两条全部转绿,其余五条维持)

### 失联后

| 断言 | 首轮 | 本轮 | 实测 |
|---|---|---|---|
| 1008 从注册表消失 | ✓ | ✓ | Sign Out 主动发了 unregister,**没等到期** —— 首轮等了几分钟,这轮 3 秒内就没了 |
| `availability=DEVICE_UNREACHABLE`、`isRegistered=false` | ✓ | ✓ | `{"state":"READY","availability":"DEVICE_UNREACHABLE","isRegistered":false}` |
| `state` 仍为 READY | ✓ | ✓ | 同上 —— 掉线不改坐席的意愿,只改可达性 |
| SSE 发出 `DEVICE_UNREGISTERED` | **0** ✗ | **1** ✓ | 见下 |
| switch 侧 agent-wei 不再 Available | 持续 `Available` ✗ | **`On Break`** ✓ | `callcenter_config agent list` 第 6 列 |

wei 名下 `DEVICE_*` 的完整序列(supervisor 流,同一 agentId):

```
seq=10300011 12:06:30Z DEVICE_IN_SERVICE    payload.availability=READY               state=READY   ← 基线续注册
seq=10300012 12:07:29Z DEVICE_UNREGISTERED  payload.availability=DEVICE_UNREACHABLE  state=READY   ← 掉线
seq=10300014 12:08:40Z DEVICE_IN_SERVICE    payload.availability=READY               state=READY   ← 恢复
```

对照首轮那三条 —— 当时第三条顶着 `DEVICE_IN_SERVICE` 的名字、载荷写着 `DEVICE_UNREACHABLE`,
名与实相反。这轮**名字和载荷说的是同一件事**,按 `type` 过滤的消费者和读载荷的消费者第一次会得到同一个结论。

交换机镜像同步走了一个完整来回:`Available → On Break → Available`。

### 恢复后

| 断言 | 实测 |
|---|---|
| roster 回到 `availability=READY`、`isRegistered=true` | `{"state":"READY","availability":"READY","isRegistered":true}` ✓ |
| SSE 出现 `DEVICE_IN_SERVICE` | 计数 3(基线 1 + 恢复 1 + ben 的 1)✓ |
| switch 侧回到 `Available` | ✓ |

`DEVICE_REGISTERED` 本轮仍为 **0**,与预期一致 —— C28 没有动它。恢复走 `DEVICE_IN_SERVICE`,
两者的分工是语义问题(注册成功 vs 可服务),留给 W7,不是补一行 publish 的事。

### 顺带证明的一条

基线本身就是 C28 的反向检查:wei `READY` 且话机在册时,switch 侧是 `Available` ——
新加的可达性判断没有把好话机误判成不可达。

### 执行中两处自己的错(留档)

- `POST /api/v1/agents/me/ready` 拿到 `http=200`,**那个 200 是假的**:该路径不存在,
  落到了内嵌 SPA 的 index.html。真实路径是 `/api/v1/agent/ready`(`docs/openapi.json` paths)。
  **凡对写接口只看 http_code 不看副作用的采集都可能被这样骗过** —— 当时 roster 没变才露出马脚。
- 头写成 `CSRF='-H X-AICC-Csrf: 1'` 再 `$CSRF` 展开:**zsh 不对无引号变量做分词**,
  整串被当成一个参数,curl 收不到头,继续 403。写成字面量才对。
