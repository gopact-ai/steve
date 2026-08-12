# acpgw

自托管的「飞书 ⇄ ACP Agent」网关:把任意兼容 [Agent Client Protocol](https://agentclientprotocol.com)(ACP v1)的编码/通用 Agent(codex、Claude Code、Gemini CLI…)接入飞书机器人,实现 claw-like 的常驻 IM 智能体,且不锁定任何一家 Agent。

```
飞书用户 ⇄ 飞书开放平台(WS 长连接) ⇄ acpgw(Go) ⇄ ACP over stdio ⇄ agent 子进程
```

- **Agent 可插拔** — 通过 [gopact-ai/acp](https://github.com/gopact-ai/acp) 实现 ACP v1 client;任何 ACP agent 都能作为后端。
- **飞书长连接** — 使用 oapi-sdk-go WebSocket 事件模式,无需公网回调地址。
- **会话映射** — 一个飞书 chat 对应一个 ACP session,消息按 chat 串行处理。
- **无头审批策略** — agent 的工具调用权限请求按配置自动决策(`auto` / `always_allow` / `deny`)。

## 快速开始

### 1. 准备 Agent 后端(以 codex 为例)

```bash
codex login                       # 或导出 OPENAI_API_KEY / CODEX_API_KEY
npx -y @agentclientprotocol/codex-acp --version   # 适配器可用即可
```

其他后端:

| Agent | command | args |
|---|---|---|
| Codex | `npx` | `["-y", "@agentclientprotocol/codex-acp"]` |
| Claude Code | `npx` | `["-y", "@zed-industries/claude-code-acp"]` |
| Gemini CLI | `gemini` | `["--experimental-acp"]` |

### 2. 创建飞书应用

1. 在 [飞书开放平台](https://open.feishu.cn) 创建自建应用,启用**机器人**能力。
2. 权限:添加 `im:message`(发送)与接收消息相关权限(单聊 `im:message.p2p_msg:readonly`,群聊 `im:message.group_at_msg:readonly`)。
3. **事件订阅**切换为「使用长连接接收事件」,订阅 `im.message.receive_v1`。
4. 发布应用版本并通过审核。

### 3. 配置与运行

```bash
cp config.example.yaml config.yaml   # 填入 app_id / app_secret
go run ./cmd/acptest "你好"           # 可选:先绕过飞书验证 agent 链路
go build -o acpgw ./cmd/acpgw && ./acpgw -config config.yaml
```

单聊直接发消息,群聊 @机器人。命令:`/new`(重置会话)、`/status`。

## 测试

```bash
go test -race ./...
```

e2e 测试通过 `cmd/mockagent`(一个最小 ACP agent)覆盖:prompt 往返、流式聚合、权限策略(allow/deny)、进程崩溃后自动重启。

## 目录结构

```
cmd/acpgw        # 主程序
cmd/acptest      # 终端直连 agent 的验证工具
cmd/mockagent    # 测试用最小 ACP agent
internal/acphost # agent 子进程 + ACP client host(会话/审批/重启)
internal/channel/feishu  # 飞书长连接接入与回复
internal/gateway # chat → session 路由与串行队列
internal/config  # YAML 配置
```

## 已知边界(MVP)

- 仅处理文本消息;图片/文件待扩展(ACP 支持 image content block)。
- 回复为纯文本;富文本卡片渲染待扩展。
- 会话不持久化:进程或 agent 重启后自动开新 ACP session(codex-acp 支持 `loadSession`,可后续接入)。
- 单 agent 后端实例;多后端路由(按 chat 选 agent)待扩展。
