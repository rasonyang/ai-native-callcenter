# AI Native Call Center

开源呼叫中心：AI 是默认接听者，而不是附加功能。

来电由 FreeSWITCH 接入，再由语音模型通过端到端的语音对话直接应答——这条语音链路
由应用自己终结。模型负责说话，流程负责阶段控制，并决定什么时候需要真人。需要的时
候，来电被转入真正的队列，坐席在浏览器软电话上等着接。两条路径做过的事，最后落成
同一份通话记录。

整个产品是**一个 Go 可执行文件**——REST API、事件流和全部 Web 界面都在里面——外加
PostgreSQL 和 FreeSWITCH。

[English](README.md) · [部署](deploy/README.md) ·
[设计文档](docs/design/00-overview.md)

## 先跑起来

```sh
git clone https://github.com/rasonyang/ai-native-callcenter
cd ai-native-callcenter/deploy/demo
docker-compose up -d
```

打开 <http://127.0.0.1:8080>，用 `admin` / `demo1234` 登录。数据库、交换机和应用会
一起启动，并预置一支坐席团队、两个队列、一条已发布的中英双语流程（挂在两个号码
上），以及一周的历史数据，这样看板不会是空的。在 `.env` 里填上 `OPENAI_API_KEY`，
它就能真正接听电话了。[关于演示环境](deploy/demo/README.md)

## 它做什么

**用模型应答，而不是按键菜单。** AI 分支是应用内部的一个 SIP 端点：FreeSWITCH 把
来电桥接过来，音频直接送往语音模型——只要对方接受，G.711 就原样透传，中间不做任何
解码和重采样。

**只控阶段，不写死对话。** 对话由模型主导，阶段由流程主导。每个阶段带一段指令和一
份允许调用的工具清单，转移条件基于工具返回结果触发。内置工具**可以拒绝**——"队列
已关闭"是一句需要说给用户听的话，而不是一个错误。人设、规则和机器人的音色一起版本
化并发布。

**转人工要转得像样。** 转接进入 `mod_callcenter` 队列，并带上来电者的上下文，所以
坐席的电话响起时，屏幕弹屏已经到位。坐席在浏览器里工作：状态管理、软电话条、回呼
队列。主管有实时看板、队列视图和坐席花名册。

**一通对话只有一条记录。** 机器人接听、转入队列、坐席结束的一通电话，是**一条**
CDR、一份对话文本、一个录音文件，而不是三段碎片。录音存本地文件系统，或任意兼容
S3 的对象存储。

**中英双语，并且明确告诉你在用哪个厂商。** 界面和机器人都支持中英文。一套部署只用
一个语音厂商，启动时选定：中国大陆用 `qwen`，其他地区用 `openai`。**通话的语言永远
不会用来选择厂商。**

## 结构

```
                    ┌─────────── 一个 Go 可执行文件 ───────────┐
  来电 ──▶ FreeSWITCH ──▶ SIP UAS ──▶ 语音厂商（Realtime 实时语音）
                 │           │
                 │           └─ 流程引擎：阶段、工具、转接
                 │
                 ├─ mod_callcenter 队列 ──▶ 坐席（浏览器软电话）
                 │
                 └─ ESL ──▶ 通话注册表 ──▶ REST + SSE ──▶ Web 界面
                                                 │
                                            PostgreSQL
```

FreeSWITCH 的用户目录、拨号方案和队列配置**都从数据库读取**，经由 Lua 提供。新增分
机、队列或号码是一次数据库变更；交换机只配置一次，之后不再改动。

读代码之前值得先知道两点：

* 领域模型沿用 Genesys 谱系：一通 **call** 聚合多个 **party**；分支事件是
  `PARTY_*`，整通电话级别的事件是 `CALL_*`。
* 每通在线通话都是一个 actor——单一 goroutine 作为唯一写入者，快照通过信箱获取。通
  话状态没有加锁，因为根本没有共享的通话状态。

完整设计见 [`docs/design/`](docs/design/)，从
[总览](docs/design/00-overview.md) 开始。

## 从源码构建

```sh
make dev-up            # 用 Docker 起 PostgreSQL
cd web && npm install && npm run build && cd ..
make build             # 产出 bin/aicc，界面已嵌入
./bin/aicc useradd -username admin -password '…' -role ADMIN
./bin/aicc
```

做前端时用 `make web-dev`，Vite 跑在 5173，API 在 8080。

```sh
go test -race ./...    # 一律带 -race，它在这个项目里抓到过真实的 bug
make lint              # go vet、gofmt、oxlint
make api-check         # API 契约校验
```

HTTP API 是**契约先行**的：[`docs/openapi.json`](docs/openapi.json) 是唯一事实来
源，Go 服务端和 TypeScript 客户端都由它生成。先改契约，再跑 `make api-generate`，
然后写实现，顺序不能反。

## 扩展

* **新增语音厂商**加的是一个 profile，而不是一个新客户端：
  [`docs/provider-extension.md`](docs/provider-extension.md)。
* **新增流程**是一份加载时校验的 JSON，可参考
  [`internal/seed/flows/`](internal/seed/flows/) 里那条双语流程。
* **新增页面**遵循 [`web/CLAUDE.md`](web/CLAUDE.md) 里的设计系统——那是硬性约束，
  不是建议。

## 当前状态

人工路径、AI 路径、产品界面和打包都已完成，并在真实 FreeSWITCH 与真实厂商上验证
过。

性能目前**还不是**本项目对外给出的承诺。设计里有容量预算和延迟目标
（[设计 06](docs/design/06-capacity.md)），压测所需的工具也已经在仓库里
（[docs/load-tests.md](docs/load-tests.md)），但正式的压测 benchmark 还没有做——所
以那份预算目前只是目标，不是实测结论。

有一件事是**明确不做**的：在本进程内做 ASR + LLM + TTS 的级联流水线。那种组合应当
是一个独立服务，说同一套协议。

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。
