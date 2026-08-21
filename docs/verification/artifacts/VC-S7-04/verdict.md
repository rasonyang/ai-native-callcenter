# VC-S7-04 — 双向 DTMF(events.md 缺口 PARTY_DTMF)

**执行 2026-08-20 18:44** · **判定:PASS(按 expect 的 [INFERENCE] 分支)**

| expect | 实测 | |
|---|---|---|
| dtmf API 返回 202 | `202` | ✓ |
| SSE 出现 PARTY_DTMF,主叫按键方向 1 条 `digit="5"`、`durationMs>0` | `{"digit":"5","durationMs":600}`,partyId 为主叫腿 | ✓ |
| wei→远端方向的 `"4"`,`"2"`,`"#"` | **未出现任何一条** | 见下 |

## needs-FACT F2 已判定

账本 expect 里挂着的推断:

> [INFERENCE] `uuid_send_dtmf` 是否在目标通道回发 DTMF 事件未经静态证实 ——
> 若发送侧不产事件,以 `digit="5"` 一条为判定,发送效果由主叫听感确认,
> 并把实测结论回填 needs-FACT F2

**实测结论:`uuid_send_dtmf` 不在任何通道产生 DTMF 事件。** 发出的 `4`、`2`、`#`
三个键在 SSE 里一条都没有,而同一条抓流里主叫按的 `5` 正常到达。发送效果由 owner
听感确认(主叫侧听到嘟声),故 API 与 SSE 两侧都按 expect 的 [INFERENCE] 分支判 PASS。

`failure_looks_like`(digits 打在 wei 自己腿上、对端什么都没收到)**未发生**:
主叫确实听到了按键音,说明 `SendDTMF` 选的是远端腿。

**F2 回填内容**:PARTY_DTMF 只覆盖**收到的**按键,不覆盖**发出的**。
若要让坐席界面回显自己按了什么,需在 `SendDTMF` 成功后由应用侧自行 publish,
交换机不会替我们发。此项建议记入 events.md 的 PARTY_DTMF 语义说明。
