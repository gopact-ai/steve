# Steve GitHub provider

独立运行的 GitHub 事件 Provider。它轮询所关注仓库的 PR、评审、检查和默认分支工作流，
按 Steve 的[事件协议](../../docs/event-providers.md)发布事件。订阅、Work 入口和 Inbox
沿用 Steve 已有的机制；Provider 不评论、不合并，也不改动仓库。

## 事件

| 类型 | 何时发布 | 去重键 |
| --- | --- | --- |
| `github.pull_request.opened` | 新开 PR | PR 编号 |
| `github.pull_request.updated` | 打开中的 PR 推送了新提交 | 新的 head 提交 |
| `github.pull_request.closed` | 合并或关闭；`merged` 区分两者 | PR 编号 |
| `github.review.submitted` | 提交了一条评审，正文属于外部资料 | 评审 ID |
| `github.checks.succeeded` / `github.checks.failed` | PR 当前提交的检查全部结束 | head 提交 |
| `github.workflow.failed` | 默认分支上的工作流运行未成功 | 运行 ID 与重跑次数 |

首次连接仓库只记录现状：所有打开 PR 的当前提交，以及已有评审、检查结果和失败运行都记为
已送达，不重放历史。之后每次轮询读取上次时间点以来的全部变化（自动翻页）；轮询失败时不推进，
下一次重复读取，由去重键吸收重叠。默认分支的运行按创建时间多回看一天，耗时长的运行和重跑
（按 `run_attempt` 区分）也会被发现。发布前事件先写入本地 outbox，响应丢失后重试会得到原来的事件。

## 配置

```sh
cd providers/github
npm ci
cp config.example.json config.json
```

在 `config.json` 的 `repositories` 中列出 `owner/name`。在 Steve 的「连接与事件 → 连接」
创建一个 `provider` 连接，允许事件 `github.*`，把一次性返回的凭据放入
`STEVE_GITHUB_CONNECTION_TOKEN`。

GitHub 凭据放入 `STEVE_GITHUB_TOKEN`。建议使用只授权这些仓库的 fine-grained token，
只读权限即可：Pull requests、Checks、Actions、Metadata。GitHub Enterprise 设置 `api_url`。

```sh
node src/cli.mjs once --config config.json   # 轮询一次，失败时退出码非零
node src/cli.mjs run --config config.json    # 常驻，每 poll_seconds 秒轮询一次
node src/cli.mjs status --config config.json # 各仓库的时间点和待发布数量
```

一个状态目录只能由一个 `run`/`once` 进程持有，`status` 可随时读取。默认每 60 秒轮询，
最小 30 秒；每个仓库每次的请求数约为 3 + 被更新 PR 数 + 尚无检查结果的打开 PR 数，
注意 GitHub 的速率限制。

## 订阅示例

把 CI 失败交给 Agent 处理：在「连接与事件 → 订阅」新建规则，事件 `github.checks.failed`，
模式 `work`，选择会话和项目，指令写明要做的事，例如"读取失败的检查日志，判断原因；
属于本 PR 的问题就在该分支修复并推送，否则在会话里说明"。只想看到通知时使用 `inbox` 模式。

## 测试

```sh
npm test
STEVE_GITHUB_INTEGRATION=1 npm test   # 另外构建并连接真实的 Steve 测试服务
```
