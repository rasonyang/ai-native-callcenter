# VC-S12-04 — 交换机侧状态真丢失后的重建 · 判定:**PASS**

执行 2026-08-22 17:30–17:53。owner 停 FreeSWITCH → 清空 mod_callcenter 的库 → 重启。
前置成立:无进行中呼叫,wei 与 ben 均 READY,两侧一致。

## 执行偏离(已确认,且它本身是一个发现)

用例原写"清空 **sqlite** `/usr/local/freeswitch/db/callcenter.db`"。**清错了库。**

```
callcenter.conf 的 odbc-dsn = pgsql://… dbname=aicc_fs      ← 实查,已生效
aicc_cc_dsn 全局             = 已设
aicc_fs 库                   = 有 agents / members / tiers 三张表,内容与现状一致
sqlite 文件最后写入           = 2026-08-21 09:40(一天前),内容陈旧
```

mod_callcenter **连的是 PostgreSQL**,sqlite 是配好 DSN 之前留下的死数据 ——
清它什么也证明不了。故本次清的是 `aicc_fs`。

**这同时更正 VC-S12-03 的判定结论**:那份 verdict 写"mod_callcenter 的 agents/tiers 存在
它自己的 sqlite 库,交换机重启时它自己就恢复了"。**归因错了** —— 自行恢复来自 PostgreSQL。
结论(重启后两侧一致、reconcile 只是确认)不变,机制说明需更正。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 清空后 agent list 为空(前置成立的证据) | owner 清空 `aicc_fs` 后重启 | ✓ |
| 日志 `agent staffing reconciled` 且 **`added=` 为正** | 见下 | ✓ |
| 重建后 agent list 含 `agent-wei\|Available`、`agent-ben\|Available` | 两者皆在 | ✓ |
| 重建后 tier list 含 wei 两条、ben 一条 | 三条齐 | ✓ |

```
17:47:50  agent staffing reconciled  agent=agent-wei  desired=2 actual=0  added=2  removed=0 failed=0
17:47:50  agent staffing reconciled  agent=agent-ben  desired=1 actual=0  added=1  removed=0 failed=0
17:47:50  agent presence mirrored to the switch  agents=3
17:47:50  queue staffing reconciled  agents=4
17:47:50  registrations reconciled   endpoints=2
```

**`actual=0 → added=2` / `added=1` 就是本用例与 S12-03 的唯一分野。**
S12-03 拿到的是 `already matched … added=0`:它证明了"重连钩子会触发且两侧一致",
**没有**证明"交换机侧真丢了时应用能推回去"。这次证明了 —— **converge 会加,不只会删**。

`failure_looks_like`(日志照常打 mirrored/reconciled,但 agent list 仍为空,
converge 只在"现实多于期望"时动作)**未发生**。

## 同批完成:队列名去掉 `@domain`(owner 直裁,C1 的根)

重建回来的 tier 起初仍带 `@192.168.31.55`,因为当时跑的是改名前的二进制。
装上新代码后完成迁移:

```
callcenter.conf 渲染      <queue name="support-en">  <queue name="support-zh">   ← 裸
switch tier list          agent-wei|support-en   agent-wei|support-zh   agent-ben|support-zh
PostgreSQL aicc_fs        support-en|agent-wei   support-zh|agent-wei   support-zh|agent-ben
带 @ 的行数               switch = 0   PostgreSQL = 0
queue list members support-en → +OK(应用的裸名字对得上交换机)
两坐席                    READY 且已注册
```

**C1 的陈旧行 `support-en@192.168.31.176` 随清库消失**,且新命名下不可能再产生同类:
队列名不再嵌入会变的主机地址。sqlite 那份死数据里仍有 4 行带 `@`,不影响任何东西
(mod_callcenter 不读它),仅记录在此。

## 判定

**PASS。** 重建路径被真正压到并通过;顺带完成了队列改名迁移,并让 C1 从
"删除要处理坏名字"变成"坏名字不存在"。
