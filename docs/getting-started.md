# 开始使用 Steve

Steve 运行在你自己的机器上，使用该机器已经安装并登录的 agent 工具。一台机器、一个项目和一个 agent 就能开始。

## 安装

使用 Homebrew：

```sh
brew install gopact-ai/tap/steve
steve init
```

初始化会建立私有状态目录、启动后台服务、登记检测到的 agent 工具，并打印控制台地址。初始工具权限为只读。工具本身需要先完成登录；检测到可执行文件不代表模型认证有效。

Steve 支持三种 agent 工具，都通过固定版本的 ACP 适配器运行：

| 工具 | 检测的可执行文件 | 适配器 |
|---|---|---|
| Codex | `codex` | `codex-acp` |
| Claude Code | `claude` | `claude-agent-acp` |
| pi | `pi` | `pi-acp` |

pi 使用自己的模型配置（`~/.pi/agent` 下的 `settings.json`、`models.json`、`auth.json`），请先在终端里让 `pi` 能正常对话。

macOS 用户也可以从 [Releases](https://github.com/gopact-ai/steve/releases) 下载 `Steve_<version>_macos_arm64.zip`，解压后打开 `Steve.app`。App 与 `steve init` 使用同一份本地安装。发布的 macOS App 未经 Apple 公证。

Linux 与 macOS 的命令行包也在 Releases 中。查看安装版本使用 `steve version`。桌面行为和数据位置见[桌面指南](desktop.md)。

## 第一个项目

在工作台登记本机 agent，在项目页选择或建立项目，再将会话绑定到该项目。项目主目录是 agent 读写文件的位置；工作区根目录用于组织项目与执行目录。建议使用独立的本地目录，例如 `~/steve`，避免使用会被云同步的 Documents 或 Desktop。

已有代码仓库可作为项目主目录。需要严格限制访问范围时，独立 clone 比 Git worktree 更容易管理：worktree 的 Git 元数据可能位于目录之外，agent 的沙箱未必允许访问。

先发一个只读请求确认环境，例如：

> 阅读这个项目，说明它的用途、启动方式和主要目录。

需要修改文件时，在会话的权限请求中批准，或在设置中选择适合的默认审批模式。回复后的变更卡片可打开本轮文件快照和 Diff。权限的作用范围见[日常使用](guide.md#权限与提问)。

## 从源码运行

构建需要 Go 1.27+、Git、Node.js 和 npm。先安装并认证要使用的 agent，再执行：

```sh
git clone https://github.com/gopact-ai/steve.git
cd steve
make console
make build
cp config.console.example.json config.json
chmod 600 config.json
```

在 `config.json` 设置稳定的 `gateway.owner_id`，把 `projects.workspace.home.path` 改为真实项目目录。只保留需要的 agent 和 harness；示例默认使用 Codex 和只读权限。

```sh
./steve doctor -config config.json
./steve run -config config.json
```

`doctor` 会准备运行环境并启动工具探针，应在服务启动前使用。保留 `run` 所在终端，在另一终端执行：

```sh
./steve dash -config config.json
```

用它打印的带凭据地址打开控制台。发送 `/project use workspace`，再发送第一个请求。`dash` 显示的登录地址具有 owner 权限，请作为私有入口使用。

内置的 Codex、Claude Code 与 pi ACP 适配器按固定版本下载并校验，首次使用需要访问 npm registry。自备的适配器命令见[配置参考](configuration.md)。常驻服务、远程访问和备份见[运维指南](operations.md)。

## 继续使用

[日常使用](guide.md)介绍队列、委派、材料、任务控制和审阅。[事件订阅](events.md)可将邮件或仓库变化交给项目处理；[插件](plugins.md)提供可复用的技能、MCP 和 agent 预设。
