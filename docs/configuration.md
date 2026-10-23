# 配置参考

Steve 的独立服务读取 `config.json`，执行节点读取 `node.json`。两者都拒绝未知字段。表中默认值指省略字段时的行为；样例配置中的显式值可以不同。

[最小控制台配置](../config.console.example.json)适合单机，[完整示例](../config.example.json)包含多机、区域和 MCP，[节点示例](../node.example.json)用于执行机器。替换路径、身份、地址和凭据占位符，并删除不用的声明。

时长使用 Go duration 字符串，例如 `"30s"`、`"10m"`。命令参数不经过 shell 展开，执行文件和跨节点路径使用绝对路径。状态目录、配置文件及凭据由部署用户私有保存。

桌面与集群的共享声明通过控制台管理；配置文件中的身份、状态路径和集群 sidecar 必须保留。启动与修改方法见[运维指南](operations.md)。

## hub 顶层

Hub 配置声明可用 agent、项目和连接。样例中的工具与项目需要适配实际部署。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `agents` | object<string, Agent> | 可为空 | 命名执行配置；非空时恰有一个默认 agent | `{"codex":{"harness":"codex","default":true}}` |
| `projects` | object<string, Project> | 无，至少一项 | 声明项目；`home` 是保留项目名 | `{"work":{"home":{"path":"/srv/work"}}}` |
| `harnesses` | object<string, Harness> | 无，至少一项 | hub 本机启动命令与权限策略；agent 引用的 harness 必须在此登记 | `{"codex":{"command":"/home/me/.local/bin/codex-acp"}}` |
| `nodes` | object<string, Node> | `{}` | hub 如何连接远端机器；能力来自 node 的实际申报 | `{"host-3":{"addr":"10.0.0.3:7701","token":"replace-me"}}` |
| `mcp_servers` | object<string, MCPServer> | `{}` | hub 本机 MCP 定义 | `{"docs":{"type":"http","url":"https://mcp.example.com/mcp"}}` |
| `feishu` | Feishu object | 可省略；启用时凭据成对填写 | 可选飞书/Lark 连接、访问规则与该通道 owner | `{"app_id":"cli_...","app_secret":"replace-me","owner_open_id":"ou_..."}` |
| `gateway` | Gateway object | 各字段按下表 | 状态目录、控制台、预算与协调设置 | `{"owner_id":"local-owner","read_model_addr":"127.0.0.1:7710"}` |
| `policies` | Policies object | 各组采用下文默认值 | 执行、规划、快照与审阅限制 | `{"execution":{"step_timeout":"15m"}}` |

## `projects.<name>`

项目必须有主目录，`home` 是保留项目名。任务创建时固定项目；切换会话项目不会迁移既有任务。主目录和副本在同一节点上不得重复或嵌套。

