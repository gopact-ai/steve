# Steve

[English](README.en.md)

Steve 是面向个人的多机 agent 工作平台：通过 Web 控制台（飞书是可选通道）管理项目、会话与任务，按机器能力执行和委派工作，记录过程、产物、审批与恢复状态。

Steve 提供三条结构性承诺：

- **平台负责交付**：回复由平台渲染和记录；委派结果持久化后主动送回父会话，附上实际落地状态。
- **执行有明确边界**：机器按实际申报准入，项目权限与数据等级约束执行位置；内置 MCP 使用会话绑定的 token，工具权限由策略处理。
- **工作有账可查**：任务、attempt、产物与落地状态进入账本；中断后按恢复规则续跑或明确记录失败，保留审批与对外动作的对账入口。

## 从控制台开始

**当前启动需要配置飞书，纯控制台启动在 [架构文档](docs/architecture.md)的路线图 §9 B3。** 使用控制台还必须填写 `feishu.owner_open_id`，控制台以该 owner 身份执行。飞书可以不作为日常交互入口，但当前程序仍会建立飞书连接。

准备 Go 1.27+、Git、Node.js 与 npm，以及一个已完成认证的 coding agent。内置清单里的适配器（codex-acp、claude-agent-acp）由 steve 按钉死的版本自己取，不用预装；它们是 npm 包，所以 Node.js 运行环境仍是前置条件。自己编译的适配器写 `command` 直接用，见 [运维文档](docs/operations.md#部署-hub)。

1. 在仓库根目录构建并生成配置：

   ```bash
   make build
   ./steve setup
   ```

   setup 可录入已有飞书应用，也可走官方设备流创建（直接创建可用 `./steve setup -create-app`）；确认该应用下自己的 `open_id` 为 owner。

2. 编辑生成的 `config.json`。保留 setup 写入的 `feishu` 和 `projects`，将 `agents` / `harnesses` 精简到实际要用的工具。下面是仅使用 codex 的两个顶层字段：

   ```json
   {
     "agents": {
       "codex": { "harness": "codex", "default": true }
     },
     "harnesses": {
       "codex": { "adapter": "codex-acp", "permission": "read" }
     }
   }
   ```

   `adapter` 是内置清单里的名字，steve 按钉死的版本自己取、校验后再运行，不用你预装，也不会哪天悄悄换个版本。想用自己编译的适配器就写 `command`（绝对路径，不经过 shell 展开，不能写 `~/...`），两者只能给一个。完整的 [config.example.json](config.example.json) 是多机配置参考，使用前需替换占位值并删掉不用的项目、agent 和服务。

3. 体检后启动 hub：

   ```bash
   ./steve doctor
   ./steve run
   ```

   doctor 会验证飞书凭据、home，并探测配置的机器与 agent 会话；它会启动工具进程。首次绑定 owner 后，run 还会通过飞书私聊进行 home 初始化。

4. 保持 run 所在终端运行，在另一终端执行：

   ```bash
   ./steve dash
   ```

   打开打印的地址（默认 `http://127.0.0.1:7710`）。在工作台先发 `/project use workspace`（setup 默认项目名），再发 `@codex 列出这个项目的文件并说明用途`。`read` 策略适合这一步；需要写文件时先按 [权限说明](docs/operations.md#harnessesname)选择策略。

控制台提供工作台、任务、项目、资源、技能、MCP、档案、待处理、历史与审计九个入口；任务入口打开工作台的看板视图。输入框支持 `/` 和 `@` 补全；进行中的消息可以排队，过程、工具调用、回复与改动留在对应交换下。默认绑定 loopback；远程访问和 token 配置见 [运维文档](docs/operations.md#控制台与凭据)。

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

协商了 **`process_journal.v1`** 的连接中断后，node 保留进程，hub 根据进程流的输入确认和输出序号续接；默认宽限 **10 分钟**。旧节点、超过宽限、日志不可回放或 node 进程已丢失时仍会失败。hub 进程重启走任务恢复：先过期旧 attempt，再恢复符合条件的任务，不能等同于原进程流续接。

## 委派双工

agent 用 `steve_delegate` 交出一件有界工作；调用短暂等待后返回子任务 ID 和状态，子任务独立执行。完成结果会持久化，父回合结束或父任务空闲时，Steve 将结果作为新消息送回父会话并续跑；父任务已暂停或结束时保留结果供查看。

父 agent 可以结束当前回合等待平台投递，**不再需要轮询**。`steve_await` 仍可用于主动取结果；投递会说明改动已落地、仍在排队或发生冲突，子任务完成和文件落地是两个状态。

## 验证与门禁

| 命令 | 范围 |
|---|---|
| `go test -race ./...` | 本地 Go 测试与竞态检查。 |
| `make e2e-fleet` | 对已运行机群验证指定远端委派、attempt、改动、用量和落地；客户端默认及上限 **10 分钟**。 |
| `make e2e-autonomous` | 验证自主查机群、拆解并行委派、按能力执行和主动回传；默认及上限 **20 分钟**，需要项目主目录中的 `kvtool/main.go` 和申报 `build` 的远端节点。 |

[CI](.github/workflows/test.yml) 只跑 gofmt、vet、race，不运行真实机群门禁。真实机群门禁使用已有 hub 和 node，不构建、部署或重启它们；会产生真实任务与文件。连接参数、前置工具、超时处理和 PR 证据要求见 [CONTRIBUTING.md](CONTRIBUTING.md) 与 [运维文档](docs/operations.md#门禁与-ci)。

## 继续阅读

- [docs/architecture.md](docs/architecture.md)：定位、对象、权威边界、实现状态与路线图。
- [docs/operations.md](docs/operations.md)：逐键配置参考、部署、门禁与排障。
- [docs/history/](docs/history/)：旧控制台方案、能力清单方案与协作审计，作为历史记录保存。
- 代码入口：[cmd/](cmd/)、核心实现 [internal/](internal/)、控制台 [web/console/](web/console/)、验收 [e2e/](e2e/)。

## License

Apache-2.0
