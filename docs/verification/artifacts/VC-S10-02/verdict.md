# VC-S10-02 — PASS(2026-08-20)

## 实测
- 前置核对:show channels = 0 total;重启前最大 id OLD=6800107;seq_blocks events=6900000。
- deploy/dev/restart.sh:一次成功,输出 "started: logs/aicc-20260820-125146.log"(advisory-lock 等待循环生效,无竞态)。
- 带过期 Last-Event-ID:5 重连:首帧即
  `event: SYSTEM_RESET` / `data: {"version":1,"seq":0,...,"payload":{"oldestSeq":0}}`(reset.log)——
  ring 为空(hub.go:132 oldest==0)判定正确;seq=0 故无 id: 行(writeEvent 语义)。
- 重启后首个业务事件 id=6900001(= 新块下界 upper−blockSize+1,seq.go:63),> OLD 6800107 → 全局单调 ✓;
  seq_blocks 6900000 → 7000000,只增不减 ✓(重启烧块为设计内行为)。
- 附带:新实例启动 15ms 内完成三重建("agent presence mirrored"、"agent staffing reconciled"、
  "registrations reconciled endpoints=1",logs/aicc-20260820-125146.log 12:51:46)—— S12-03 的机制在 app 侧重启路径上已顺带取证。
- 该启动日志同时又出现 "agent-wei ... removed=1 failed=0" 而陈旧 tier 仍在 —— VC-S3-02 缺陷的第 5、6 次复现。

## 遗留状态
- wei 已恢复执行前状态:签入 + READY @1008(post-restart-events.log 中 AGENT_LOGGED_IN/AGENT_READY 即恢复动作本身)。
