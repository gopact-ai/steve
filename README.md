# Steve

[English](README.en.md)

Steve 是面向个人的多机 agent 工作平台：通过 Web 控制台（飞书是可选通道）管理项目、会话与任务，按机器能力执行和委派工作，记录过程、产物、审批与恢复状态。

Steve 提供三条结构性承诺：

- **平台负责交付**：回复由平台渲染和记录；委派结果持久化后主动送回父会话，附上实际落地状态。
- **执行有明确边界**：机器按实际申报准入，项目权限与数据等级约束执行位置；内置 MCP 使用会话绑定的 token，工具权限由策略处理。
- **工作有账可查**：任务、attempt、产物与落地状态进入账本；中断后按恢复规则续跑或明确记录失败，保留审批与对外动作的对账入口。

## 从控制台开始

控制台可以独立运行，无需飞书应用。独立部署配置 `gateway.owner_id`；需要飞书/Lark 时，再配置成对的应用凭据及该通道的 owner。两种启动方式共享项目、任务和账本。

准备 Go 1.27+、Git、Node.js 与 npm，以及一个已完成认证的 coding agent。内置适配器 `codex-acp`、`claude-agent-acp` 由 Steve 按固定版本下载和校验；首次取用需要 npm registry，之后使用本机缓存。

1. 在仓库根目录构建并复制最小配置：

   ```bash
   make build
   cp config.console.example.json config.json
   chmod 600 config.json
   ```

2. 编辑 `config.json`：把 `projects.workspace.home.path` 改成已有项目目录，`gateway.owner_id` 改成部署使用的稳定 owner 标识。默认只使用 codex，权限策略为 `read`。需要其他工具时调整 `agents` / `harnesses`；自备适配器可使用绝对路径 `command`，与 `adapter` 二选一。

   [config.console.example.json](config.console.example.json) 是不带飞书的最小配置；[config.example.json](config.example.json) 展示多机、区域、MCP 和可选飞书配置，使用前需要替换占位值并删除不用的部分。

3. 体检后启动 Hub：

   ```bash
   ./steve doctor -config config.json
   ./steve run -config config.json
   ```

   doctor 会准备运行目录并启动已配置工具进行探测，不是线上只读健康检查。只有配置了飞书凭据时才验证并连接飞书；独立控制台会在本地准备身份和记忆文件。

