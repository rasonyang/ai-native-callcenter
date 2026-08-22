# VC-S13-02 — 坐席回放自己的录音,且回放不了别人的 · 判定:**PASS**

执行 2026-08-22 09:0x。只读,未产生新呼叫。app `logs/aicc-20260822-084508.log`。

## 靶子(collect 第 1 条 + 归属互斥核对)

| | call_id | recording_id | agent_ids |
|---|---|---|---|
| ben | `01a02275-8884-…` | `01a02276-2a62-…` | **{ben}** |
| wei | `01a023d4-3c2b-…` | `01a023d7-0b19-…` | **{wei}** |

**precondition 的"互不参与对方那通"必须自己去核,不能顺手取最近一通** ——
wei 最近的那通录音呼叫 `01a023ec-49fa-…` 的 `agent_ids` 是 **{ben, wei}**(一次转接),
ben 本就正当地在场。拿它做越权靶子,403 反而是错的判定,200 也证明不了越权。
改取 ben 从未参与的 `01a023d4-3c2b-…`。**此坑已回写进账本 precondition。**

## 逐条对照

| expect | 实测 | |
|---|---|---|
| `/cdrs/mine`(ben)含他自己那通 | `[{"callId":"01a02275-8884-…","hasRecording":true}]` | ✓ |
| `/cdrs/mine`(ben)**不含** wei 那通(长度=0) | `0` | ✓ |
| ben 取自己的音频:200 / bytes>0 / audio\* | `http=200 bytes=1258924 type=audio/wav` | ✓ |
| ben 取 wei 的音频:**403**,body 含 "not one of your calls" | `403` `{"error":{"code":"FORBIDDEN","message":"not one of your calls"}}` | ✓ |
| supervisor 取同一条:200 | `200` | ✓ |

`failure_looks_like` 的两个方向**都未发生**:ben 没有取到 wei 的音频(越权不成立),
ben 也没有被挡在自己那通之外(自查手段没被关掉)。
403 是**有辨别力的**而非一刀切 —— 同一个会话在自己那条上拿到了 200 与 1.2MB 音频。

## needs-FACT **F10 由此闭合**

F10(坐席取他人 recordingId 的拒绝路径)此前是**半闭**:代码侧已读,wei+ben 实测未做。
本次实测完成,拒绝形态与代码侧读出的一致(`recording_handlers.go` 的 `mayHearCall` 分支
给 403 + "not one of your calls";主管由 `Role.AtLeast(RoleSupervisor)` 早退拿 200)。
**F10 → 已闭。**

## 补强:collect 第 2 条其实什么都没验

```
- "... | jq -c '{total, mine_only: ([.items[].callId] | length)}'"
```

`mine_only` 是**条目个数**,`total` 是同一个数 —— 这一条恒等成立,不检查任何归属。
**另跑了一次真正的归属核对**:把 ben 的 10 个 callId 逐一回查 CDR,
按 `agentWasOnCall`(primary 或在 agent_ids 里)判定:

```
10 行,ben_was_on_call 全部为 t
其中 6 行 primary 是 wei —— 转接呼叫,ben 在 agent_ids 里,按规则本就该在他的列表里
```

归属规则**应用正确**。此条 collect 的空转已回写进账本。

## 本次发现:**C24 的幽灵行不只是脏数据,它带着坐席归属,并且喂给鉴权谓词**

ben 的 10 行里有一行 `01a023b6-1845-…`:`OUTBOUND`,**from 1007 → to 1007**,
NO_ANSWER,ring=0,talk=0,无主责坐席,`agent_ids={ben}`。
调出 C24 那一刻(2026-08-21 09:45)的四行:

```
01a023b6-1384-… INTERNAL NO_ANSWER 1008→1007  agents={wei}    ← 真实的那通(wei 拨 1007)
01a023b6-1845-… OUTBOUND NO_ANSWER 1007→1007  agents={ben}    ← 幽灵,却挂在 ben 名下
01a023b6-8fbc-7032 OUTBOUND NO_ANSWER voicemail→()  agents=(none)
01a023b6-8fbc-73b1 INTERNAL NO_ANSWER 1007→()       agents=(none)
```

**分机 1007 不绑任何坐席**(实查全部 20 个分机:1002=ben、1008=wei、1007=未绑定)。
这通电话与 ben 毫无关系,却写着他的 agent_id,于是**出现在他自己的通话历史里** ——
一通他没打过、发生在一部不是他的分机与它自己之间的电话。

C24 立案时记的是"每次未接产生 3 行多余 CDR"与幽灵计数被污染,
**没有记这一层:幽灵行会被**归户**到某个坐席。** 差别是实质的:
`agent_ids` 不只是报表字段,它就是 `agentWasOnCall` —— **录音鉴权用的正是同一个谓词**。
本次那一行 `has_recording=f`,**没有任何录音因此被泄露**;
但把坐席放进一通他不在的呼叫的机制,与放行录音的机制是同一个。
**C24 应从"报表脏数据"升格为"会影响鉴权输入",建议记入其条目。**

不构成本用例 FAIL:五条 expect 全中,归属规则本身应用正确,
错的是喂给它的 `agent_ids`——那是 C24 的账,不是本用例的。
