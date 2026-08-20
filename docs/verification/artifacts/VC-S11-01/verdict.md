# VC-S11-01 — PASS(2026-08-20)

## 实测
- API 返回链:login→{NOT_READY,LOGIN,1008} → READY → {NOT_READY,BREAK} → READY → LOGGED_OUT(全部 200)。
- agent_state_logs(时间正序后 5 行):NOT_READY|LOGIN → READY → NOT_READY|BREAK → READY → LOGGED_OUT —— 与 expect 完全一致。
- SSE 计数(sse.log):AGENT_LOGGED_IN=1、AGENT_READY=2、AGENT_NOT_READY=1、AGENT_LOGGED_OUT=1 —— 与 expect 完全一致。
- switch 侧:agent-wei status="Logged Out"(fs_cli agent list 第 6 字段)—— 与 expect 一致。

## 执行偏差(账本 collect 命令的两处缺陷,需回修 ledger.yaml,不影响判定)
1. 账本用 `| tac` 反转 state_logs —— macOS 无 tac,已用等价的 `| tail -r` 执行。
2. 账本用 `awk -F'|' '{print $3}'` 取 agent list 状态 —— 实测状态在第 6 字段($3 是空的 uuid 列),
   已按 $6 执行;VC-S4-02 / VC-S4-03 / VC-S12-03 的同类行需一并修正。
3. SSE 计数行账本缺 `sort`(uniq -c 对交错序列会拆行),已按 `sort | uniq -c` 执行。

## 遗留状态
- 用例结束时 wei 为 LOGGED_OUT;将在全部纯脚本用例跑完后恢复为签入+READY(执行前的原始状态)。
