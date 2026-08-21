# VC-S12-03 — 重启 FreeSWITCH,ESL 重连后的三项重建 · 判定:**PASS**

执行 2026-08-21 20:50(owner 在终端手动 `-stop` 后重启,唯一无法脚本化的特权动作)。
前置满足:无进行中呼叫、wei 已签入 READY。

## 断连与重连

```
20:25:32  esl connected      (app 启动时的原连接)
20:50:29  esl disconnected   ← FreeSWITCH 停止
20:50:45  esl connected      ← 16 秒后重连
```

三条重建日志全部落在重连后的**同一毫秒区间**(20:50:45.7xx),顺序与 expect 一致。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 日志出现 `agent presence mirrored to the switch` | `agents=3` @20:50:45.732 | ✓ |
| 日志出现 `agent staffing reconciled` 或 `already matched` | `agent-wei desired=2 actual=2`;`queue staffing reconciled agents=4` | ✓ |
| 日志出现 `registrations reconciled` | `endpoints=2` @20:50:45.767 | ✓ |
| switch 侧 `agent-wei status="Available"` | `Available` | ✓ |
| switch 侧 tier `agent-wei\|support-en@<domain>` 存在 | `agent-wei\|support-en@192.168.31.55`(另有 support-zh) | ✓ |
| roster:wei `isRegistered=true`、`availability=READY` | `{"availability":"READY","isRegistered":true}` | ✓ |

两部话机(1008 / 1007)均已重注册。
`failure_looks_like`(agent list 为空且日志无 `mirrored`,坐席在 app 里全 READY 而 switch 里谁都不存在)
**未发生**。

## 重要保留:本次没有真正走到"重建"路径

`agent staffing already matched … added=0 removed=0` —— **一条 tier 都没有被重新添加**。
原因:mod_callcenter 的 agents 与 tiers 存在它自己的 `/usr/local/freeswitch/db/callcenter.db`
(sqlite,`callcenter.conf.xml` 里 `dbname` 保持注释即默认此库),**交换机重启时它自己就恢复了**。
我们的 reconcile 跑了,但只是**确认**,没有**恢复**。

所以本例证明的是:**ESL 重连钩子会触发,且触发后两侧一致**。
它**没有**证明:交换机侧状态真的丢了时,应用能把它推回去。

要压到后者,需要在重启前清空 mod_callcenter 的库(例如停机后
`sqlite3 /usr/local/freeswitch/db/callcenter.db "delete from agents; delete from tiers;"`),
再启动并观察 `added=` 是否为正。**建议追加为 VC-S12-04**(阶段 6 起草)。

## 附带:C1 的陈旧 tier 没有随重启浮现

重启前的推测是"mod_callcenter 从 callcenter.db 重新加载,那行陈旧 tier 可能重新进入 `tier list`,
C1 由此重获活的复现场景"。**实测未浮现**:

```
callcenter_config tier list | grep -c 192.168.31.176   →  0
sqlite3 callcenter.db "select queue, agent from tiers"  →  仍有 support-en@192.168.31.176|agent-wei
```

行还在库里,但 `tier list` 依旧看不到它 —— 因为叫这个名字的**队列**仍然不存在,
而 mod_callcenter 只列出已加载队列的 tier。与 2026-08-21 早前的判断一致:
**症状潜伏,缺陷未动**,复现需另造场景(见 `VC-S3-02/verdict.md` 追记)。
