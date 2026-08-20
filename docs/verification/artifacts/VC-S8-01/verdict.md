# VC-S8-01 — PASS(2026-08-20 17:54)

## 实测(output.txt)
- 启动期 "voice provider selected" 恰好 1 次:provider=openai(12:51 实例)。
- 该通 "ai conversation started":language=zh、provider=openai、flowId=novanet_support——
  与启动行完全一致,**language 不选 provider**(phase1-decisions A1)在线证实。
- cdrs:did=95002,language=zh,ANSWERED。
- transcripts:BOT 中文问候(感谢致电 NovaNet…)+ 中文营业时间回答——判定条款满足。
- 【留证】CUSTOMER 行数 = 0,与 SYS-8 判定一致;W1 落地后由 T6.9 转正式断言。
- failure_looks_like(provider 随语言变 / bot 说英文)均未出现。
