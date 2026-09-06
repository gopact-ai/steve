# Steve 架构：终态与归位（2026-09-05 refresh）

这份文档是仓库的架构权威：写终态，不写阶段；阶段性取舍只进 §9 的实施顺序，不进类型与接口。设计日志在 `docs/history/`，那里是记录，不是现状。

## 1. 定位

Steve 是一个 **agent 工作台的 hub**：把多台机器上的 coding agent（codex、Claude Code、任何 ACP 实现）组成一支可以协作的队伍，人从一个会话里给目标，hub 负责拆解、放置、隔离工作区、落地结果、把过程与结果送回会话。

三条结构性承诺没有变：**送达是平台行为**（答案由 hub 从事件流投影成消息，不靠模型自觉）；**越权在结构上不可能**（每会话独立 token，A 会话的 agent 写不进 B）；**崩溃不丢任务**（任务、attempt、结果、投递状态都是持久记录，重启后补账）。

变了的是**通道**：控制台是工作台的本体，飞书是它的一个投影。任何 IM 都是 `channel` 的一个实现，hub 核心不认识 open_id。

## 2. 分层与包

| 层 | 职责 | 包 | 只依赖 |
|---|---|---|---|
| 通道 | 入站消息 → 回合请求；出站以会话消息模型投递 | `channel`（接口与消息模型）、`channel/console`、`channel/feishu` | 协调层的请求/结果、读模型 |
| 协调 | 回合、任务树、委派、预算、上下文装配、放置 | `turn`、`task`、`delegate`、`plan`/`planner`/`exec`、`roster`、`ability`、`ctxpack`、`memory` | 记录层、执行层的接口 |
| 记录 | 每一个名字、绑定、租约、attempt、产物、落地 | `ledger`、`attempt`、`artifact`、`project`、`state`、`intent` | 无（纯本地） |
| 执行 | 在某台机器上跑一个 harness 进程、隔离运行时、反向 MCP | `node`、`nodewire`、`node/journal`、`acphost`、`harness`、`runtime`、`skills`、`agentmcp` | 记录层 |
| 投影 | 系统状态的快照 + 事件流；页面、TUI、飞书卡片都是它的渲染器 | `readmodel`、`console`（服务端）、`tui`、`web/console` | 协调层的观察者接口 |

规矩：下层不认识上层；渲染器不写状态；`view` 只是执行层向上报告"这一轮正在发生什么"的一次性形状，**不是**第二个读模型（见 §7）。

## 3. 核心对象

| 对象 | 权威 | 一句话 |
|---|---|---|
| Conversation | `console` 文档 / 通道 | 人与 hub 的一条线程；控制台叫会话，飞书叫聊天 + 话题 |
| Exchange | `console` 文档 | 会话里一次"发出去 → 回答"的交换，带 `Key` 去重、`Prompt` 与 `Input` 分离、可排队与插队 |
| Task | `task` 存储 | 一段有目标的工作；树形（委派生子）；预算可选；`Result` / `Delivery` 记子任务的结果与投递状态 |
| Attempt | `ledger` | 一次执行：租约、围栏、前后快照、用量；每台机器每个工作区同一时刻一个写者 |
| Artifact / Landing | `ledger` + 影子仓库 | 每个结果是一个提交；落地是把提交合进项目主目录，排队、冲突、已落地三态 |
| Process / Timeline | `readmodel` | 一个 agent 一回合做了什么：叙述、思考、工具按发生顺序；子任务是其中一段 |
| Delivery | `delegate` → 通道 | 子任务结束送回父会话的那条消息：先落地再说话 |
| Stream | `node` + `node/journal` | 节点上一个 harness 进程的可续接字节流；断线不死，重连补账 |

## 4. 通道是插件

### 4.1 会话消息模型

一个任务不再是一条消息。通道以**消息**为单位投递，hub 决定什么时候拆：

| 消息 | 何时 | 内容 |
|---|---|---|
| 开工 | 回合开始 | 谁在哪台机器、用什么模型接了 |
| 活动 | 时间线里连续的工具组结束、或每 N 秒 | 活动摘要行（"读了 3 个文件、跑了 2 条命令"），可展开明细 |
| 子任务 | 委派放置、结束 | 子任务卡：目标、状态、时长、它的时间线摘要、改动 |
| 续接 | 子结果送回 | "⤵ 子任务 #N 完成 · agent@node · 时长" |
| 答案 | 回合结束 | 最终回复，@ 提问人 |
| 通知 | 任务级事件 | 跑了多久、结果送到哪、需要人的审批与提问 |

每种消息由 `readmodel` 的同一个投影生成（`Reply`、`Process`、`Timeline`、`StepProcess`），通道只做渲染与投递，**不重新计算**。

### 4.2 Channel 接口

