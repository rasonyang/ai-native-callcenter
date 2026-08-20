# VC-S4-02 — PASS(2026-08-20 18:22)

- agent_states:NOT_READY | AFTER_CALL_WORK | wrap_up_call_id 有 ✓
- wrap_ups 平台代开:RESOLVED(默认词)| is_confirmed=f ✓
- GET /agent/wrap-up:{callId 非空, isConfirmed=false, dispositionCode=RESOLVED} ✓
- rejects 差分 0 ✓(FS 重复/乱序事件全被幂等表吸收)
- switch 侧 agent-wei = On Break ✓ —— failure_looks_like(ACW 期间仍 Available)不存在