普通交互使用执行机器上的主目录或就绪副本，`repo: isolated` 不会改变这一目录选择。委派和计划步骤使用隔离目录并合并落地。项目页的目录名相对于目标机器工作区的 `projects/`，配置文件保存解析后的绝对路径。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `home` | ProjectHome object | 必填 | 唯一主目录的机器和路径 | `{"node":"host-3","path":"/srv/lab"}` |
| `home.node` | string | `""`（hub） | 主目录所属机器，非空必须在 `nodes` 中 | `"host-3"` |
| `home.path` | string | 必填 | 主目录；远端路径不在 hub 上展开 | `"/srv/lab"` |
| `level` | string | `"internal"` | 数据等级：`public`、`internal`、`restricted`、`sealed` | `"restricted"` |
| `repo` | string | `"inplace"` | 接受 `inplace` / `isolated`；普通交互均使用主目录或副本，不由此字段隔离 | `"isolated"` |
| `skills` | string[] | `[]` | 固定到项目的技能目录，在 hub 读取 | `["/home/me/steve-skills/review"]` |
| `durable_places` | string[] | `[]`（hub）；sealed 会补入 home 所在机器 | 哪些机器的产物回执算耐久；其中任一有效回执即可，`""` 表示 hub | `[""]` |
| `external_remote` | string | `""`；克隆时尝试使用主目录根仓库的 remote | 控制台创建工作区副本时优先使用的克隆来源；加载配置本身不克隆 | `"git@github.com:example/work.git"` |
| `grants` | object<string, string> | `{}` | principal（owner 标识或飞书 open_id）到 `none` / `read` / `write` / `admin`；owner 总是 admin | `{"ou_...":"write"}` |
| `default_role` | string | public/internal 为 `write`，restricted/sealed 为 `none` | 未单独授权的用户角色，可显式设上述四种值 | `"none"` |
| `workspaces` | ProjectWorkspace[] | `[]` | 主目录之外的已有工作区，一台机器一个；配置不会自动克隆，sealed 不允许副本 | `[{"node":"host-3","path":"/srv/work-copy"}]` |
| `workspaces[].node` | string | `""`（hub） | 副本所在机器，不能是项目 home 所在机器 | `"host-3"` |
| `workspaces[].path` | string | 必填 | 该机器上的副本绝对路径 | `"/srv/work-copy"` |

## `agents.<name>`

Agent 是执行配置，不拥有独立项目目录。偏好模型和选项必须来自目标 harness 实际报告。能力选择器支持 `kind:id`、模式、或、否定与版本条件；harness、tool、hardware、model、skill、mcp、tag 可参与调度，network、credential、a2a 仅展示。

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

## `harnesses.<name>`

`adapter` 与 `command` 二选一。内置适配器固定版本并校验摘要；自备命令由部署者安装。权限策略针对 ACP 工具请求，既有操作系统权限与沙箱仍然有效。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `adapter` | string | `""` | 内置清单里的适配器名（`codex-acp`、`claude-agent-acp`、`pi-acp`）。steve 按钉死的版本取到缓存、校验后运行；与 `command` 二选一，不能并存，也不接受 `args` | `"codex-acp"` |
| `command` | string | 与 `adapter` 二选一，必须给一个 | 自己预装的适配器可执行文件；hub 直接启动，不做 shell 展开 | `"/home/me/.local/bin/codex-acp"` |
| `args` | string[] | `[]` | 直接传给命令的参数；`adapter` 不接受 | `[]` |
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

### pi

pi 不为工具调用请求授权，`pi-acp` 也不把文件读写和终端交给 Steve，所以上表的逐次审批对 pi 不适用。Steve 改为控制 pi 能加载哪些工具：

| 策略 | pi 的工具 |
|---|---|
| `read`、`deny` | 只有 `read`、`grep`、`find`、`ls`；不能写文件或运行命令 |
| `write`、`auto`、`always_allow` | pi 默认工具（`read`、`bash`、`edit`、`write`），执行前不询问 |

Steve 在 `<state>/runtimes/pi/` 下为 pi 准备 `read-only` 与 `full` 两个隔离目录（`PI_CODING_AGENT_DIR`），按会话的策略选用。两者都从 `~/.pi/agent/settings.json` 继承模型选择（`defaultProvider`、`defaultModel`、`defaultThinkingLevel`、`enabledModels`、`modelThinkingLevels`），不继承扩展、包、主题和技能路径；`auth.json` 与 `models.json` 以符号链接指向原文件，不复制密钥。两者都设置 `defaultProjectTrust: "never"`，工作目录里的 `.pi` 设置、MCP 与扩展不会加载。Steve 下发的技能放在两者共用的 `skills/` 目录。

`harnesses.pi.env` 里的 `PI_ACP_PI_COMMAND` 是登记时检测到的 `pi` 路径。自己设置了 `PI_CODING_AGENT_DIR` 时，Steve 不替换它；该目录不是 Steve 准备的只读目录时，只读策略下 pi 不会启动。

