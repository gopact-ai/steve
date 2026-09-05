# Steve 运维指南

本文按当前代码说明配置、部署、门禁和排障。对象与权威边界见 [architecture.md](architecture.md)，首次使用见 [中文 README](../README.md) / [English README](../README.en.md)，旧方案保存在 [history/](history/)。

## 配置约定

hub 读取 `config.json`，可通过 `steve setup|doctor|run -config /绝对路径/config.json` 指定；node 读取 `node.json`，可用 `steve-node -config /绝对路径/node.json` 指定。两者都拒绝未知 JSON 字段。下文“默认”指省略字段时的代码行为，不是样例里显式写入的值。

- hub 字段以 [internal/config/config.go](../internal/config/config.go) 的结构体为准；默认行为还涉及 agent、harness、project 和 task 的校验、初始化。
- `Duration` 的 JSON 类型是字符串，使用 Go duration，如 `"30s"`、`"10m"`、`"1h30m"`，不能用数字代替。`[]` / `{}` 表示没有配置项。
- 路径示例统一用 `/home/me`，需替换为实际部署用户。harness 的 `command`、`args`、环境变量值不经过 shell 展开，`command` 不要写 `~/...` 或启动时在线安装的命令。预先安装适配器，填实际绝对路径。
- hub 会展开本机的 `state_path`、`home_path`、本机项目 `home.path`、技能路径和 harness `process_dir`；相对路径以进程启动目录为基准。远端项目路径保留原值，工作区副本要求绝对路径。跨机器路径一律填写目标机器上的绝对路径最清楚。
- [config.example.json](../config.example.json) 展示多机、区域、MCP 和调试配置，并非最小配置。模型和选项的 `<...>` 是占位符，必须替换为资源页实际报告的 ID/值，或删除对应字段；技能目录、地址和密钥同样需要替换。删除不用的 agent、项目、区域和服务；不用调试时删除 `debug_addr` / `debug_chat_id`。
- [node.example.json](../node.example.json) 展示带 `hubs` 名称绑定和独立 MCP broker 的节点。将 `hubs` 里的 `hub-a` 换成实际 hub 名称，或以 `STEVE_NODE=hub-a` 启动 hub。只运行 node 时可删除 `mcp_broker`，保留空 `mcp_servers`；需要 MCP 时按[下文](#mcp-部署)选内置或独立 broker。

### hub 顶层

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `agents` | object<string, Agent> | 无，至少一项 | 命名执行配置，必须恰有一个默认 agent | `{"codex":{"harness":"codex","default":true}}` |
| `projects` | object<string, Project> | 无，至少一项；旧布局可迁移 | 声明项目；`home` 是保留项目名 | `{"work":{"home":{"path":"/srv/work"}}}` |
| `harnesses` | object<string, Harness> | 无，至少一项 | hub 本机启动命令与权限策略；agent 引用的 harness 必须在此登记 | `{"codex":{"command":"/home/me/.local/bin/codex-acp"}}` |
| `nodes` | object<string, Node> | `{}` | hub 如何连接远端机器；能力来自 node 的实际申报 | `{"host-3":{"addr":"10.0.0.3:7701","token":"replace-me"}}` |
| `mcp_servers` | object<string, MCPServer> | `{}` | hub 本机 MCP 定义 | `{"docs":{"type":"http","url":"https://mcp.example.com/mcp"}}` |
| `feishu` | Feishu object | 无有效默认，凭据必填 | 飞书/Lark 连接、访问规则与 owner | `{"app_id":"cli_...","app_secret":"replace-me","owner_open_id":"ou_..."}` |
| `gateway` | Gateway object | 各字段按下表 | 状态目录、控制台、预算与协调设置 | `{"read_model_addr":"127.0.0.1:7710"}` |

`Config.Migrated` 是加载时生成的迁移提示（`json:"-"`），不是可配置键。

### `projects.<name>`

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `home` | ProjectHome object | 必填 | 唯一主目录的机器和路径 | `{"node":"host-3","path":"/srv/lab"}` |
| `home.node` | string | `""`（hub） | 主目录所属机器，非空必须在 `nodes` 中 | `"host-3"` |
| `home.path` | string | 必填 | 主目录；远端路径不在 hub 上展开 | `"/srv/lab"` |
| `level` | string | `"internal"` | 数据等级：`public`、`internal`、`restricted`、`sealed` | `"restricted"` |
| `repo` | string | `"inplace"` | `inplace` 的交互回合直接改工作目录；`isolated` 使用隔离树并走落地 | `"isolated"` |
| `skills` | string[] | `[]` | 固定到项目的技能目录，在 hub 读取 | `["/home/me/steve-skills/review"]` |
| `durable_places` | string[] | `[]`（hub）；sealed 会补入 home 所在机器 | 哪些机器的产物回执算耐久；其中任一有效回执即可，`""` 表示 hub | `[""]` |
| `external_remote` | string | `""`；克隆时尝试使用主目录根仓库的 remote | 控制台创建工作区副本时优先使用的克隆来源；加载配置本身不克隆 | `"git@github.com:example/work.git"` |
| `grants` | object<string, string> | `{}` | principal（飞书 open_id）到 `none` / `read` / `write` / `admin`；owner 总是 admin | `{"ou_...":"write"}` |
| `default_role` | string | public/internal 为 `write`，restricted/sealed 为 `none` | 未单独授权的用户角色，可显式设上述四种值 | `"none"` |
| `workspaces` | ProjectWorkspace[] | `[]` | 主目录之外的已有工作区，一台机器一个；配置不会自动克隆，sealed 不允许副本 | `[{"node":"host-3","path":"/srv/work-copy"}]` |
| `workspaces[].node` | string | `""`（hub） | 副本所在机器，不能是项目 home 所在机器 | `"host-3"` |
| `workspaces[].path` | string | 必填 | 该机器上的副本绝对路径 | `"/srv/work-copy"` |

任务创建时固定项目；切换会话项目不会把既有任务搬到新项目。计划步骤和委派使用隔离树；主目录直接修改不提供原子落地保证。

### `agents.<name>`

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `aliases` | string[] | `[]`；agent ID 本身可用 | `@` 选择的别名，不能跨 agent 重复 | `["coder"]` |
| `harness` | string | 必填 | 引用 `harnesses` 中的 AI 工具 | `"codex"` |
| `node` | string | `""`（hub） | 固定执行机器，非空必须在 `nodes` 中 | `"host-3"` |
| `model` | string | `""`（工具默认） | 开会话时尝试设置的偏好模型；实际模型仍以工具报告为准 | `"<model-id-from-fleet>"` |
| `options` | object<string, string> | `{}` | 其他会话选择器的 ID → 值；必须是该 harness 暴露的选项 | `{"<option-id-from-fleet>":"<option-value-from-fleet>"}` |
| `about` | string | `""` | 告诉规划器和其他 agent 这份配置适合什么工作 | `"编译与验证 Go 项目"` |
| `requires` | string[] | `[]` | 执行所需能力选择器 | `["tool:go","hardware:gpu"]` |
| `system_prompt` | string | `""` | 装配到会话指令中的文本 | `"报告验证结果与未解决问题。"` |
| `skills` | string[] | `[]` | 固定到 agent 的技能目录，在 hub 读取 | `["/home/me/steve-skills/review"]` |
| `mcp_servers` | string[] | `[]` | 引用执行机器本地的 MCP 名字；远端使用 node 上的同名定义 | `["filesystem","docs"]` |
| `default` | boolean | `false`；整个配置恰有一个 `true` | 未指定 agent 时的默认选择 | `true` |
| `workspace` | string | `""`，兼容项 | 仅用于迁移旧布局；没有 `projects` 时迁成同名项目，与新 `projects` 并存会报错 | `"/srv/old-work"`（仅旧文件） |

能力选择器支持 `kind:id`、模式匹配、或、否定、版本条件，例如 `tool:go`、`model:claude*`、`tool:docker@>=27`；裸标签如 `build` 对应 `tag:build`。当前 harness、tool、hardware、model、skill、mcp、tag 可参与调度；network、credential、a2a 只展示，作为要求会返回 `NOT_SCHEDULABLE`。参见 [internal/ability](../internal/ability/) 与 [internal/roster](../internal/roster/)。

### `harnesses.<name>`

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `adapter` | string | `""` | 内置清单里的适配器名（`codex-acp`、`claude-agent-acp`）。steve 按钉死的版本取到缓存、校验后运行；与 `command` 二选一，不能并存，也不接受 `args` | `"codex-acp"` |
| `command` | string | 与 `adapter` 二选一，必须给一个 | 自己预装的适配器可执行文件；hub 直接启动，不做 shell 展开 | `"/home/me/.local/bin/codex-acp"` |
| `args` | string[] | `[]` | 直接传给命令的参数；`adapter` 不接受 | `[]`；kimi 为 `["acp"]` |
| `process_dir` | string | `""`（hub 进程当前目录） | harness 进程启动目录；会话工作目录另由项目决定 | `"/home/me/steve-runtime"` |
| `env` | string[] | `[]`（继承进程环境，再加入默认的隔离 home 设置） | 追加或覆盖进程环境，格式 `KEY=value` | `["PATH=/home/me/.local/bin:/usr/local/bin:/usr/bin:/bin"]` |
| `permission` | string | `"read"` | ACP 工具权限策略，见下表 | `"write"` |
| `slots` | integer | `0`（不限） | hub 上该 harness 的并发会话槽位；远端使用 node 自己的值 | `4` |

| 策略 | 当前行为 |
|---|---|
| `read` | 放行 read/search/fetch/think 类调用；其他调用有人工回调时询问，否则拒绝。 |
| `write` / `auto` | 放行工具权限请求，优先选择 allow-once。 |
| `always_allow` | 放行，优先选择 allow-always。 |
| `deny` | 拒绝工具权限请求。 |

策略作用于 harness 经 ACP 发出的权限请求，session mode 也会尝试与策略匹配。**当前控制台没有接入工具权限的人工问答回调**，所以 `read` 下的写请求会被拒绝；在控制台执行写任务需要选择允许写入的策略。飞书回合接有卡片问答。控制台“待处理”里的 sealed 披露批准、对外动作对账是另外的机制。依据：[permission/broker.go](../internal/permission/broker.go)、[acphost/host.go](../internal/acphost/host.go)、[console/console.go](../internal/console/console.go)。

### `mcp_servers.<name>`

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `type` | string | hub 无有效默认，需显式填写 | `stdio`、`http` 或 `sse` | `"stdio"` |
| `command` | string | `""`；stdio 必填 | 本机已安装的 MCP 可执行命令 | `"/home/me/.local/bin/mcp-server-filesystem"` |
| `args` | string[] | `[]` | stdio 命令参数 | `["/srv/work"]` |
| `env` | object<string, string> | `{}` | stdio MCP 服务环境；与 harness 的 `env` 数组格式不同 | `{"LANG":"C.UTF-8"}` |
| `url` | string | `""`；http/sse 必填 | 完整 HTTP(S) 服务地址 | `"https://mcp.example.com/mcp"` |
| `headers` | object<string, string> | `{}` | http/sse 请求头 | `{"Authorization":"Bearer replace-me"}` |

hub 本机的 MCP 描述交给本机 harness；远端 MCP 的定义与秘密留在 node 或独立 broker，由 broker 绑定到执行。内置的 Steve MCP 工具经 node 的 loopback 反向通道回 hub，与这里配置的外部 MCP 是两件事。

### feishu

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `app_id` | string | 必填 | 飞书/Lark 自建应用 ID | `"cli_..."` |
| `app_secret` | string | 必填 | 应用密钥 | `"replace-me"` |
| `domain` | string | `"feishu"` | `feishu` 或 `lark` | `"lark"` |
| `owner_open_id` | string | `""` | owner 身份；控制台必须配置。省略时 hub 可启动，但私聊用 guest home，控制台不能执行 | `"ou_..."` |
| `allowed_senders` | string[] | `[]`（群消息不按发送者限制） | 非空时只接收这些发送者的群消息，私聊不使用此名单 | `["ou_..."]` |
| `blocked_senders` | string[] | `[]` | 群聊和私聊均拒绝这些发送者，优先于其他规则 | `["ou_..."]` |
| `group_policy` | string | `"open"` | 接受 `open` / `allowlist` / `disabled`；`disabled` 禁止群消息 | `"disabled"` |
| `allow_unmentioned` | boolean | `false` | 接收未 @ bot 的群消息，再由参与策略决定是否响应 | `true` |
| `dm_policy` | string | `""`，兼容项 | 只校验 `pairing` / `allowlist`，当前不参与访问决策 | `"pairing"`（仅旧文件） |

当前访问代码对非 disabled 群消息都会检查 `allowed_senders`，与 `group_policy` 写 open 还是 allowlist 无关；**空名单不会封闭群访问**。要关闭群入口用 `disabled`。这两个兼容项（`feishu.dm_policy`、`agents.<name>.workspace`）不放进新样例。依据：[channel/feishu/access.go](../internal/channel/feishu/access.go)。

### gateway

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `node_binary` | string | `""` | 引导脚本可下载的 steve-node 文件；不填则需预先复制 | `"/home/me/steve-bin/steve-node"` |
| `prompt_timeout` | Duration string | `"10m"`；非正值也取此默认 | 一轮没有文本、工具调用或报告的静默超时；不是总时长上限 | `"15m"` |
| `state_path` | string | `"~/.steve/state.json"` | 旧状态文件路径，其父目录决定账本、运行状态和锁的位置 | `"/home/me/.steve/state.json"` |
| `home_path` | string | `state_path` 父目录下的 `home` | Steve 身份、用户档案与全局记忆目录；保留项目 `home` 的主目录 | `"/home/me/.steve/home"` |
| `task_max_turns` | integer | `0`（不限） | 为新任务设置最大轮数 | `100` |
| `task_max_elapsed` | Duration string | `"0s"`（不限） | 为新任务设置执行耗时预算 | `"2h"` |
| `offline_reminder_after` | Duration string | `"15m"`；零取默认 | 长回合结束且用户期间未继续说话时，额外通知；负数关闭 | `"-1s"` |
| `debug_addr` | string | `""`（关闭） | 调试消息/卡片回调注入接口，只允许 loopback | `"127.0.0.1:7711"` |
| `debug_chat_id` | string | `""` | 调试注入的默认飞书 chat ID | `"oc_..."` |
| `capabilities` | string[] | `[]` | hub 的兼容能力标签 | `["build"]` |
| `tools` | string[] | `[]` | hub 需要观测的命令 | `["git","go","gh"]` |
| `declares` | string[] | `[]` | 运维声明的能力，不能替代工具的实际观测 | `["network:internal"]` |
| `read_model_addr` | string | `"127.0.0.1:7710"` | 控制台、状态快照和事件流监听地址 | `"0.0.0.0:7710"` |
| `read_model_token` | string | `""`（仅 loopback 可省） | 控制台/API 的 bearer token；非 loopback 必填，拥有 owner 操作权限 | `"replace-me-with-a-long-random-token"` |
| `planner` | string | `""`（规则规划器） | `/plan` 的拆解 agent；不填时开放目标按一步处理 | `"claude"` |
| `level` | string | 按 hub 需耐久保存的项目推导，至少 `restricted` | hub 的数据等级；默认排除 home 在远端的 sealed 项目 | `"restricted"` |
| `region` | string | `"default"`（账本的本地区域） | 本 hub 的租约签发区域 | `"east"` |
| `regions` | object<string, Region> | `{}` | 其他区域的租约签发方 | `{"west":{"url":"http://10.0.0.9:7720","token":"replace-me"}}` |
| `issuer_addr` | string | `""`（关闭） | 对其他区域提供本地租约签发 HTTP 服务 | `"0.0.0.0:7720"` |
| `issuer_token` | string | `""` | 本地租约签发服务认证 token | `"replace-me-with-an-issuer-token"` |
| `direct_transfer` | boolean | `false` | 允许持有产物的 node 在 hub 授权下直传给另一 node；关闭时经过 hub | `true` |
| `default_project` | string | 只有一个项目时自动选它，否则 `""` | 新会话未选择项目时的绑定；owner 飞书私聊默认使用保留项目 home | `"work"` |

任务预算的 **0 是不限**；设置正值才施加限制，子任务从父任务剩余预算分配。`prompt_timeout`、任务预算、node 续接宽限和 e2e 客户端截止时间是不同的时钟。

### `nodes.<name>` 与 `gateway.regions.<name>`

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `nodes.<name>.addr` | string | 必填 | hub 拨号目标 | `"10.0.0.3:7701"` |
| `nodes.<name>.token` | string | 必填 | node 验证 hub 的 token；与目标 node 配置对应 | `"replace-me"` |
| `nodes.<name>.dial_timeout` | Duration string | `"10s"` | TCP 拨号超时；连接上下文也可能施加更短期限 | `"5s"` |
| `nodes.<name>.level` | string | `"internal"` | 该机器允许承载的最高项目等级，由 hub 赋予 | `"restricted"` |
| `nodes.<name>.region` | string | hub 自己的区域 | 机器资源的租约签发区域；外区域须列在 `gateway.regions` | `"west"` |
| `nodes.<name>.peer_addr` | string | `addr` | node 直传时其他机器访问它的地址 | `"10.0.0.3:7701"` |
| `gateway.regions.<name>.url` | string | `""`，使用时需有效地址 | 外区域租约签发 HTTP 地址 | `"http://10.0.0.9:7720"` |
| `gateway.regions.<name>.token` | string | `""` | 外区域服务的 bearer token | `"replace-me"` |

### node.json

依据：[cmd/steve-node/main.go](../cmd/steve-node/main.go)、[internal/node/serve.go](../internal/node/serve.go)。hub 的 `nodes` 只是连接配置，这张表才是执行机器自己的配置。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `name` | string | 必填 | node 申报的名字，应与 hub 中登记的机器名一致 | `"host-3"` |
| `listen` | string | `"0.0.0.0:7701"` | 接收 hub 连接；`-listen` 参数可覆盖 | `"10.0.0.3:7701"` |
| `token` | string | 必填，即使配置了 `hubs` | 通用 hub 认证 token；若也列在 `hubs`，名称同样受约束 | `"replace-me-with-a-long-random-secret"` |
| `hubs` | object<string, string> | `{}` | hub 名称 → token；表中的 token 必须匹配该名字，不会自动禁用未列入表的顶层 token | `{"hub-a":"replace-me-with-a-long-random-secret"}` |
| `harnesses` | object<string, HarnessSpec> | 必填，至少一项 | 该机器可启动的 ACP 工具 | `{"codex":{"command":"/home/me/.local/bin/codex-acp"}}` |
| `harnesses.<name>.adapter` | string | `""` | 内置清单里的适配器名；node 自己取到 `state_dir/adapters` 并校验后运行。与 `command` 二选一 | `"codex-acp"` |
| `harnesses.<name>.command` | string | 与 `adapter` 二选一 | 本机自己预装的可执行文件 | `"/home/me/.local/bin/codex-acp"` |
| `harnesses.<name>.args` | string[] | `[]` | 命令参数；`adapter` 不接受 | `[]` |
| `harnesses.<name>.env` | string[] | `[]`（继承 node 环境，再加入默认的隔离 home 设置） | harness 环境覆盖 | `["PATH=/home/me/.local/bin:/usr/local/bin:/usr/bin:/bin"]` |
| `harnesses.<name>.process_dir` | string | `workspace_root`；后者为空时用进程当前目录 | harness 启动目录，与会话工作目录不同 | `"/home/me/steve-runtime"` |
| `harnesses.<name>.models` | string[] | `[]` | 该工具在本机提供的模型声明；真实可用模型仍以会话报告为准 | `["<model-id-from-this-harness>"]` |
| `harnesses.<name>.slots` | integer | `0`（不限） | node 上该 harness 的并发会话槽位 | `4` |
| `capabilities` | string[] | `[]` | 兼容能力标签，`build` 等价于 `tag:build` | `["build"]` |
| `tools` | string[] | `[]` | 要在机器上观测的命令 | `["git","go","docker"]` |
| `mcp_servers` | object<string, MCPSpec> | `{}` | 内置 broker 管理的 MCP 定义；不能与有效 `mcp_broker` 并用 | `{"docs":{"type":"http","url":"https://mcp.example.com/mcp"}}` |
| `mcp_servers.<name>.type` | string | `"stdio"` | MCP 传输：stdio/http/sse；此默认与 hub 不同 | `"http"` |
| `mcp_servers.<name>.command` | string | `""`；stdio 需要有效命令 | node 本地 MCP 命令，由 broker 启动 | `"/home/me/.local/bin/mcp-server-filesystem"` |
| `mcp_servers.<name>.args` | string[] | `[]` | stdio 命令参数 | `["/srv/work"]` |
| `mcp_servers.<name>.env` | object<string, string> | `{}` | stdio 服务环境，留在 node/broker | `{"LANG":"C.UTF-8"}` |
| `mcp_servers.<name>.url` | string | `""`；http/sse 需有效地址 | broker 代理的 MCP 上游地址 | `"https://mcp.example.com/mcp"` |
| `mcp_servers.<name>.headers` | object<string, string> | `{}` | broker 在 HTTP 转发中注入的头 | `{"Authorization":"Bearer replace-me"}` |
| `declares` | string[] | `[]` | 运维声明的能力 | `["network:internal"]` |
| `mcp_broker` | BrokerRef object/null | `null` | 选择独立进程中的 broker；不配置时使用内置 broker | `{"socket":"/home/me/.steve-mcp/mcp.sock","token":"replace-me"}` |
| `mcp_broker.socket` | string | `""`（不启用独立 broker） | 本机 Unix socket；填写绝对路径 | `"/home/me/.steve-mcp/mcp.sock"` |
| `mcp_broker.token` | string | `""` | broker 控制 token，应与 mcp.json 相同 | `"replace-me-with-a-separate-broker-token"` |
| `workspace_root` | string | `""` | 物化工作树的根目录，以及未指定目录的命令/进程的后备目录 | `"/home/me/steve-work"` |
| `state_dir` | string | `"~/.steve-node"` | 隔离 home、进程日志、技能包、取来的适配器（`adapters/`）、节点归属与 broker 状态 | `"/home/me/.steve-node"` |

`Source`、`SessionGrace`、`FaultDropAfter` 的 JSON 标签是 `-`，不能写进 node.json。`Source` 来自 `-config`；后两项的命令行环境入口如下：

| 环境变量 | 默认 | 作用与示例 |
|---|---|---|
| `STEVE_NODE`（hub） | 主机名；无法读取时 `local` | 固定 hub 机器名，例如 `STEVE_NODE=hub-a`；使用 `hubs` 时必须匹配。 |
| `STEVE_NODE_SESSION_GRACE`（node） | `10m` | 进程流续接宽限，必须为正 duration，例如 `15m`；通过 advert 告知 hub。 |
| `STEVE_NODE_FAULT`（node） | 关闭 | 测试故障注入，例如 `drop-hub-after:20s`；会切断 hub 连接，不用于正常部署。 |

### 独立 broker 的 mcp.json

仅 `steve-node mcp-broker -config mcp.json` 使用，依据 [internal/node/mcpbroker.go](../internal/node/mcpbroker.go)。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `socket` | string | 必填 | 供 node 和 MCP launcher 访问的 Unix socket | `"/home/me/.steve-mcp/mcp.sock"` |
| `token` | string | 必填 | 控制接口认证，与 node 的 `mcp_broker.token` 对应 | `"replace-me-with-a-separate-broker-token"` |
| `mcp_servers` | object<string, MCPSpec> | `{}` | 同 node MCP 字段表中的完整服务定义；密钥放这里 | `{"docs":{"type":"http","url":"https://mcp.example.com/mcp"}}` |
| `workspace_root` | string | `""`（broker 当前目录） | stdio 服务的启动目录 | `"/home/me/steve-work"` |
| `port_file` | string | `""`（不持久化代理端口） | 保存 HTTP loopback 代理端口 | `"/home/me/.steve-mcp/proxy.port"` |
| `launcher` | string | broker 自己的可执行文件 | agent 调用 `mcp-launch` 的 steve-node 路径 | `"/home/me/steve-bin/steve-node"` |
| `socket_mode` | integer | `0` → `0600` | socket 权限位；JSON 用十进制，跨用户共享组的 `0660` 为 432 | `432` |

## 部署 hub

以下命令用于新部署。示例用户是 `me`，目录按实际环境替换。

内置清单里的适配器（`codex-acp`、`claude-agent-acp`）不用预装：harness 写 `"adapter": "codex-acp"`，hub 首次启动时取到 `state_path` 父目录下的 `adapters/`，按编译进二进制的摘要校验后运行。首次取用需要能访问 npm registry 和本机的 `npm`；之后命中缓存就不再联网。摘要对不上、清单里没有这个名字、或者装不上，都是启动失败并说明原因——不会退回去随便跑一个版本。

自己编译或另有来源的适配器改写 `command`（绝对路径）并把 `args` 填 `[]`，两者只能给一个。这种情况下自行安装：

```bash
npm install -g --prefix /home/me/.local @agentclientprotocol/codex-acp
```

只使用一个工具时只保留对应 agent/harness。工具本身的认证与代理是部署用户 profile 里的东西，先在登录 shell 里确认，再按 [README](../README.md#从控制台开始) 执行 `make build` → `steve setup` → `steve doctor` → `steve run` → `steve dash`。

`make build` 使用 `CGO_ENABLED=0` 构建 `steve` 和 `steve-node`；控制台静态文件已嵌入 Go 源码目录，普通后端构建不需要重新构建前端。修改前端时先执行 `make console`，再构建二进制。

部署到固定目录后，可在登录 shell 前台运行：

```bash
bash -lc 'exec /home/me/steve-bin/steve run -config /home/me/steve-bin/config.json'
```

hub 对 `state_path` 的父目录持单例锁，同一状态目录不能同时启动两个 hub。配置由 setup 保存时权限为 `0600`；手工复制的含密钥文件也设为 `0600`。不要用 `doctor` 作为线上只读健康检查：它会准备运行目录、打开账本、声明项目并启动 agent 探针；已有 hub 的日常检查用控制台、`steve top` 或 `/state`。

### 控制台与凭据

当前启动会校验飞书 `app_id` / `app_secret` 并启动飞书连接；不存在跳过飞书的运行模式。`owner_open_id` 为空不会阻止 hub 启动，但控制台无法执行动作。owner 是当前应用下的 open_id，由 setup 确认；首次 owner 初始化还会用到飞书私聊。纯控制台启动见 [architecture.md](architecture.md) 路线图 §9 B3。

默认地址 `127.0.0.1:7710` 只在 hub 本机可访问。对外监听需要设置 `gateway.read_model_addr` 和非空 `gateway.read_model_token`。`dash` / `top` / `say` 不读取 config.json 的地址或 token，使用自定义监听时要显式传参：

```bash
./steve dash -url http://10.0.0.1:7710 -token "$STEVE_CONSOLE_TOKEN"
./steve top -url http://10.0.0.1:7710 -token "$STEVE_CONSOLE_TOKEN" -once
./steve say -url http://10.0.0.1:7710 -token "$STEVE_CONSOLE_TOKEN" /fleet
```

这里的环境变量由部署者预先设置。read-model token 代表 owner 管理权限；`dash` 输出的 URL 会包含 token，不要贴进 PR。它与飞书 app secret、node 认证 token、MCP broker token 和区域 issuer token 分别配置。

资源页可以即时添加机器和 agent，项目页可以添加项目/工作区，并写回相应配置；直接在磁盘上编辑 JSON 不是通用热加载接口。当前没有 `steve config apply` 命令。

## 部署 node

### 构建、复制、启动

在源码目录为目标机器的 OS/CPU 架构构建。例如目标是 Linux amd64；arm64 机器将 `GOARCH` 改成 `arm64`，不能把本机架构的二进制直接用于异构机器：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o steve-node ./cmd/steve-node
ssh me@host-3 'mkdir -p /home/me/steve-bin /home/me/steve-work'
scp ./steve-node me@host-3:/home/me/steve-bin/steve-node
```

在本地准备 node.json，最小配置如下；token 换成与 hub 登记值相同的随机密钥，工具需要预装在目标机器：

```json
{
  "name": "host-3",
  "listen": "0.0.0.0:7701",
  "token": "replace-me-with-a-long-random-secret",
  "workspace_root": "/home/me/steve-work",
  "state_dir": "/home/me/.steve-node",
  "harnesses": {
    "codex": {
      "command": "/home/me/.local/bin/codex-acp",
      "args": []
    }
  }
}
```

```bash
scp ./node.json me@host-3:/home/me/steve-bin/node.json
ssh me@host-3 'chmod 600 /home/me/steve-bin/node.json'
ssh me@host-3 'bash -lc "nohup /home/me/steve-bin/steve-node -config /home/me/steve-bin/node.json > /home/me/steve-node.log 2>&1 < /dev/null &"'
```

**从登录 shell 启动**，才能让 agent 继承部署用户 profile 中的 PATH、代理及认证环境。若使用自备 `nodectl` 脚本，也从登录 shell 调用，例如 `bash -lc '/home/me/bin/nodectl start'`；`nodectl` 不是 Steve 内置命令。systemd 可让 `ExecStart` 调用 `/bin/bash -lc 'exec ...'`，并用 `EnvironmentFile` 显式提供环境。

node 要安装 Git；工作树物化和产物传输依赖它。默认 harness home 隔离在 `state_dir/runtimes/<harness>`，初始化时按工具规则引用认证、筛选配置，不会直接沿用用户的个人技能/MCP 清单。正常运行时 hub 下发启用的技能包，node 校验并物化；认证和必要的环境变量仍需在 node 本机准备。

### 在控制台添加机器

资源页添加机器需要 hub 能拨到的 node 地址，例如 `10.0.0.3:7701`。返回的引导命令要在 node 上执行，命令中的 hub URL（如 `http://10.0.0.1:7710`）也必须从 node 可达；不要把浏览器访问 hub 时的 `127.0.0.1` 当作 node 的下载地址。

如果 hub 配了 `gateway.node_binary`，引导脚本会从 hub 下载二进制；该文件必须匹配目标 OS/架构。当前脚本会写 node.json，只复制 hub harness 的 `command` / `args`，不复制 `env`、`process_dir`、slots、模型、MCP、技能目录或认证；node 上路径不同就需要调整。已有可执行的 steve-node **不会更新**，更新需重新构建和复制。引导脚本会替换其匹配到的既有 node 进程；应把它用于新增/重新部署，不当作在线升级接口。

手工部署时在 hub 登记对应 `nodes`、绑定 `node` 的 agent，以及需要的项目工作区；资源页登记可即时连接，无需重启 hub。配置中写了 harness 只代表声明，资源页或 `/fleet` 报的可用性来自机器观测；缺命令、模型不匹配或能力不足都有具体原因。

node 同时只服务一个 hub。`hubs` 可把 token 绑定到 hub 名称；hub 名称取 `STEVE_NODE`，否则取主机名。原 hub 干净断开会交还 node；异常静默时归属保留 10 分钟，之后其他 hub 才可认领，也可在明确移交时使用：

```bash
./steve-node adopt -config /home/me/steve-bin/node.json hub-b
```

`adopt` 更新归属记录，不会修改 `hubs` 的认证映射或踢掉仍在线的 hub。这一归属宽限与下面的进程流续接宽限相互独立。

### MCP 部署

简单模式是在 node.json 的 `mcp_servers` 写定义，并删除 `mcp_broker`。例如下面两项（合并进 node.json），stdio 命令已预装，HTTP 地址和认证头换成自己的服务：

```json
{
  "mcp_servers": {
    "filesystem": {
      "type": "stdio",
      "command": "/home/me/.local/bin/mcp-server-filesystem",
      "args": ["/home/me/steve-work"],
      "env": {"LANG": "C.UTF-8"}
    },
    "docs": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": {"Authorization": "Bearer replace-me-with-the-mcp-token"}
    }
  }
}
```

内置 broker 使用 `state_dir/mcp.sock`。远端 agent 的 `mcp_servers: ["filesystem", "docs"]` 会绑定到这台 node 的服务；stdio 会话只拿到 `steve-node mcp-launch -socket ... <binding>`，HTTP/SSE 只拿到 loopback binding URL，服务的 env/headers 由 broker 使用。

[node.example.json](../node.example.json) 演示另一种部署：将定义移进独立的 mcp.json，node 的 `mcp_servers` 留空，只保留 `mcp_broker` 的 socket/token。例如 mcp.json：

```json
{
  "socket": "/home/me/.steve-mcp/mcp.sock",
  "token": "replace-me-with-a-separate-broker-token",
  "workspace_root": "/home/me/steve-work",
  "port_file": "/home/me/.steve-mcp/proxy.port",
  "launcher": "/home/me/steve-bin/steve-node",
  "mcp_servers": {
    "filesystem": {
      "type": "stdio",
      "command": "/home/me/.local/bin/mcp-server-filesystem",
      "args": ["/home/me/steve-work"],
      "env": {"LANG": "C.UTF-8"}
    },
    "docs": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": {"Authorization": "Bearer replace-me-with-the-mcp-token"}
    }
  }
}
```

```bash
bash -lc 'exec /home/me/steve-bin/steve-node mcp-broker -config /home/me/steve-bin/mcp.json'
```

先运行 broker，再从另一登录 shell 启动 node。独立进程仍与 node 同用户时不构成秘密隔离；需要隔离时以另一用户运行 broker，仅该用户能读 mcp.json，给 node/agent 共享组访问 socket 及父目录的权限，`socket_mode` 使用十进制 `432`（`0660`）。launcher 必须是 agent 用户能执行的路径。

### 连接中断与恢复

[internal/node/transport.go](../internal/node/transport.go) 按协商特性选择传输方式。双方支持 `process_journal.v1` 时，node 保留 harness 进程与有界输入/输出 journal；连接恢复后按 stream ID、已读输出位置与 `ResumeAck.HaveIn` 续接，回放缺失输出并避免重复输入。默认续接宽限 10 分钟，由 node 的 `STEVE_NODE_SESSION_GRACE` 可调整；等待重连期间相关静默时钟暂停。

旧节点没有此能力，断线仍会结束流。超过宽限、node 进程已消失、日志超出保留范围（`too old`）、日志写入失败或不可续接都会失败；journal 有界，不保证无限期回放。显式结束、取消和干净关闭不按网络故障保留进程。

hub 进程重启是另一条路径：先过期旧 attempt、恢复未完成的落地，再恢复符合条件的会话任务及持久队列，并补投递已完成子任务的结果。暂停的任务不会自动续跑；中断超过 24 小时的会话任务留在停止状态，缺少消息锚点的聊天任务不能自动回复。它不承诺恢复旧 hub 内存中的 ACP 连接。

## 门禁与 CI

[Makefile](../Makefile)、[e2e/fleet](../e2e/fleet/) 和 [.github/workflows/test.yml](../.github/workflows/test.yml) 定义实际入口：

| 命令 | 内容 | 期限/前置 |
|---|---|---|
| `make build` | 静态构建 hub 和 node | Go 1.27+；不启动服务 |
| `make test` / `go test -race ./...` | Go 测试与竞态检查 | 本地运行 |
| `make e2e-fleet` | 指定远端 agent 的委派、attempt、改动索引、用量、文件落地 | 已运行机群；客户端默认/上限 10 分钟 |
| `make e2e-autonomous` | 查机群、并行拆解委派、按 build 能力执行、回传结果与落地 | 已运行机群；客户端默认/上限 20 分钟 |
| `make e2e` | `STEVE_MESH_E2E=1` 的 mesh 测试 | Go 测试总超时 25 分钟；独立测试机群配置见 [e2e/mesh/](../e2e/mesh/) |
| CI | gofmt、`go vet ./...`、`go test -race ./...` | master push 与 PR；不运行真实机群门禁 |

两个 fleet 门禁在 **hub 机器的仓库根目录**运行，使用已有 hub 和 node，不构建、部署或重启它们。它们以 owner 访问控制台 API，读取 hub 本机项目主目录，所以仅有远程 HTTP 访问不够；任务会使用真实模型并留下会话与文件证据。

```bash
bash -lc 'make e2e-fleet'
bash -lc 'make e2e-autonomous'
```

连接参数优先级为命令行 `-hub` / `-token` → 环境变量 `HUB` / `TOKEN` → 当前工作树的 `config.e2e.json` 中 `gateway.read_model_addr` / `gateway.read_model_token`。缺地址时用 `http://127.0.0.1:7710`，通配监听地址转换为 loopback；token 必填。新工作树必须自备连接配置或环境变量，门禁不会读取别的工作树配置。

| 参数 / 环境变量 | 默认 | 含义 |
|---|---|---|
| `-config` | `config.e2e.json` | 仅连接配置，已 gitignore |
| `-project` / `PROJECT` | `scratch` | 主目录在 hub 本机的测试项目 |
| `-agent` / `AGENT` | `claude` | hub 上的协调 agent |
| `-target-node` / `TARGET_NODE` | `node-b` | 普通 fleet 指定的远端机器 |
| `-target-agent` / `TARGET_AGENT` | `shipper` | 普通 fleet 指定的远端 agent |
| `-scenario` / `SCENARIO` | `delegate` | `autonomous` 由协调 agent 自己挑目标；make e2e-autonomous 显式选择它 |
| `-timeout` | delegate 为 `10m`，autonomous 为 `20m` | 可缩短，必须为正数且不能超过对应上限 |

自主门禁还要求项目主目录有 `kvtool/main.go`，在线远端 node 的 `capabilities` 包含 `build`，并有可编译、可写文档的 agent。它检查至少两个子任务成功且执行有重叠、编译产物确实来自申报 build 的远端、README 校验章节与发布说明落地、`sha256sum -c` 成功、二进制是 ELF x86-64，以及子任务 attempt 和实际报告的用量。hub 需有 `sha256sum` 与 `file`。`steve_await` 次数超过子任务数的两倍会失败，避免把轮询当自主协调。

门禁的客户端超时 **不会取消服务端工作**。超时后按输出中的 conversation/task/attempt ID 去控制台检查，必要时用 `/cancel`。成功输出分别以 `FLEET PASS`、`AUTONOMOUS PASS` 收尾；PR 证据要求及需重跑门禁的变更范围见 [CONTRIBUTING.md](../CONTRIBUTING.md)。贴输出前移除 token。

## 排障

先用资源页、任务详情、历史与审计关联机器、会话、任务 ID 和 attempt ID，再看对应 hub/node 日志。`steve top -once` 和 `/fleet` 可确认运行中的机群；`steve dash` 只检查读模型并打印 URL，不会启动另一个 hub。

| 日志或现象 | 含义与处理 |
|---|---|
| `expired attempt of the previous process: ... hub restarted` | hub 启动时把前一个进程留下的活 attempt 置为过期，释放旧执行占用；不是“新任务刚启动就超时”。继续找该 task 的恢复或停止记录。 |
| `console: resuming task #...` | 为控制台任务建立恢复交换并继续；若出现 `console: resume task #...: ...`，查看后面的具体恢复错误。 |
| `console restarted before this exchange completed` | 上一个进程未完成的 running 交换被明确结算为错误；任务若符合恢复条件，会另开恢复交换。 |
| `node: <name> disconnected (connection ...)` / `node: <name> up ...` | 节点连接断开/重新连上；仅有 up 不代表原 agent 流已经续接，继续找 stream 日志。 |
| `node: <name> reattached stream ... (after output ..., accepted input ...)` | hub 收到进程流的续接确认，记录输出游标和已接受输入位置。 |
| `steve-node: reattached stream ..., replayed N lines` | node 回放缺失输出并切回实时流；`N=0` 也可能是正常续接。 |
| `session grace exceeded` / `too old` / `stream is not resumable` | 宽限用尽、输出已不在 journal 保留范围或流不可恢复。检查断线时长、node 是否重启及磁盘/日志错误；改长 prompt_timeout 不能修复丢失的进程。 |
| `delegate: ... task #C under #P on ...` | 子任务 C 已创建并交给所列机器，P 是父任务；后续靠这些 ID 查 attempt 与产物。 |
| `delegate: delivered N child result(s) into ... for task #P` | 父会话投递接口已接受结果，delivery 状态已处理；不代表父 agent 已完成汇总，也不能单凭此行认定文件落地。查看消息中的落地状态、任务改动和主目录。 |
| `delegate: deliver ...: <error>` | 结果投递失败，结果留在任务记录，待后续 flush 或启动补投递；先处理日志中的队列/持久化错误。 |
| `sweep: landing ...: <state> (N paths)` | 每 30 秒的后台任务重试待落地产物；看实际 state，不能把这行一律当成功。 |
| `sweep: land pending for <project>: <error>` | 后台落地遇到非占锁错误；查看对应产物/landing。规范锁被占用时继续排队，不打印这一错误。 |
| `sweep: removed N orphaned worktree(s) on ...` | 清理没有活 attempt 持有的隔离工作树；启动及 node 连接时会触发检查。 |
| `sweep: worktrees on <node>: <error>` | 该机器的孤儿工作树清扫失败；检查可达性、目录和文件权限。 |
| `the console needs feishu.owner_open_id` | owner 未配置；补上当前应用的 open_id。只有 read-model token 不足以建立 owner。 |
| `Authentication required`，但资源页 harness 可用 | 可执行程序存在/能启动不等于模型认证有效；检查 node 的登录环境、认证链接和隔离 home，尤其不要用裸环境启动 node/nodectl。 |
| `harness ... missing` 或 `start agent ...` | 检查目标机器命令的绝对路径、执行权限、解释器及 PATH；不是只检查 hub 的安装。启动探针有 3 秒期限。 |
| `this node is served by hub ...` / `this node belongs to hub ...` | 机器已有活 hub 或仍在归属宽限；确认连接的 hub 名称和 token，按明确的移交流程处理。 |
| `mcp_servers and mcp_broker are exclusive` | node 同时配置了内置服务和独立 broker；定义应只由其中一个持有。 |
| 有回复但记忆没有变化 | 只在 owner 私聊/控制台允许写；项目 scope 只能是会话绑定项目，委派子任务不能写。记忆更新在新会话首轮注入，当前上下文不会立即重注入；同 scope 幂等键 24 小时内重放原回执。 |

排查快照时，记住影子仓库会收录未提交文件，并按快照忽略/大小规则筛选；不能从“源仓库没 commit”推断没有产物。排查委派时，区分 **子任务完成 → 产物耐久 → 落地 → 结果投递 → 父任务续跑**，以实际账本状态、变更索引和目标文件为准。

从备份恢复账本属于单独的维护流程：停止使用该状态目录的 hub 后，按 [cmd/steve/ledger.go](../cmd/steve/ledger.go) 的命令执行 `steve ledger rotate -config ...`、`steve ledger recover -config ...`，使旧租约失效并对账；用 `steve ledger status -config ...`、`steve ledger effects -config ...` 检查。不确定的外部动作由 owner 在 `/effects` 中裁决，不能因为重试就假设它从未发生。普通连接抖动不需要 rotate/recover。

历史文档中的 `steve config apply` 和 `steve-node streams` 都不是当前已实现的命令；当前排障入口是上面的控制台、read model、日志和 ledger 子命令。
