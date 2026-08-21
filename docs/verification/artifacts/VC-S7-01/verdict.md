# VC-S7-01 — 保持 / 取回

**执行 2026-08-20 18:43** · **判定:PASS**

| expect | 实测 | |
|---|---|---|
| hold/retrieve 均返回 202 | `hold: 202` / `retrieve: 202` | ✓ |
| SSE:PARTY_HELD 1 条、PARTY_RETRIEVED 1 条 | 各 1 条 | ✓ |
| `/calls/mine`:wei 的腿回到 TALKING,主叫腿 TALKING | ORIGINATOR TALKING + TARGET(wei)TALKING | ✓ |
| 日志无新增 `transcription tap refused a command` | 无输出 | ✓ |

`failure_looks_like`(交换机真保持了但 SSE 无 PARTY_HELD、主管墙上仍显示通话中、
转写 tap 不暂停)**未发生**。

`HELD→RELEASED` 这条边由 **VC-S7-03** 单独覆盖,本例只走 `TALKING→HELD→TALKING`。
