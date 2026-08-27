# Steve

飞书（Lark）⇄ [ACP](https://agentclientprotocol.com) 网关：把 codex、Claude Code 等
coding agent 接进飞书聊天，像同事一样给它派活。

与「把消息转发给模型」的做法不同，Steve 的核心承诺是**结构性保证**：

- **送达不靠模型自觉**——最终答案由 Steve 从 ACP 事件流渲染成卡片并 @ 提问人；
  agent 中途可以主动发里程碑卡，但答案的送达是平台行为。
- **越权在结构上不可能**——agent 的发送能力经每会话独立 token 注入，token 与会话
  绑定；A 会话的 agent 无法发进 B 会话，撤回只能撤自己本轮发的消息。
- **审批由策略决定**——工具调用的批准走 permission broker（auto/read/write/deny），
  需要人的才上卡片按钮，其余不打扰。
- **崩溃不丢任务**——任务锚点持久化；网关重启后关闭孤儿 attempt、恢复会话、
  清理残留卡片，续跑并照常送达。

## 快速开始

前置：Go 1.27+、Node.js（`npx` 拉起 ACP 适配器）、一个飞书自建应用
（`steve setup -create-app` 可走官方设备流现场创建）。

```bash
go build -o steve ./cmd/steve
./steve setup     # 交互式：应用凭据、群策略、主人 open_id（本人自填绑定）
./steve doctor    # 体检：凭据、home、每个 agent 拉起一次会话
./steve run       # 启动网关（flock 单例，重复启动会被拒绝）
```

绑定主人后首次 `run` 会主动私聊建立 **home 会话**（身份初始化：SOUL/USER/MEMORY）。
不知道自己的 open_id？先 `run` 起来私聊 bot 一句话，网关日志里
`gateway: message ... sender=` 就是（open_id 按应用作用域，别处查到的无效）。

## 聊天即界面

| 动作 | 含义 |
|---|---|
| 直接说话 | 派活给当前 agent（默认排队，不打断进行中的轮次） |
| `!` 前缀 | 打断并替换当前轮次（全角 `！` 同） |
| `@codex` / `@claude` | 指定或切换 agent |
| `/t 任务` | 在群里种一个话题会话并行跑 |
| `/new` `/clear` | 重置会话（归档可恢复，卡片带恢复按钮） |
| `/status` `/tasks` `/model` `/history` `/skills` | 状态、任务与预算、模型切换、历史恢复、技能管理 |

多阶段任务里 agent 会用内置的 `feishu_send` / `feishu_update` / `feishu_recall`
维护一张演进的进度卡（带 `2/3` 阶段角标、与最终卡同族的尾标）。

## 架构

```
飞书长连接 → gateway（卡片/审批/锚点）→ coordinator（轮次/任务/预算）
           → capability assembler（身份+技能+MCP，指纹化）→ acphost（ACP 子进程）
agentmcp：内置 loopback HTTP MCP server，给 agent 的发送原语（每会话 token）
task store：任务与 attempt 持久化——崩溃续跑、预算刹车的依据
```

能力装配全部进指纹：能力变了会话即显式漂移（提示 `/new`），绝不静默换底。

## 安全模型

- `config.json` 含应用凭据：0600、已 gitignore，绝不入库
- 状态目录 `~/.steve/` 持锁（flock）：同一状态只允许一个网关进程
- 内置 MCP server 只绑 127.0.0.1，每会话独立 bearer token，随会话持久化
- 权限策略按 harness 配置；owner 与 guest 的 home（身份/记忆）严格隔离
- agent 内容中的 `<at>` 会被剥离：@ 人是平台最终卡的专属行为

## 测试

```bash
go test -race ./...        # 全部 wire 级防线（含 mockagent 真子进程往返）
```

`e2e/ux/` 是对真实飞书的场景验收（卡片顺序/状态/@/撤回等用户可见面）。
需要 `lark-cli` 授权与一个专用测试会话：复制 `e2e/ux/.env.example` 为
`e2e/ux/.env` 填入标识（已 gitignore）。脚本会发送真实消息。

## Harness 支持

| Harness | 状态 |
|---|---|
| codex（@agentclientprotocol/codex-acp） | ✅ 真机验证 |
| Claude Code（@agentclientprotocol/claude-agent-acp） | ✅ 真机验证 |
| grok / kimi | ⚠️ 已接线，未实测 |

## License

Apache-2.0
