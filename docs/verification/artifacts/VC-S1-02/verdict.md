# VC-S1-02 — PASS(2026-08-20 17:27)

## 实测(output.txt)
- cdrs 恰好 1 行且该 call_id 行数=1:INBOUND|ANSWERED|is_contained=f|bot_sec=125|total=125|95001|en|NORMAL_CLEARING。
- transcripts:seq 1–10 连续无洞,全部 BOT|TEXT|MODEL(bot 确认过营业时间问题,seq 9-10)。
- "caller leg ended, closing the model session" 计数=1。
- 【留证】CUSTOMER 行数=0——与审计 SYS-8 判定一致(stock profile 无 TranscribeModel、ASR tap 只挂坐席腿);
  W1 落地后此条转正式断言(CUSTOMER|MODEL ≥1)并重跑。
- failure_looks_like(双行 CDR / seq 烧洞)均未出现。

## 注记
- 通话中段 bot 有多轮"Are you still there?"(seq 2-5)——主叫端说话被拾取的灵敏度值得留意,
  但与本用例断言无关;若 S9(ASR)阶段听感异常再展开。
