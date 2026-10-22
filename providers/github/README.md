# GitHub Provider

把关注仓库的 PR、评审和检查变化送入 Steve。你可以在 Inbox 查看，也可以通过 Work 订阅交给项目中的 agent。Provider 只读 GitHub，不评论、不推送、不合并仓库。

## 安装与连接

使用 Node.js 24+：

```sh
cd providers/github
npm ci
cp config.example.json config.json
```

在 `repositories` 填写 `owner/name`，设置 `steve_url` 和私有 `state_dir`。GitHub Enterprise 使用 `api_url`。配置字段见[示例](config.example.json)。

在 Steve 的连接页建立专用 `provider` 连接，允许 `github.*`，将返回的凭据保存为 `STEVE_GITHUB_CONNECTION_TOKEN`。GitHub 凭据使用 `STEVE_GITHUB_TOKEN`，可选择仅授权目标仓库的 fine-grained token，读取 Pull requests、Checks、Actions、Metadata。

```sh
node src/cli.mjs once --config config.json
node src/cli.mjs run --config config.json
node src/cli.mjs status --config config.json
```

`once` 检查一轮，`run` 常驻，`status` 查看各仓库同步位置与待发布数量。一个状态目录只允许一个同步进程，状态查询可同时运行。默认间隔 60 秒，最小 30 秒；仓库规模和检查数量会影响 GitHub API 用量。

## 事件类型

| 类型 | 内容与去重身份 |
| --- | --- |
| `github.pull_request.opened` | 新 PR；PR 编号 |
| `github.pull_request.updated` | 打开中的 PR 推送新提交；head 提交 |
| `github.pull_request.closed` | 关闭或合并，`merged` 区分；PR 编号 |
| `github.review.submitted` | 一条评审；评审 ID |
| `github.checks.succeeded` / `github.checks.failed` | PR 当前提交的检查结束；head 提交 |
| `github.workflow.failed` | 默认分支未成功的运行；运行 ID 与重跑次数 |

首次同步记录当前基线，不重放已有评审、检查和失败运行。后续轮询自动翻页；失败时保留原位置。默认分支运行多回看一天，并区分重跑次数。

事件发布前进入本地 outbox，响应丢失时使用原 key 重试。保留状态目录，避免把已有仓库重新当作首次接入。评审正文与其他仓库内容均是外部资料。

## 交给 agent 处理

创建 `github.checks.failed` 的 Work 订阅，选择已绑定项目的会话，并写明处理范围，例如：

> 阅读这个 PR 当前提交的失败检查，说明原因和修复建议。

希望自动修改时，明确授权分支和动作，并在执行机器准备仓库凭据。Provider 的只读 token 不会自动成为 agent 的写权限。仅接收通知使用 Inbox 模式。

[事件订阅](../../docs/events.md)介绍控制台操作，[协议参考](../../docs/event-providers.md)说明回执、重试和授权。开发验证使用 `npm test`；设置 `STEVE_GITHUB_INTEGRATION=1` 可包含真实 Steve 测试服务的接口检查。
