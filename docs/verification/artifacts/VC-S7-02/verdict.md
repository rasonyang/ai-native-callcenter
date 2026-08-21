# VC-S7-02 — 通话中转接给第二坐席

**执行 2026-08-21 09:35–09:39** · **判定:PASS(带偏差)**

## 偏差声明

账本 precondition/collect 写的转接目标是 **1002**(ben 的绑定分机)。实测 1002 无话机注册,
ben 的浏览器话机始终卡在 "Answering…" 无法完成应答(见 VC-S7-02/answering-stuck 记录)。
本次改用 **1007**(Telephone 1.6 原生软电话)承载第 2 坐席:ben 经 API 签入 1007 并置 READY。
`callcenter_config agent list` 实证 `agent-ben | user/1007@192.168.31.55 | Available`。
转接目标为分机号,与被测代码路径(coordinator 选幸存腿 → adapter `uuid_transfer`)无关,
故偏差不影响结论;账本 collect 的 `1002` 建议改为"任一已注册的第 2 坐席分机"。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| transfer 返回 202 | `http=202` | ✓ |
| show channels:主叫腿仍在 | `01a021f5-0cd3…` inbound `sofia/external/18688886669@192.168.31.5` CS_EXECUTE dest=1007 app=bridge | ✓ |
| wei 的腿消失 | `uuid_exists 01a021f6-3e4f…` → **false**;channels 只剩 2 条 | ✓ |
| ben 的腿振铃/接通 | `01a021f7-668f…` outbound `sofia/internal/1007@192.168.31.55:61162` CS_EXCHANGE_MEDIA | ✓ |
| 主管 /calls:同一 callId 下主叫腿 TALKING + ben 腿 agentId 非空 | 单一 callId `01a021f5-0ce9…`,ORIGINATOR TALKING otherNumber=1007;TARGET 1007 TALKING | ✓ |
| wei 的 /calls/mine 长度=0 | wei 的 party 转 `RELEASED` ✓(长度断言在通话结束后才取到,非净读——以 party 状态判定) | ✓* |

`failure_looks_like`(uuid_transfer 打错腿、主叫被挂断)**未发生**:主叫腿存活并与 1007 通话 35 秒。

## 收官 CDR(单条,正确)

```
call_id 01a021f5-0ce9-7403-a820-4659a425b02c
call_type INBOUND   from 18688886669   to/did 95001
primary_agent_id wei     agent_ids {wei, ben}
ring_sec 16  bot_sec 0  queue_wait_sec 135  talk_sec 58  total_sec 191
status ANSWERED   hangup_cause NORMAL_CLEARING
legs [QUEUE support-en 135s] [AGENT 1008 58s] [AGENT 1007 35s]
```

转接后**仍是一条 CDR**、两个坐席都进了 `agent_ids`、三段 leg 齐全 —— 转接的账目正确。

## 顺带复现的既有缺陷(不影响本例判定)

- **C11 再次坐实**:`bot_sec=0`,但本通电话 bot 确实说了 3 句(转写面板可见)。
  转接后的 CDR 丢失 bot 时长;`queue_wait_sec=135` 亦含振铃时间(`ring_sec=16` 未从中扣除)。
- **新发现,另立 C20**:同一通排队电话产生了 **12 条幽灵 OUTBOUND CDR**(`1008 → 18688886669`,
  NO_ANSWER,total_sec=0)。详见 `docs/verification/artifacts/VC-S7-02/phantom-delivery-calls.md`。