```go
package channel

type Channel interface {
    // 入站：通道把自己的消息翻译成回合请求；hub 不认识 open_id。
    Start(ctx context.Context, inbound func(Inbound)) error
    // 出站：按会话投递一条消息，返回通道内的消息 id（用于编辑、撤回、锚定）。
    Post(ctx context.Context, conversation string, m Message) (string, error)
    Edit(ctx context.Context, id string, m Message) error
    Recall(ctx context.Context, id string) error
    // 能力声明：不能编辑就每次新发；不能点按就把按钮变成一句"回复 yes / no"。
    Can() Capabilities
}
```

控制台实现 = 今天的 `console.Service`（消息就是转写行）；飞书实现 = 今天的 `gateway` + `card` + `channel/feishu` 收拢成一个包，卡片按 §4.1 逐项复刻控制台的渲染：时间线、活动摘要行、子任务卡、续接行、排队列表、引用。做不到的按能力声明退化。

### 4.3 归位清单（通道）

- `gateway` 拆成 `channel/feishu`（渲染 + SDK）和留在协调层的通用部分（resume、notify、deliver 都变成"向会话投递一条消息"）。
- `card` 并入 `channel/feishu`，输入改为 `readmodel.Reply/Process`，不再直接吃 `view.Progress`。
- `agentmcp` 通过 `channel_send / channel_update / channel_recall` 向已绑定的会话通道投递进度消息，由通道适配器渲染。`steve_recall` 仍用于查找长期记忆。
- 任务记录里的 `ChatID / AnchorMessage / ChatType / Requester` 收成一个 `Anchor{Channel, Conversation, Message, Actor}`。
- 控制台的 `Sender` 垫片、`console.IsConsole` 前缀判断消失：hub 按 `Anchor.Channel` 路由。

飞书概念今天泄漏到 hub 核心的面（codex 审计，2026-09-05）：`turn` 的 27 处（身份、ChatType / Mentioned、message_id 充当 TurnID、恢复与通知的锚点）、`task` 记录的 Requester / ChatID / AnchorMessage / ChatType / OpenCard / Interim、`delegate` 的投递、`agentmcp` 的工具名与卡片接口、`console` 的伪锚点与 `owner_open_id`、`readmodel` 透传 Requester、`i18n` 按域名定语言、`config` / `setup` / `onboard` / `schedule` 的注册与锚点、`cmd/steve` 的 19 处分流。两条渲染管线：飞书从 `turn.Result` → `view.Turn` → `card.Render`，**时间线在这条线上丢失**；控制台从 `readmodel.Reply / Process`，保留时间线与子任务但缺完整 usage / settings，且权限询问（`OnAsk / OnAskUser`）未接。

迁移六步，每步独立合入、有测试：① 中性身份与锚点（`Actor`、`Anchor{Channel, Conversation, Message}`），旧字段做迁移；② 会话与交换队列从 `console` 上提为 `conversation` 包（任何通道共用），重启 / 去重 / 续接顺序有测试；③ `readmodel` 补齐为唯一投影（usage、settings、审批、版本与顺序），与旧卡片做影子比较；④ 控制台切到 `Channel` 接口，补权限闭环；⑤ 飞书切到新渲染（快照、限流分页、失效锚点、回调重放）；⑥ MCP 工具中性名、解除启动对飞书的依赖（仅控制台可启动）。风险：重复投递、锚点过期、跨回合误更新、权限串线——靠持久 outbox、幂等键、版本与身份校验；按通道开关回退旧投递器。

## 5. 执行层

- **进程流可续接**（已落地）：节点拥有 harness 进程，流是 hub 的视图；`process_journal.v1` 协商、双向序号、`SessionGrace`。
- **委派是双工**（已落地）：子结果作为消息送回父会话，父 agent 不轮询；`steve_await` 只给"这一回合就要"的场景。
- **过程是时间线**（已落地）：acphost 的 collector 按到达顺序记 span，读模型整体携带。
- 未落地：hub 进程重启后的续接（需要 hub 侧持久化 ACP 客户端状态）；反向 MCP 写类效果在断线期间的排队。

## 6. 依赖策略

原则：**运行一个 hub 或一个 node，只需要一个二进制和一份配置**。外部程序只允许两类：用户自己的 coding agent（harness），和确实无法内置的系统能力（GPU 探测）。

决定（依据 codex 依赖审计）：