`pi-acp` 不接 Steve 提供的 MCP 服务（包括 Steve 自己的协作工具），pi 的会话不会被提供这些工具；pi 的历史会话也不能导入。`pi-acp` 固定把会话索引写在运行用户的 `~/.pi/pi-acp/session-map.json`，只记录会话 ID、工作目录和会话文件路径，不含凭据；会话内容本身在 Steve 的隔离目录里。
## ACP 工作区文本文件

Unix 的本机与节点托管会话支持 `fs/read_text_file`、`fs/write_text_file`。请求只在原活跃回合受理，路径固定在原工作区内，写入遵循工具审批。仅处理普通 UTF-8 文本，不跟随写入目标的符号链接，不覆盖多硬链接文件。

单次文本和 JSON 响应、写入内容上限为 1 MiB，读取扫描上限为 8 MiB。行号从 1 开始；行数上限为 0 返回空内容。Windows 与旧式进程流不声明此能力。这些回调限制不是任意 agent 程序的操作系统沙箱。

## ACP 节点终端

Unix 节点托管会话可提供 ACP terminal 创建、读取、等待、终止与释放。命令以 argv 在原工作区内执行，不隐式加入 shell，遵循 `execute` 审批。协调端和节点都必须支持终端准入。

每个活跃回合最多 32 个终端资源、8 个长期创建/等待回调，输出预算合计 1 MiB，单终端默认 32 KiB。超限保留合法 UTF-8 尾部。取消等待不终止命令；回合结束、取消或会话停止清理所属终端。

节点重启清理能确认属于原执行的进程，不重放命令或恢复终端输出。终端记录为 format 2，不支持该格式的软件拒绝加载。进程归属无法证明时保留停止待确认。

## `mcp_servers.<name>`

这里定义外部 MCP；Steve 自己的 `steve_*` 工具由平台按会话授权。远端 MCP 的定义和凭据保存在执行节点或独立 broker。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `type` | string | hub 无有效默认，需显式填写 | `stdio`、`http` 或 `sse` | `"stdio"` |
| `command` | string | `""`；stdio 必填 | 本机已安装的 MCP 可执行命令 | `"/home/me/.local/bin/mcp-server-filesystem"` |
| `args` | string[] | `[]` | stdio 命令参数 | `["/srv/work"]` |
| `env` | object<string, string> | `{}` | stdio MCP 服务环境；与 harness 的 `env` 数组格式不同 | `{"LANG":"C.UTF-8"}` |
| `url` | string | `""`；http/sse 必填 | 完整 HTTP(S) 服务地址 | `"https://mcp.example.com/mcp"` |
| `headers` | object<string, string> | `{}` | http/sse 请求头 | `{"Authorization":"Bearer replace-me"}` |

## feishu

飞书 / Lark 是可选通道。`app_id` 和 `app_secret` 成对配置，owner 使用该应用下的 open_id。`enabled` 可显式启停，省略时按凭据判断；默认通道必须保持启用。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `app_id` | string | 必填 | 飞书/Lark 自建应用 ID | `"cli_..."` |
| `app_secret` | string | 必填 | 应用密钥 | `"replace-me"` |
| `domain` | string | `"feishu"` | `feishu` 或 `lark` | `"lark"` |
| `owner_open_id` | string | `""` | 飞书/Lark owner；也是未设置 `gateway.owner_id` 时的控制台 owner 后备值 | `"ou_..."` |
| `allowed_senders` | string[] | `[]` | `group_policy=allowlist` 时的群聊发送者名单，空名单拒绝全部群聊；私聊不使用此名单 | `["ou_..."]` |
| `blocked_senders` | string[] | `[]` | 群聊和私聊均拒绝这些发送者，优先于其他规则 | `["ou_..."]` |
| `group_policy` | string | `"open"` | `open` 允许群聊，`allowlist` 仅允许名单命中者（空名单拒绝全部），`disabled` 禁止群消息；阻止名单始终优先 | `"disabled"` |
| `allow_unmentioned` | boolean | `false` | 接收未 @ bot 的群消息，再由参与策略决定是否响应 | `true` |

