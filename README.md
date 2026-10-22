# Steve

[English](README.en.md) · [使用文档](docs/README.md) · [下载](https://github.com/gopact-ai/steve/releases)

> *You, me, and your friend Steve.*

**和你的 AI agent 一起工作，把协作交给 Steve。**

You 是你，Me 是与你对话的 agent，Steve 是你们的协作伙伴。

Steve 是一个自托管的 AI agent 工作台。你可以在自己的机器上使用 Codex、Claude Code、Kimi Code 或 Grok，让不同 agent 共同处理一个项目，在同一个地方查看对话、任务、文件变更和需要你决定的事。

它复用你已有的 agent 工具和登录，不要求换一个模型服务。一台机器就能开始；接入更多机器后，工作可以交给具备相应工具、算力或访问权限的 agent。

## 一起把事情做完

给 agent 一个目标，和它讨论、补充材料、查看结果。需要其他 agent 帮忙时，它可以委派独立任务；Steve 保存子任务结果并送回原对话。你可以离开窗口，之后回来继续查看工作。

每轮文件变更都有快照和 Diff。远端或隔离任务的改动可以合并回项目主目录；完成、等待落地和冲突分别显示。需要授权或补充信息时，问题就在原会话中等你回答。

任务看板呈现进展和阻塞，资源页显示机器与 agent 的实际状态，用量页汇总工具上报的 token 和执行耗时。任务记录、产物和审批留在你管理的 Steve 实例中；模型请求仍由所选 agent 的模型服务处理。

## 从你习惯的地方开始

macOS App 和浏览器提供完整工作台，飞书 / Lark 可作为消息入口。它们连接同一套项目和任务记录。

```sh
brew install gopact-ai/tap/steve
steve init
```

先安装并登录至少一种支持的 agent 工具。`steve init` 创建本地安装、登记检测到的工具并打印控制台地址，初始权限为只读。在工作台选择项目和 agent，就可以开始对话。

macOS App、macOS 和 Linux 二进制包见 [Releases](https://github.com/gopact-ai/steve/releases)。首次使用见[入门指南](docs/getting-started.md)；服务器部署见[运维指南](docs/operations.md)。

## 让项目拥有自己的能力

每个项目有自己的目录、技能和访问范围。Agent 可以使用 MCP 服务检索资料、操作系统或访问开发工具；能力包把技能、MCP 配置和 agent 预设放在一起，方便复用。已打开的会话使用固定版本，升级影响新会话。

仓库提供用于开发和评审本项目的 `steve-dev` 能力包。你也可以编写自己的包，为不同项目配置不同职责的 agent。[了解插件](docs/plugins.md)。

## 让事情来找你

邮件和 GitHub Provider 把新邮件、PR 变化、评审及检查结果送入 Steve。你可以把事件留在待处理列表，也可以订阅到指定项目和会话，让 agent 按你的指令处理。

邮件支持 Gmail、Outlook、Hotmail 和 iCloud Mail。Provider 需要单独运行并完成账号授权；订阅决定哪些事件触发工作。[连接外部事件](docs/events.md)。

## 在自己的机器之间协作

Steve 区分负责协调的节点和执行任务的节点。Agent 使用目标机器上的工具与登录，项目的数据等级限制可用机器。网络中断后，Steve 根据原执行的回执尝试续接；无法确认的执行会保留恢复状态，等待核实。

完整节点可以保存协作账本副本并交接协调职责。自动容灾需要至少三个独立物理故障域中的完整节点，以及明确的接任授权。[桌面与多机协作](docs/desktop.md)。

## 文档与贡献

[使用指南](docs/guide.md)介绍项目、会话、任务和权限；[文档目录](docs/README.md)包含配置、扩展协议和架构参考。欢迎通过 [Issues](https://github.com/gopact-ai/steve/issues) 报告问题，通过 Pull Request 贡献改进。开发环境见 [Contributing](CONTRIBUTING.md)。

[Apache-2.0](LICENSE)