4. 保持 run 运行，在另一终端打开控制台：

   ```bash
   ./steve dash
   ```

   默认地址是 `http://127.0.0.1:7710`。在工作台发送 `/project use workspace`，再发送 `@codex 列出这个项目的文件并说明用途`。工具需要超出 `read` 策略的权限时，可以在本轮权限请求中明确批准或拒绝，详见 [权限说明](docs/operations.md#harnessesname)。

需要飞书/Lark 时，可用 `./steve setup` 录入已有应用，或用 `./steve setup -create-app` 走官方设备流；确认应用下自己的 `open_id`。配置完成后，飞书与控制台可同时使用。远程访问、地址与 token 参数见 [控制台与凭据](docs/operations.md#控制台与凭据)。

控制台包括工作台、任务、项目、资源、技能、MCP、档案、待处理、历史与审计，以及控制台配置。任务入口打开看板。偏好设置支持简体中文、English 或跟随浏览器；切换语言保留草稿、文件标签和阅读状态，不改写用户输入、Agent 回复或历史正文。

工作台支持 `/`、`@` 补全、排队、工具权限请求与 Agent 提问。问题的答复直接送回等待中的本轮，不作为新任务排队；断网后重试使用同一答复标识。材料可来自会话、快照文本或上传文件，支持文本选段、源码/Diff 行范围、图片矩形选区和按项目保存的标记；发送时引用的材料被固定，不会随后读取变化中的文件。见 [材料、标记与问答](docs/operations.md#材料标记与问答)。

用量页提供 **1d / 7d / 30d** 趋势：1d 按 Hub 当天小时统计，7d / 30d 为含今天的最近日历天。未上报 token 的图表点按 **0** 绘制并保持连线，上报覆盖率仍单独展示。输入、输出、缓存读取/写入、TPM、任务耗时，以及 Agent / 模型 / harness / 触发方式 / 项目明细使用同一范围，任务明细可继续展开。

会话详情的「代码」入口统一浏览文件和变更，提供文件树、源码高亮与行号、文件标签、源码 / Diff 切换。内容来自所选执行的开始或结束快照，工作区只读。控制台配置页按字段编辑预算、超时和资源限额，区分已保存值与当前运行值；这些设置需手动重启 Hub 生效。版本区显示 Hub、节点、协议与项目归属，不自动安装或迁移。

## 核心概念

| 概念 | 含义 |
|---|---|
| hub | 负责协调、账本和控制台的 Steve 进程；hub 所在机器也作为一台 node 申报能力、执行工作。 |
| node | 运行 `steve-node` 的执行机器，启动本机 harness 进程，并向 hub 申报工具、能力和健康状态。 |
| agent | 命名的执行配置：机器、harness（ACP 工具）、偏好模型，以及能力要求、会话选项、技能和 MCP。实际模型以工具的会话报告为准。 |
| project | 一件工作的目录与规矩：唯一主目录 `home`、可选的其他机器工作区、数据等级和工作方式。目录属于项目，不属于 agent。 |
| channel | 会话的消息通道，如 `console`、`feishu`。Agent 通过 `channel_send / channel_update / channel_recall` 发送、更新、撤回本回合的进度消息；适配器负责呈现与投递。 |
| 会话与交换 | 会话是绑定项目的对话容器；交换（exchange）是控制台里一次输入及其排队、执行、过程和回复。一个会话可以包含多个交换。 |
| 任务与子任务 | 任务承载目标、预算和执行历史，可以跨多个交换；委派产生挂在父任务下的子任务，共用父任务的剩余预算，并受深度和环检测限制。 |
| attempt | 一次具体执行的账本记录，包含执行者、机器、工作区、租约和结果；恢复会产生新的执行记录。 |
| 产物与落地 | 工作目录快照形成可追踪的产物；隔离执行的结果经合并与落地写回主目录，排队和冲突有独立状态。影子快照会收录未提交文件，受快照的忽略和大小规则约束。 |
| 记忆 | 全局记忆记录用户偏好，项目记忆记录当前项目的约定；在 owner 私聊和 owner 控制台的新会话首轮注入。`steve_remember` / `steve_recall` / `steve_forget` 提供读写，写入有锁和审计，群聊、访客及委派子任务不能写；remember 的同作用域幂等键保留 24 小时。 |

`gateway.task_max_turns` 和 `gateway.task_max_elapsed` 默认均为 **0（不限）**。`gateway.prompt_timeout` 默认 **10 分钟**，是没有文本、工具调用或报告的静默超时，不是整轮执行上限。

## 多机执行

在控制台“资源”页添加机器，填写 hub 能拨到的 `ip:port`、机器名和数据等级，再到目标机器执行生成的引导命令；随后添加绑定该机器的 agent。登记会写回配置并立即生效，无需重启 hub。

目标机器需要 Git、已认证的 harness 和适配目标 OS/CPU 架构的 `steve-node`。hub 可通过 `gateway.node_binary` 提供用 `CGO_ENABLED=0` 构建的二进制，也可手工 `scp`；完整步骤见 [node 部署](docs/operations.md#部署-node)。引导命令中的 hub 地址必须从 node 可达。引导只复制 hub harness 的 `command` / `args`，不复制认证、环境或其他 harness 配置，也不更新已存在的可执行二进制。node 和 `nodectl` 等自备启动脚本都应从登录 shell 启动。

协商了 **`process_journal.v1`** 的连接中断后，node 保留进程，hub 根据进程流的输入确认和输出序号续接；默认宽限 **10 分钟**。旧节点、超过宽限、日志不可回放或 node 进程已丢失时仍会失败。Hub 重启先隔离缺少停止证据的执行，再回收已确认静止的 attempt 并恢复符合条件的任务；这不等同于续接原进程流。

## 委派双工

agent 用 `steve_delegate` 交出一件有界工作；调用短暂等待后返回子任务 ID 和状态，子任务独立执行。完成结果会持久化，父回合结束或父任务空闲时，Steve 将结果作为新消息送回父会话并续跑；父任务已暂停或结束时保留结果供查看。

父 agent 可以结束当前回合等待平台投递，**不再需要轮询**。`steve_await` 仍可用于主动取结果；投递会说明改动已落地、仍在排队或发生冲突，子任务完成和文件落地是两个状态。

## 验证与门禁

| 命令 | 范围 |
|---|---|
| `make test` | 本地 Go 测试、竞态检查与依赖门禁。 |
| `make test-console` | 前端依赖门禁、构建和隔离浏览器交互测试。 |
| `make e2e-fleet` | 对已运行机群验证指定远端委派、attempt、改动、用量和落地；客户端默认及上限 **10 分钟**。 |
| `make e2e-autonomous` | 验证自主查机群、拆解并行委派、按能力执行和主动回传；默认及上限 **20 分钟**，需要项目主目录中的 `kvtool/main.go` 和申报 `build` 的远端节点。 |

[CI](.github/workflows/test.yml) 运行 gofmt、vet、race，以及前端构建、依赖门禁和隔离浏览器测试；不运行真实机群门禁。真实机群门禁使用已有 hub 和 node，不构建、部署或重启它们；会产生真实任务与文件。连接参数、前置工具、超时处理和 PR 证据要求见 [CONTRIBUTING.md](CONTRIBUTING.md) 与 [运维文档](docs/operations.md#门禁与-ci)。

## 继续阅读

- [docs/architecture.md](docs/architecture.md)：模块依赖、状态权威、提交与读取边界。
- [docs/operations.md](docs/operations.md)：逐键配置参考、部署、门禁与排障。
- [docs/history/](docs/history/)：旧控制台方案、能力清单方案与协作审计，作为历史记录保存。
- 代码入口：[cmd/](cmd/)、核心实现 [internal/](internal/)、控制台 [web/console/](web/console/)、验收 [e2e/](e2e/)。

## License

Apache-2.0
