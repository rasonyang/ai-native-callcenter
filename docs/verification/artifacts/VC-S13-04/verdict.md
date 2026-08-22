# VC-S13-04 — 报表数字与 CDR 明细逐项对账 · 判定:**PASS**

执行 2026-08-22 08:45–08:52。只读,未产生新呼叫。
app 由 `deploy/dev/restart.sh` 起(`logs/aicc-20260822-084508.log`)。
四个会话(sup/wei/ben/admin)均 200 —— **顺带把昨天并入时的 admin 约定实证了**:
`admin` / `aicc@12345` 登录成功,与 `internal/seed/seed.go:42、:59、:203` 一致。

## 一、按原样执行的结果:**空窗,贯穿性 0 = 0**(见 output-as-written.txt)

```
window   2026-08-22T00:00:00Z .. 2026-08-23T00:00:00Z
overview {"totalCalls":0,"answeredCalls":0,"abandonedCalls":0,"containedCalls":0,"queueCalls":0,…}
cdrs     0|0|0|0|0
queues   []
```

五项确实"逐一相等",但**全是 0**。这不是通过,是**用例没有被压到** ——
UTC 日窗此刻只有 44 分钟大,阶段 3–5 的执行在 08-20 / 08-21。
**collect 缺陷已立案(见下 ①)**,判定不采用本次结果。

## 二、substantive 执行:两个真实日窗,逐项对账

| | 08-21 报表 | 08-21 明细 | | 08-20 报表 | 08-20 明细 | |
|---|---|---|---|---|---|---|
| totalCalls | 36 | 36 | ✓ | 74 | 74 | ✓ |
| answeredCalls | 14 | 14 | ✓ | 38 | 38 | ✓ |
| abandonedCalls | 6 | 6 | ✓ | 4 | 4 | ✓ |
| containedCalls | 1 | 1 | ✓ | 1 | 1 | ✓ |
| queueCalls | 15 | 15 | ✓ | 12 | 12 | ✓ |

per-queue 亦全等:08-21 `019ffaae…`=2 / `b098f1bf…`=13;08-20 `019ffaae…`=4 / `b098f1bf…`=8。
**两个独立日窗、十四项、零差异。** `failure_looks_like` 记的两种漂移
(时间窗边界误用 ended_at、abandoned 口径把 NO_ANSWER 全算进去)**均未发生**。

`ledger.sql:122-130` 的 FILTER 子句与 collect 第 3 条的 psql 逐字同构,
不是"数字恰好撞上",是同一套谓词。

## 三、W2 前的留证条款:**已取到证据**

```
08-21  ABANDONED_WAITING|6   (null)|30
08-20  ABANDONED_WAITING|3   SHORT_ABANDONED|1   (null)|70
```

**`ABANDONED_RINGING` 两个窗口都是 0 行**,abandonedCalls 只由 SHORT_ABANDONED 与
ABANDONED_WAITING 贡献 —— 与 expect 一致。**这正是本用例必须先于 W2 执行的那条证据**:
W2 修好条件互斥之后,这个值将变为可达,届时同样的查询会给出不同的构成。旧行为已入档。

## 四、SLA 一项:expect 说对了一半,并牵出一个 expect 没预见的缺陷

### ① expect 的措辞不准确 —— 需修订账本

expect 写"若前端把它除以 totalCalls,SLA 会被系统性低估"。
**对 `/reports/queues` 而言这是错的**:`ReportByQueue` 自带 `WHERE queue_id IS NOT NULL`
(`ledger.sql:147`),一行里的 `totalCalls` **就是**该队列的排队呼叫数,除它是对的。
该警告只对 **`/reports/overview`** 成立,那里 totalCalls 含非排队呼叫:

```
08-21  answeredWithinSla=6  /queueCalls=15 → 40%   /totalCalls=36 → 17%
08-20  answeredWithinSla=5  /queueCalls=12 → 42%   /totalCalls=74 →  7%
```

08-20 是**六倍**的低估。后端结论成立,但账本该句须限定到 overview。

### ② 新缺陷:两块屏用两个不同的分母,同一队列同一天给出两个数

| 位置 | 分母 | support-zh 08-21 |
|---|---|---|
| `web/src/routes/_app.admin.reports.tsx:128` | `row.totalCalls` | **31%** |
| `web/src/components/queue-performance.tsx:58` | `row.answeredCalls` | **57%** |

两者各自都是自洽的(后者注释明写"share of answered calls that beat the threshold"),
但它们是**两个不同的服务水平定义**,而两块屏都只标 "SLA" / 服务水平,不标口径。
主管看墙板得 57%,管理员看报表得 31%,同一个队列同一天。
行业通行的 service level 是前者(接通及时数 ÷ 呼入总数),后者衡量的是另一回事。
**立案 C27**;属呈现层(G-B2 族),不影响本用例的后端判定。

## 五、判定

**PASS。** expect 的后端断言逐条成立(14/14 相等 + 口径同构 + ABANDONED_RINGING 留证)。
第四节两项都不构成 FAIL:①是账本措辞需收窄,②是 expect 明确划归呈现层的范围之外
("本用例只断言后端数字,呈现层归 G-B2")。

## 六、本次暴露的 collect 缺陷(C23 族,已立案)

**用例的时间窗写死为"今天",而 precondition 假设"当天已有若干已收官呼叫"。**
这两条只在"起草当天就执行"时同时成立。今天(08-22)执行,窗口空,
五项 0=0 **会被读成通过**。这是最难发现的一类假通过:没有报错,没有不等式,
只有一张全 0 的表。
**修法**:窗口取参数,默认落到"最近一个有 CDR 的日窗",并在 expect 加一条闸门 ——
`totalCalls > 0`,否则判 SKIP 而非 PASS。归入下一批账本修订。
