# VC-S7-03 — 保持中主叫挂断(fsm-edges 缺口边 HELD→RELEASED)

**执行 2026-08-21 10:30–10:34** · **判定:PASS**

同一通电话同时用于验证 C20 修复(见文末)。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| `rejects_now = baseline_rejects`(无新增非法转换) | baseline 0 → now **0** | ✓ |
| cdrs:`status=ANSWERED`、`talked=t` | `ANSWERED`,`talk_sec=119` | ✓ |
| `/calls` items 长度 = 0(actor 退出) | `count=0`;`show channels` 0 total | ✓ |

过程实证:hold 返回 **202**,2 秒后 wei 的腿确为 `HELD`:

```
[{"role":"ORIGINATOR","state":"TALKING","number":"18688886669"},
 {"role":"TARGET","state":"RELEASED","number":"95001"},
 {"role":"TARGET","state":"HELD","number":"1008"}]
```

主叫在听到保持音时直接挂断,`HELD→RELEASED` 被接受(`call.go` `partyTransitions[PartyHeld][TriggerRelease]`),
Finish 正常触发,CDR 落库。

`failure_looks_like`(HELD 腿的 RELEASE 被判非法丢弃、呼叫永远 RUNNING、CDR 永不落库)**未发生**。

## 收官 CDR

```
call_type INBOUND  from 18688886669  to 95001
ring_sec 54  bot_sec 0  queue_wait_sec 55  talk_sec 119  total_sec 192
status ANSWERED  hangup_cause NORMAL_CLEARING  missed_reason (null)
legs [QUEUE support-en 55s] [AGENT 1008 119s]
```

**C11 再次坐实**:`bot_sec=0`(bot 实际说了话);`queue_wait_sec=55` 与 `ring_sec=54` 几乎相等,
即振铃时间整段含在排队时长里,未扣除。

## 同场验证:C20 修复生效

| | 修复前(09:35 那通) | 修复后(本通) |
|---|---|---|
| 振铃期 `/calls/mine` 条数 | 2(真实 + 幽灵) | **1** |
| 派单腿 role/state | ORIGINATOR / DIALING | **TARGET / RINGING** |
| 派单腿所在 call 的 callType | OUTBOUND | **INBOUND** |
| 幽灵 OUTBOUND CDR 增量 | +12 | **+0**(47 → 47) |
| CDR 总数增量 | +13 | **+1**(6183 → 6184) |

振铃窗口原始抓取见 `ringing-window.txt`。