| 依赖 | 现状 | 决定 |
|---|---|---|
| Go 库：lark SDK、modernc sqlite（纯 Go）、gopact、go-qrcode、x/term、acp | 都随二进制编译，部署无需安装 | 保留；自写的代价是数千行加协议追踪，收益是零 |
| 节点侧拼出来的 sh 脚本（`artifact.Script` 经 `node.runCommand`；依赖 mkdir / find / awk / xargs / du / sed / tar / base64） | 是最脆的一层：GNU 工具差异、引号、退出码翻译 | **P0**：节点提供类型化操作（快照、检出、大小检查、归档）由 Go 实现，git 仍由 Go 侧调用；任意任务的 shell 属于 harness 自己 |
| git | 8 个直接启动点 + 脚本；go-git 覆盖对象读写、tree、ref，但 bundle、merge-tree、read-tree / ls-files 组合、大仓库内存都有风险 | 保留 git，hub 与 node 声明最低版本（≥ 2.38）并在申报里带版本；**P2** 只评估用 go-git 替换只读路径（browse / changes） |
| ACP 适配器（claude-agent-acp、codex-acp，npm） | 默认 `npx` 未锁版本；Go 重写要重做会话、流、权限、工具、恢复并追踪上游 | **P1**：steve-node 自动获取固定版本（可信清单 + SHA256 + 原子安装 + 缓存），配置里只写 harness 名；仍需 Node 运行时，写进前置条件 |
| nvidia-smi、open / xdg-open、ssh、ps、go | 可选探测、开发与测试前置 | 保留，缺失时有回退 |

## 7. 弯路归位

| 弯路 | 归位 |
|---|---|
| 三个投影：`view.Progress`（执行层报告）、`card.Turn`（飞书渲染）、`readmodel.Process`（页面） | 只留 `readmodel`；`view` 退成执行层的报告形状，`card` 只是渲染函数 |
| `gateway` 既是飞书适配又是通用的 resume / notify / deliver | 通用部分上提到协调层，飞书部分下沉为通道 |
| `console` 既是通道又是队列、转写、锚点、里程碑的宿主 | 队列与转写是**会话**的事（任何通道共用），控制台只剩渲染与 HTTP |
| `task` 的 JSON 文件与 `ledger` 的 sqlite 两套持久化 | task 迁入 ledger（同一事务、同一备份） |
| 节点侧 git 操作是拼出来的 sh 脚本 | 节点类型化操作，Go 实现（§6 P0） |
| `plan` / `planner` / `exec` 三包在委派双工之后的位置 | 计划是"多步委派 + 显式收敛"的语法糖，落在 `delegate` 之上，不另起执行器 |
| 内嵌中文文案散在 28 个非 i18n 包 | 全部经 `i18n`；英文目录同步补齐 |

## 8. 文档

- `README.md`：定位、控制台版快速开始（不需要飞书）、概念、指路。
- `docs/architecture.md`：本文。
- `docs/operations.md`：配置参考（从 `internal/config` 结构体生成）、部署 node、门禁、排障。
- `docs/history/`：`console.md`、`capability-manifest.md`、`collab-audit.md` 归档，开头注明"记录，非现状"。

## 9. 实施顺序与分工

每一步都过 `make e2e-fleet`（委派 / 落地 / 用量）与 `make e2e-autonomous`（自主拆解）两道真机门禁，飞书侧另加一个 mock channel 的对照测试；任何一步合入后 hub 与两台 node 重新部署。

| 阶段 | 事 | 谁 | 依赖 |
|---|---|---|---|
| A1 | 文档重排：README（定位、控制台版快速开始）、`docs/operations.md`（配置参考从 `internal/config` 生成、部署、门禁、排障）、旧文档归档 `docs/history/`、`config.example.json` / `node.example.json` 补齐 | codex `docs` | 无 |
| A2 | 节点类型化操作替代 sh 脚本（§6 P0），git 最低版本申报 | codex `node-ops` | 无 |
| A3 | ACP 适配器固定版本自动获取（§6 P1） | codex `adapters` | 无 |
| A4 | 中性身份与锚点（迁移 ①）、`conversation` 包上提队列与转写（②） | 我 | 无 |
| B1 | `readmodel` 成为唯一投影（③），控制台切 `Channel`（④），权限闭环 | 我 | A4 |
| B2 | `channel/feishu` 收拢 `gateway` + `card` + SDK，按 §4.1 消息模型渲染（⑤） | codex `feishu` | B1 接口冻结 |
| B3 | MCP 工具中性名、仅控制台可启动（⑥） | codex `neutral` | A4 |
| C1 | `task` 迁入 `ledger`；`plan` / `exec` 归位到 `delegate` 之上 | 我 + codex | B1 |
| C2 | 文案全部经 `i18n`，英文目录补齐；CI 内跑不依赖真机的 e2e（mockagent 起两个本地 node） | codex | A1 |

A 阶段四件并行，约一周；B 两周；C 一周。B1 之前不碰飞书渲染，避免两边同时改一个模型。

## 10. 审计原文

三份 codex 只读审计（通道耦合面、依赖内置、文档过期项）的原文存档在 `docs/history/audits-2026-09-05.md`；本文 §4.3、§6、§8 是对它们的决定。
