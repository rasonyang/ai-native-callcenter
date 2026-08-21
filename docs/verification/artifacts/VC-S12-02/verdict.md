# VC-S12-02 — 人工通话中重启 app · 判定:**PASS**

执行 2026-08-21 20:25。wei 与主叫通话中执行 `deploy/dev/restart.sh`。
**owner 听感:重启后"还能互相听见"** —— 媒体未中断。

本用例的 expect 本身就是一份**已知缺口的既成事实**记录,四条全部符合。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 重启期间 `show channels count` = 2 total(媒体不掉,FS 独立于 app) | 重启前 2、重启后 **2**;owner 确认双方仍能互相听见 | ✓ |
| 重启后 `/calls` items=0【已知缺口】 | `{"count":0}` | ✓ |
| wei 的 `agent_states` 恢复为重启前值 | 前 `READY\|` → 后 `READY\|` | ✓ |
| 挂断后 5 秒 `cdrs_now = baseline_cdrs`(该通话**没有**新行) | 6206 → **6206**,最近两行分别是更早的 12:07 与 10:44 那两通 | ✓ |

`restart.sh` 输出 `started: logs/aicc-20260821-202532.log`;新实例日志中
`adopt` / `CHANNEL_HANGUP` / `could not write the cdr` 三者**计数均为 0** ——
新实例从头到尾**不知道这通电话存在过**,挂断事件落在它不认识的 channel 上被 `Dispatch` 丢弃。

## 比 expect 更严重的一处(本次新记)

expect 只说"进行中的人工通话对新实例不可见"。实测还有一层:
**重启后 wei 在应用里读作 `availability: READY`,而他正在跟人通话**(交换机侧 2 条 channel、
双方听感正常)。`SetOnCall` 由呼叫事件驱动,registry 空了就没人被标记在通话中。

后果不是"墙上少一通电话",而是**一个正在讲话的坐席被系统当作空闲** ——
下一通排队呼叫会被派给他。一次 deploy 窗口里,每个在讲的坐席都处于这个状态。

这一层与 expect 记的账面损失同根(registry 不恢复),但影响面更靠前:
账面损失是事后对不上账,这一层是**当场把电话派给忙着的人**。
建议随"show channels 对账恢复"一并处理(见 `failure_looks_like` 末句)。

## 与 VC-S12-01 的关系

S12-01 执行时,被重启打断的呼叫同样**一行 CDR 都没有** —— 那正是本用例钉的这件事,
在另一个场景里提前显形。两例互为佐证:**app 重启窗口内的所有在途呼叫都不进账本**,
无论它当时在 bot 手上还是在人手上。
