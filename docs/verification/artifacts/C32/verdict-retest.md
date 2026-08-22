# C32 重测(2026-08-22,aicc context 改动之后)· **内部一型:不再复现;外线一型:被中继阻断**

C32 的诊断做于今日改 aicc context **之前**,故先重测再决定是否开工。

## 内部一型:**症状消失**,而且顺带把 VC-S14-04 的空白补齐了

`wei(1008,浏览器 SIP.js)→ 1002` 经 API click-to-dial:

```
18:22:49  wei 腿 CS_EXECUTE(上次死在 CS_CONSUME_MEDIA,从未到达)
          1002 腿被拨起 → CS_EXCHANGE_MEDIA,owner 接听
          FS 日志 Transfer sofia/internal/fugn18u1@… ← transfer 真的执行了
          应用日志 click-to-dial transfer failed 计数 = 0
```

**首次诊断的机制是"`CHANNEL_ANSWER` 从未发出 → arm 的回调不触发 → `TransferToExtension` 根本没被调用"。
这次它被调用了。** 症状不复现。

**根因仍未证明。** 当时的怀疑(`absolute_codec_string=PCMU` 在 DTLS/WebRTC 腿上使媒体协商无法收官)
**从未被验证过**,而今日之间发生了多项交换机侧变更(FreeSWITCH 重启、`internal_auth_calls=true`、
internal profile `context=aicc`、`TransferToExtension` 目标 context 由 `default` 改 `aicc`)。
**因此本条记为"不再复现,原因未证明",不是"已修"** —— 未经解释的痊愈会静默复发。
守卫由 VC-S14-04 承担(见下),它是可重跑的。

### 顺带完成:VC-S14-04 从未跑成的两段

**能力限制(C18 首次现场验证,此前只有单测)**:

```
hold      → 409 OPERATION_NOT_ALLOWED_FOR_CALL_TYPE  "this is not available on an internal call"
retrieve  → 409  同上
transfer  → 409  同上
mute      → 202        unmute → 202
```

**账面(内部一型 expect 逐条)**:

```
INTERNAL | ANSWERED | 1008->1002 | ring=2 talk=245 bill=0 total=249 | agents=2
legs=[{kind:AGENT, label:1002, durationSec:245}]
CDR 6259 → 6260 —— 恰好 1 行,零幽灵
```

`talk_sec` 是两条坐席腿的**并集**(245,不是 490)、`bill_sec=0`(分机互拨无运营商计费)、
`legs` 只含被叫腿 —— 全中。**C15/C17/C18 的成果现场确认。**

## 外线一型:**无法判定,被中继阻断**

两次 API 拨 `18688886669` 均失败,形态一致且**可复现**(非偶发):

```
应用:click-to-dial placed(无错误)
FS:  New Channel … → 1.13 秒后 Originate Resulted in Error Cause: 487 [ORIGINATOR_CANCEL]
     Cannot create outgoing channel of type [user] cause: [ORIGINATOR_CANCEL]
CDR: OUTBOUND | NO_ANSWER | 1008->18688886669 | NO_ROUTE_DESTINATION —— 恰好 1 行,零幽灵
```

**但根因不在 click-to-dial。** 排除过程:

1. 手工 `originate` 完全模拟应用的变量集(含 `aicc_call_id`、`aicc_call_type=OUTBOUND`、
   同样的 `origination_caller_id_*`),**三次全部 `+OK` 成功** —— originate 本身与变量集无罪
2. `AICC_SWITCH_DOMAIN` = `192.168.31.55`,与交换机一致;同一 `Endpoint()` 在 18:22 的内部呼叫里работал
3. **直接经网关拨,绕过 click-to-dial**:

```
originate …sofia/gateway/pstn_sim/18688886669 &park()   →   -ERR NO_USER_RESPONSE
```

网关 `Status UP`,主机 `192.168.31.5` ping 通(0% 丢包),但 **SIP 对端不应答 INVITE**。
2026-08-20 时这条中继是通的(C17 记录 `OUTBOUND 1008→18688886669 ring=2/talk=9`),中间有变化。

**结论**:外线一型的失败**先于** click-to-dial —— 中继现在拨不出去。
待模拟器/对端恢复后重测,方能判定 VC-S14-04 的外线一半。
