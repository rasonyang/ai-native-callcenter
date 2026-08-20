# VC-S9-01 — FAIL(2026-08-20 18:37;链路全通,质量条款不达,C14 立案)

## 通过的条款
- 日志链:transcription tap attached → tapped stream connected → transcription live(同 call,18:37:34-35)✓
- SSE:CALL_TRANSCRIPT isFinal=false partial ≥2 ✓
- DB(source=ASR):HUMAN_AGENT 行 has_agent=t、CUSTOMER 行 has_agent=f、provider=openai,
  seq 5-11 与 MODEL 行共序无冲突(uq 不炸)✓

## 不达的条款(→ FAIL)
- isFinal=true 含 "quick brown fox":未命中——fox 句被识别为 "Butro focus jobs owing the lazy workin"
  (seq 11),CUSTOMER 侧同样破碎("Brew forcement"/"Joss Are the 多")。
- 无 "transcribe: audio was dropped":**双侧告警**——HUMAN_AGENT sent=1146 dropped=46(4.0%),
  CUSTOMER sent=1084 dropped=78(7.2%)。
- 因果判读:pump 计数器正是为分开"引擎听错"与"我们没送到"而设(pump.go:88-90 注释)——本次是**没送到**,
  丢帧率足以解释识别质量崩坏。

## 立案 → TASKS C14
ASR tap 摄取路径丢帧(pump 背压/缓冲不足候选),修复后重跑本用例。
