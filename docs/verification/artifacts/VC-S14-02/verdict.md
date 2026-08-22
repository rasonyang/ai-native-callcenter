# VC-S14-02 — 队列配员 → tier 生效 → 撤销 · 判定:**PASS**

执行 2026-08-22 12:59–13:06(UTC 04:59–05:06)。app `logs/aicc-20260822-084508.log`。
一通真实呼叫(`01a027d9-bdfb-…`,95001)。环境已复原:两坐席均 READY,tier 回到基线三条。

## 前置(全部现读,不臆造)

```
ben   READY  extensionNumber=1002  isRegistered=true   contact=user/1002  status=Available
wei   NOT_READY(隔离,不参与派单)                        contact=user/1008  status=On Break
tier  agent-ben|support-zh  agent-wei|support-en  agent-wei|support-zh     ← ben 不在 support-en
```

**ben 的分机是执行时读出来的 1002,不是用例里写死的号码**。同日查明:
`agent_states.extension_number` 是**粘的**,08-21 留下的 1007 跨天跨重启一直没被重读;
签出再签入即从配置(`seed.go:63` = 1002)重取,交换机 contact 随之变为 `user/1002`。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 配员:2xx | `http=204` | ✓ |
| 2 秒内 switch 侧出现 `agent-ben\|support-en@<domain>` | 出现 | ✓ |
| (修订后)`tier not applied on the switch` 警告计数 = 0 | `0` | ✓ |
| 撤销:2xx | `http=204` | ✓ |
| 2 秒内该 tier 消失,**以 `tier list` 复查为准** | 复读回到基线三条,`agent-ben\|support-en` 计数 **0** | ✓ |
| 应用侧撤销后不再含 ben | `{"contains_ben":false,"n":3}`(回到 wei/chen/amy) | ✓ |

## 关键的一层:配员**真的参与了路由**,不只是躺在 list 里

ben 基线上**不在** support-en。配员之后拨 95001(→support-en),`agent list` 的逐拍记录:

```
05:03:04  agent-ben = Available/Waiting          ← 空闲
05:03:14  agent-ben = Available/Receiving        ← 被派单,1002 在响
05:03:20  agent-ben = Available/In a queue call  ← 接起
```

同期 `agent-wei = On Break/Waiting` 全程未动 —— 隔离成立,这一通只可能来自 ben 的新 tier。落库:

```
01a027d9-bdfb-… | did=95001 | ANSWERED | primary=ben | queue=support-en | talk_sec=43 | queue_wait_sec=4
```

**幽灵核对**:本次窗口 CDR 恰好 **1 行**(`18688886669->95001`),无多余行。

**起草时漏了这一层断言**:原 expect 只查 tier 是否出现在 list 里,而"出现在 list"与
"真的被派单"是两件事 —— C1 正是"list 里有一条却指向不存在的队列"的反例。
已把派单链与 CDR 归属补进 expect。

## 用例修订三处(都是我起草时错,与产品无关)

1. **请求体字段名错了**:草案写 `agentIds` 数组;契约是 `StaffQueueRequest`,
   **必填 `agentId`(单数)**,另有可选 `level`/`position`(省略时 handler 各置 1,
   `catalog_handlers.go:117-122`)。写成数组会 decode 出 `agentId=uuid.Nil`。
2. **note 的警告是反的**:原写"PUT 是全量替换,必须把 wei 一起带上否则会把他撤下来"。
   实际 `StaffQueue` → `SetQueueAgent`(`catalog/service.go:443`)是**单个坐席的 upsert**,
   **wei 从来不在风险里**。
3. **日志断言找错了子系统**:原写"日志应显示 `agent staffing reconciled` 且 `added=1`"。
   那两行来自 **ESL 重连时的对账**;`StaffQueue` 直接调 `AddCallcenterTier`
   (`service.go:450-454`),**成功时不打任何日志**,失败才 `Warn "tier not applied on the switch"`。
   实测抓到的三行 `agent staffing …` 时间戳是 **40 分钟前**的旧行,与本次操作无关 ——
   若不核时间戳,会误读成"配员走了对账路径且 already matched"。
   已改为断言 Warn 计数=0,落地一律以 `tier list` 复查为准。

## C1 的规矩在本例被遵守

撤销那一步**没有采信任何 `removed=` 数字**,只认 `tier list` 的复读 ——
C1 已证实 mod_callcenter 对 miss 的 tier del 也返回 `+OK`。本次复读结果与期望一致。
