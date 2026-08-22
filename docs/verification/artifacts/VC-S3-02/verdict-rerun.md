# VC-S3-02 重跑 — DB 期望配员 vs mod_callcenter 现实 · 判定:**FAIL → PASS**

重跑 2026-08-22 18:03。纯脚本,无人工步骤。
前置:app 运行中,wei 与 ben 均已签入 READY。

## 逐条对照

```
DB 期望(第 1 条)          switch 现实(第 2 条)
agent-ben|support-zh        agent-ben|support-zh
agent-wei|support-en        agent-wei|support-en
agent-wei|support-zh        agent-wei|support-zh
                    diff 为空
```

`agent list`:两名已签入坐席各一行,`Available`(DB 为 READY),
contact 各带其绑定分机 —— `user/1002@…`(ben)、`user/1008@…`(wei)。

`failure_looks_like` 两个方向**均未发生**:switch 上没有 DB 没有的 tier,DB 里也没有 switch 缺的。

## 这次的通过是**结构性**的,不是上次那种假通过

2026-08-21 复查时的结论是"**症状潜伏、缺陷未动**,此时重跑会得到假通过":
陈旧行 `support-en@192.168.31.176|agent-wei` 仍在库里,只是因为同名队列早已不存在,
`tier list` 列不出它,diff 才恰好为空。

现在不同:
- 那一行已随 VC-S12-04 的清库消失(实测两侧带 `@` 的行数均为 **0**)
- 而且**同类的行再也产生不出来** —— 队列名不再嵌入会变的主机地址(owner 直裁,
  `QueueName`/`aicc_xml.lua`/`aicc_queue.lua` 三处从源头去掉 `@domain`)
- 万一还有历史遗留的限定名,`bareQueueName` 现在在第一个 `@` 处截断,
  converge **认得出**它因而删得掉 —— 认不出的 tier 就是删不掉的 tier,那正是它当初活下来的原因

## 但 C1 只修了一半,另一半有活证据证明仍在

C1 立案时是两件事,本次只根治了第一件:

| C1 的两半 | 状态 |
|---|---|
| ① 删除对含域名字二次限定 | **已根治**(名字不再含域;修法由 owner 直裁改为"从源头去掉") |
| ② `converge` 的 `removed=` 以复查为准 | **未修** |

直接探针(本次执行,未破坏现状):

```
callcenter_config tier del does-not-exist agent-wei   → +OK
callcenter_config tier del support-en agent-nobody    → +OK
```

**交换机对 miss 的删除同样回 `+OK`**,而 `converge`(`catalog/service.go:288-295`)
只要 `DeleteCallcenterTier` 返回 nil 就 `removed++`,**不复查**。
所以 `removed=` 依旧可能谎报"我删掉了一条其实不存在的 tier"。

**这不影响本用例的判定**:VC-S3-02 断言的是 DB↔switch 的一致性,而那是真的一致;
`removed=` 的谎报是另一回事,本例不断言它。**C1 条目应保留开启状态**,
待其第二半修复后另行验证(它需要的是一个"删后复读 tier list"的断言,不是本例这种全量 diff)。
