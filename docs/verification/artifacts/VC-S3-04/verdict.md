# VC-S3-04 — 队列停用 → bot 改为留言 → 回访单全链 · 判定:**PASS**

执行 2026-08-21 16:01–16:05。窗口内无并行呼叫。
呼叫 `01a02356-9c09-7563-bbe6-238b81fa3063`,回访单 `01a02357-2c35-7bac-a6a5-e2d9e3388c73`。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| callbacks 新增 1 行,`status=OPEN` | 2 → 3,新行 `OPEN` | ✓ |
| `phone_number` 含 13800000000 | `13800000000`(bot 从对话里取到,未走主叫号兜底) | ✓ |
| `message` 非空 | `Caller says their order is late and requests a callback.` | ✓ |
| SSE `CALLBACK_CREATED` 计数=1 | **1**(见下方 collect 缺陷) | ✓ |
| claim 返回 `CLAIMED` | `"status":"CLAIMED"` | ✓ |
| complete 返回 `DONE` | `"status":"DONE"`;DB `DONE\|handled_by 非空\|handled_at 非空` | ✓ |
| SSE `CALLBACK_UPDATED` 计数=2 | **2**(CLAIMED 一条、DONE 一条) | ✓ |
| 最后一条恢复返回 true | `{"name":"support-en","isEnabled":true}`;DB `support-en\|t` | ✓ |

`failure_looks_like`(bot 口头答应留言但 callbacks 无新行、SSE 计数=0)**未发生** ——
这正是本用例存在的理由,本次全链贯通。

**放行门已满足**:`is_enabled=true` DB 实测通过,队列在交换机侧仍在,wei 已恢复 READY。

## collect 缺陷三处(回修建议)

### ① 队列启停需要 ADMIN,collect 用的是主管会话

第 1、8 条用 `/tmp/vc-sup.jar`,实测:

```
PUT /api/v1/queues/{id}
→ 403 {"code":"FORBIDDEN","message":"insufficient role","params":{"requiredRole":"ADMIN"}}
```

与 **C12**(`/calls/waiting` 拒绝主管)同族:账本多处默认"主管会话万能",实际角色门更细。

```diff
-      - "... curl -s -b /tmp/vc-sup.jar ... -X PUT http://127.0.0.1:8080/api/v1/queues/$QID ..."
+      # 队列启停是 ADMIN 权限,主管会话 403。先 login admin:
+      #   curl -s -c /tmp/vc-admin.jar -X POST .../auth/login -d '{"username":"admin","password":"…"}'
+      - "... curl -s -b /tmp/vc-admin.jar ... -X PUT http://127.0.0.1:8080/api/v1/queues/$QID ..."
```

### ② `grep -c 'CALLBACK_CREATED'` 会数出 2

SSE 每个事件产生两行(`event: CALLBACK_CREATED` 与 `data: {…"type":"CALLBACK_CREATED"…}`),
裸串计数把一个事件数成两个。本次实测 74/75 两行、真实事件数 1。

```diff
-      - "grep -c 'CALLBACK_CREATED' /tmp/vc-s3cb-sse.log"
+      - "grep -c '\"type\":\"CALLBACK_CREATED\"' /tmp/vc-s3cb-sse.log"
```

同一缺陷影响第 7 条的 `CALLBACK_UPDATED` 计数(expect 写 2,裸串会数出 4)。
**本判定的两个计数均按 data 行统计**,故 1 与 2 都是真实事件数。

### ③ SSE 抓流 `--max-time 120` 太短

从"开启抓流"到人工拨号、对话、挂断,120 秒不够。本次用 1800 秒。
同一问题在 VC-S6-01 首跑已导致 SSE 条款漏抓(见该用例判定)。建议 S3/S5/S6/S7 各条
统一改为 `--max-time 1800`。

## 本用例同时关闭的文档缺口

- events.md:`CALLBACK_CREATED` / `CALLBACK_UPDATED` 两个事件首次取得实证 payload
- tables.md:`callbacks` 表的场景缺口(此前只进不出)—— OPEN → CLAIMED → DONE 全链贯通
