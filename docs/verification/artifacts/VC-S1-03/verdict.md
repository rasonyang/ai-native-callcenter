# VC-S1-03 — PASS(2026-08-20 17:29)

## 实测(output.txt)
- 95999 不在 dids(=0)✓
- rejects 差分 = 0(baseline 0 → now 0):DIALING→RELEASE 合法边无一次被拒 ✓(fsm 缺口边闭合)
- cdrs 差分 = +6 —— 与 fs 日志的 **6 条** `aicc_inbound: unknown number 95999 from 18688886669`
  (17:29:11–17:29:34)逐一对应:主叫侧 .5 链路对被拒呼叫自动重试 6 次,**每次都正确入账**
  (全部 NO_ANSWER | UNALLOCATED_NUMBER | never_answered=t)。被猎形态"被拒话务漏记"不存在。
- expect 的"+1"按单次拨打假设书写;实际为 +N(N=链路重试数),账目 N↔N 对齐,判定 PASS。

## collect 缺陷回修建议(下轮账本修订)
- `fs_cli -x 'console last 100'` 返回空(console 环形缓冲不可靠);证据实际取自
  /usr/local/freeswitch/log/freeswitch.log。建议 collect#5 改 grep 日志文件。

## 环境观察(不阻塞,记档)
- 主叫链路(1000@ws.aicc.test → 192.168.31.5 → external profile)对失败呼叫自动重试 ×6:
  后续所有计数类断言须容忍 N 次尝试(本轮账本已全部差分化,天然兼容)。
- cdrs 中存在来源不明的测试行(from=tone-a/5900/0000000000、以及一条 1008→18688886669),
  时间戳显示异常;主机与容器时钟已核对同步(差 2s)。列为环境卫生观察,若干扰后续基线,
  改用 from_number/did 过滤。