## gateway

控制台 token 具有 owner 权限。Loopback 监听未提供 token 时在状态目录生成 `loopback-token`；非 loopback 必须显式配置。远程接入见[控制台与凭据](operations.md#控制台与凭据)。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `hub_id` | string | 首次从状态目录生成并持久化的 ID | Hub 的稳定身份；不能借修改 ID 接管其他 Hub 的项目 | `"hub-a"` |
| `peers` | object<string, HubPeer> | `{}` | 稳定 Hub ID 对应的配置记录；每项含 `name`、`url`、`token`，URL 禁止用户凭据、查询参数和 fragment；不代表在线状态，不启用自动接管 | `{"hub-b":{"url":"https://hub-b.example","token":"replace-me"}}` |
| `owner_id` | string | 后备到 `feishu.owner_open_id` | 控制台 owner 标识；独立控制台需要有效 owner | `"local-owner"` |
| `locale` | string | `lark` 域为 `en`，其他为 `zh` | 系统回复默认语言，接受 `""` / `zh` / `en`；浏览器语言另行选择 | `"en"` |
| `default_channel` | string | 启用飞书时 `"feishu"`，否则 `"console"` | 填充授权消息锚点缺省的通道；只能指向已配置通道，不改变收件人 | `"console"` |
| `node_binary` | string | `""` | 引导脚本可下载的 steve-node 文件；不填则需预先复制 | `"/home/me/steve-bin/steve-node"` |
| `prompt_timeout` | Duration string | `"10m"`；非正值也取此默认 | 一轮没有文本、工具调用或报告的静默超时；不是总时长上限 | `"15m"` |
| `state_path` | string | `"~/.steve/state.json"` | 旧状态文件路径，其父目录决定账本、运行状态和锁的位置 | `"/home/me/.steve/state.json"` |
| `home_path` | string | `state_path` 父目录下的 `home` | Steve 身份、用户档案与全局记忆目录；保留项目 `home` 的主目录 | `"/home/me/.steve/home"` |
| `task_max_turns` | integer | `0`（不限） | 为新任务设置最大轮数 | `100` |
| `task_max_elapsed` | Duration string | `"0s"`（不限） | 为新任务设置执行耗时预算 | `"2h"` |
| `offline_reminder_after` | Duration string | `"15m"`；零取默认 | 长回合结束且用户期间未继续说话时，额外通知；负数关闭 | `"-1s"` |
| `debug_addr` | string | `""`（关闭） | 调试消息/卡片回调注入接口，只允许 loopback 地址：配置文本须是 loopback IP 或不区分大小写的 `localhost`，绑定后实际地址也须是 loopback IP（`localhost` 被解析到其他地址时不满足）；任一检查不过时调试端点不启动，Hub 照常运行，只记一条 ERROR 日志说明原因；没有 token，本机其他用户和进程都能注入消息，仅在单用户机器上调试时开启 | `"127.0.0.1:7711"` |
| `debug_chat_id` | string | `""` | 调试注入的默认飞书 chat ID | `"oc_..."` |
| `capabilities` | string[] | `[]` | hub 的兼容能力标签 | `["build"]` |
| `tools` | string[] | `[]` | hub 需要观测的命令 | `["git","go","gh"]` |
| `declares` | string[] | `[]` | 运维声明的能力，不能替代工具的实际观测 | `["network:internal"]` |
| `read_model_addr` | string | `"127.0.0.1:7710"` | 控制台、状态快照和事件流监听地址；首尾空白会去掉，只含空白时取默认值 | `"0.0.0.0:7710"` |
| `read_model_token` | string | `""`（loopback 监听时生成 `<状态目录>/loopback-token`） | 控制台/API 的 bearer token，拥有 owner 操作权限；首尾空白会去掉，只含空白视为未设；非 loopback 监听必填，loopback 监听未设时 Hub 首次启动生成并沿用（loopback 监听的判断见[控制台与凭据](operations.md#控制台与凭据)） | `"replace-me-with-a-long-random-token"` |
| `planner` | string | `""`（规则规划器） | `/plan` 的拆解 agent；不填时开放目标按一步处理 | `"claude"` |
| `level` | string | 按 hub 需耐久保存的项目推导，至少 `restricted` | hub 的数据等级；默认排除 home 在远端的 sealed 项目 | `"restricted"` |
| `region` | string | `"default"`（账本的本地区域） | 本 hub 的租约签发区域 | `"east"` |
| `regions` | object<string, Region> | `{}` | 其他区域的租约签发方 | `{"west":{"url":"http://10.0.0.9:7720","token":"replace-me"}}` |
| `issuer_addr` | string | `""`（关闭） | 对其他区域提供本地租约签发 HTTP 服务；Hub 启动时绑定该地址，绑定失败时 Hub 不启动（错误见[排障](operations.md#排障)），启动日志 `steve: issuing region <区域> leases on <地址>` 记录实际监听的地址 | `"0.0.0.0:7720"` |
| `issuer_token` | string | `""` | 本地租约签发服务认证 token | `"replace-me-with-an-issuer-token"` |
| `direct_transfer` | boolean | `false` | 允许持有产物的 node 在 hub 授权下直传给另一 node；关闭时经过 hub | `true` |
| `default_project` | string | 只有一个项目时自动选它，否则 `""` | 新会话未选择项目时的绑定；owner 私聊（飞书私聊和控制台）默认使用保留项目 home。绑定前，控制台和内置工具按同一规则显示会话将绑定的项目；本进程还没收到消息的飞书会话分不出群聊或私聊，显示为未绑定 | `"work"` |
| `default_approval` | string | `""`（不设置） | 全局默认审批策略：`ask`、`auto`、`full`，见下文 | `"auto"` |

`task_max_turns` 和 `task_max_elapsed` 的 0 表示不限。`prompt_timeout` 是无文本、工具或报告的静默超时，默认 10 分钟，与总执行时长和节点续接宽限不同。结果提交、落地及进程清理各有独立期限；超时后以原任务状态为准。

## 默认审批策略

`gateway.default_approval` 接受 `ask`、`auto`、`full`，分别表达需要确认、工作区内自主处理、完全放行的意图。Steve 按 harness 实际提供的模式映射，不把规划模式当审批模式。

优先级为：会话单独设置 > Agent 原生审批选项 > 全局默认 > 工具默认。默认值在打开或恢复原生会话时应用，不中断活动任务；导入的原生会话保留原设置。工具无对应选项时保持原样并记录原因。

控制台可设置全局及 Agent 默认。「恢复 Agent 跟随」清除 Agent 的单独审批选项，不清除会话设置。保存全局值本身不会执行该操作。

## policies

策略位于顶层 `policies`。配置省略或零值采用下表默认值，控制台要求显式正值。策略变更在重启后生效；读取超限会标记截断。整数范围为 1–9007199254740991。

| 键 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `execution.step_timeout` | Duration string | `"15m"` | 单个执行步骤的超时 |
| `execution.verify_timeout` | Duration string | `"10m"` | 验证执行的超时 |
| `planning.timeout` | Duration string | `"3m"` | 规划请求超时 |
| `planning.attempts` | integer | `2` | 规划尝试次数 |
| `snapshot.max_files` | integer | `20000` | 快照文件数量上限 |
| `snapshot.max_bytes` | integer bytes | `2147483648`（2 GiB） | 快照总字节上限 |
| `snapshot.max_file_bytes` | integer bytes | `209715200`（200 MiB） | 快照单文件上限，不能大于总字节上限 |
| `review.max_changes` | integer | `500` | 返回的变更文件数量上限 |
| `review.max_diff_bytes` | integer bytes | `204800`（200 KiB） | Diff 读取上限 |
| `review.max_file_bytes` | integer bytes | `204800`（200 KiB） | 源码文件读取上限 |
| `review.max_entries` | integer | `2000` | 目录列表条目上限 |
| `review.timeout` | Duration string | `"30s"` | 审阅读取超时 |

## `nodes.<name>` 与 `gateway.regions.<name>`

`nodes` 是协调端的连接声明，执行程序、模型与工具可用性由节点实际报告。集群中的节点 owner 使用 cluster ID；独立 Hub 使用稳定 hub ID。

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

## node.json

执行节点的配置与 Hub 连接声明独立。`hubs` 将 token 绑定到稳定协调身份；顶层 token 不会因配置了 `hubs` 而自动失效。`Source`、`SessionGrace`、`FaultDropAfter` 不能写进 JSON。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `name` | string | 必填 | node 申报的名字，应与 hub 中登记的机器名一致 | `"host-3"` |
| `listen` | string | `"0.0.0.0:7701"` | 接收 hub 连接；`-listen` 参数可覆盖 | `"10.0.0.3:7701"` |
| `token` | string | 必填，即使配置了 `hubs` | 通用 hub 认证 token；若也列在 `hubs`，名称同样受约束 | `"replace-me-with-a-long-random-secret"` |
| `hubs` | object<string, string> | `{}` | 稳定 Hub ID → token；表中的 token 必须匹配该 ID，不会自动禁用未列入表的顶层 token | `{"hub-a":"replace-me-with-a-long-random-secret"}` |
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

| 环境变量 | 默认 | 作用与示例 |
|---|---|---|
| `STEVE_NODE`（hub） | 主机名；无法读取时 `local` | 固定 Hub 所在机器的显示名称；不改变稳定 `gateway.hub_id` 或节点归属。 |
| `STEVE_NODE_SESSION_GRACE`（node） | `10m` | 进程流续接宽限，必须为正 duration，例如 `15m`；通过 advert 告知 hub。 |
| `STEVE_NODE_FAULT`（node） | 关闭 | 测试故障注入，例如 `drop-hub-after:20s`；会切断 hub 连接，不用于正常部署。 |

## 独立 broker 的 mcp.json

用于 `steve-node mcp-broker -config mcp.json`。定义与敏感值由 broker 持有；同一节点不能同时配置有效的独立 broker 和内置 MCP 定义。

| 键 | 类型 | 默认 | 作用 | 示例 |
|---|---|---|---|---|
| `socket` | string | 必填 | 供 node 和 MCP launcher 访问的 Unix socket | `"/home/me/.steve-mcp/mcp.sock"` |
| `token` | string | 必填 | 控制接口认证，与 node 的 `mcp_broker.token` 对应 | `"replace-me-with-a-separate-broker-token"` |
| `mcp_servers` | object<string, MCPSpec> | `{}` | 同 node MCP 字段表中的完整服务定义；密钥放这里 | `{"docs":{"type":"http","url":"https://mcp.example.com/mcp"}}` |
| `workspace_root` | string | `""`（broker 当前目录） | stdio 服务的启动目录 | `"/home/me/steve-work"` |
| `port_file` | string | `""`（不持久化代理端口） | 保存 HTTP loopback 代理端口 | `"/home/me/.steve-mcp/proxy.port"` |
| `launcher` | string | broker 自己的可执行文件 | agent 调用 `mcp-launch` 的 steve-node 路径 | `"/home/me/steve-bin/steve-node"` |
| `socket_mode` | integer | `0` → `0600` | socket 权限位；JSON 用十进制，跨用户共享组的 `0660` 为 432 | `432` |
