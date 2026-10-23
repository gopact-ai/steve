# Steve 文档

Steve 把你使用的 AI agent、项目和机器放在一个工作台中。首次使用从安装开始；已有实例可直接查看日常使用与部署参考。

## 使用 Steve

| 文档 | 内容 |
| --- | --- |
| [入门](getting-started.md) | 安装、登记 agent、打开第一个项目 |
| [日常使用](guide.md) | 会话、任务、文件审阅、权限和用量 |
| [终端客户端](tui.md) | 在终端里用 `steve` 对话、回答提问和切换会话 |
| [桌面与多机协作](desktop.md) | macOS App、SSH 接入、协调交接与恢复 |
| [连接外部事件](events.md) | 邮件、GitHub、订阅和待处理 |
| [能力包](plugins.md) | 技能、MCP、agent 预设和会话版本 |

## 部署和扩展

| 文档 | 内容 |
| --- | --- |
| [运维](operations.md) | 独立服务、集群、执行节点、备份与排障 |
| [配置参考](configuration.md) | JSON 字段、默认值和权限策略 |
| [项目迁移](project-migration.md) | 独立实例之间的离线迁移与恢复 |
| [插件作者指南](plugins-local.md) | 包清单、本地命令、凭据与管理 API |
| [事件与客户端协议](event-providers.md) | Provider、Work 和客户端能力的 HTTP 协议（English） |
| [客户端协议](protocol.md) | `steve.client.v1`：终端、桌面与网页客户端共用的会话协议（English） |
| [邮件 Provider](../providers/mail/README.md) | 邮箱授权、账号管理与同步 |
| [GitHub Provider](../providers/github/README.md) | 仓库授权、事件类型与订阅 |

## 开发 Steve

[贡献指南](../CONTRIBUTING.md)介绍构建与验证；[架构](architecture.md)说明数据与执行关系；[服务边界](service-boundaries.md)说明模块契约和扩展接口。

[返回首页](../README.md) · [English overview](../README.en.md)
