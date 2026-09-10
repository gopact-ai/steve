# 插件操作与作者指南

控制台「插件」页管理项目能力包：导入、审核、配置机器与凭据引用、启用、应用预设、升级/回退、查看引用和安全移除。本地 CLI 仅提供包准备与节点凭据管理；CLI 的 `prepared` 表示固定内容及本地回执已保存，不代表会话已启用。

## 使用

构建 `steve` 后可直接预览仓库里的两个样例：

```sh
./steve plugins preview -source examples/plugins/github
./steve plugins preview -source examples/plugins/team-tools
```

输出 JSON 包含 `manifest`、`digest` 和已解析的 `source`。审核内容后，将输出的 SHA-256 摘要原样带入准备命令；存储目录必须显式指定，父目录须已存在：

```sh
./steve plugins prepare -source examples/plugins/github \
  -store /tmp/steve-plugins -command-id github-001 -digest <preview中的digest>
./steve plugins list -store /tmp/steve-plugins
./steve plugins show -store /tmp/steve-plugins -digest <preview中的digest>
```

所有命令都使用独立的本地包存储，不加载 hub 配置，不建立模型会话。`preview` 不创建包存储；读取 Git 时只使用会被清理的临时裸仓库。`prepare` 不执行包内代码或安装脚本。

同一 `command-id`、摘要和来源重试，返回原回执；完成准备后，来源目录被删除也能重放回执。来源自预览后改变时，摘要校验拒绝准备。已经开始准备的同一包 ID/版本不能换内容；发布改动请使用新版本。

固定 Git 提交的例子：

```sh
./steve plugins preview -source /absolute/path/to/repository \
  -commit <完整的小写commit-ID> -subdir examples/plugins/github
```

Git 来源接受绝对本地仓库路径和不含凭据的 HTTPS URL；提交必须是完整的 40 或 64 位小写十六进制对象 ID。分支、标签、SSH/外部 Git helper 和 URL 内的凭据不被接受。私有包可以先在本机通过已有授权获取仓库，再读取固定提交。读取使用隔离 Git 配置、不 checkout，不运行仓库 hooks、过滤器或 LFS smudge；包内符号链接和 submodule 会被拒绝。

## 清单 schema 1

包根目录必须有 `plugin.json`，可直接参考 [GitHub 样例](../examples/plugins/github/plugin.json) 和 [团队工具样例](../examples/plugins/team-tools/plugin.json)。

| 字段 | 契约 |
|---|---|
| `schema` / `api` | 必须是 `1` / `steve.plugins.v1` |
| `id` | `namespace/name`；每段为 1–64 个小写字母、数字、点、下划线、连字符，以字母或数字开头 |
| `version` | 固定 `major.minor.patch`，可带小写预发布标识；不接受 `latest`、范围或前导零数字段，不含 build metadata |
| `description` | 非空描述 |
| `platforms` | 可选 OS/Arch 组合：linux/darwin、amd64/arm64；这是声明，当前准备命令不判断执行机器能否运行 |
| `settings` | 配置声明：描述、是否必填、是否为 secret、可选普通默认值；secret 无默认值 |
| `skills` | 包内技能名到相对目录的映射；必须实际含 `SKILL.md` |
| `mcp` | 包内服务名到固定 MCP 定义的映射 |
| `agents` | Agent 预设：harness、模型偏好、options、system prompt、包内技能/MCP 引用；导入只保存预设，在页面预览并应用后创建或更新 Agent |

字段名大小写严格匹配，拒绝未知字段、任意层级的重复对象键与多个顶层 JSON 值；不通过忽略字段实现未来能力的静默降级。

MCP 支持两种声明：

- `http` / `sse`：URL、可选 headers。URL 可以是普通配置引用，不能带用户凭据或 fragment；header 名使用 HTTP 规范形式，例如 `Authorization`。保存远端配置不等于固定远端服务实现。
- `stdio`：`program.path` 指向包内实际文件；可直接执行已标记可执行的文件，或声明现有 `node` / `python3` 解释器。`args` 是参数数组，`env` 是环境映射。无 URL、headers、安装钩子或自由 shell 命令入口。解释器及环境依赖的可用性留给节点准备阶段验证。

URL、header、参数和环境值使用同一个引用结构：`text` 为字面量，`config` 引用普通设置，`secret` 引用节点凭据声明，二者必须匹配 `settings` 中的类型。`prefix` 仅用于引用值前缀，例如 `Bearer `。当前命令只校验和保存引用，不解析凭据或调用远端服务。

逻辑能力身份由包 ID、能力种类和包内名称组成；原生名称由该身份派生。节点装配拒绝原生文件名冲突，不覆盖既有文件。同一包不能重复启用到重叠的项目和节点。

## 内容与存储

本地目录只接收普通文件，排除根目录的 `.git` 元数据；拒绝隐藏条目、符号链接、设备文件、路径逃逸和在大小写不敏感/Unicode 规范化文件系统上会冲突的路径。内容读取使用受限文件系统根，不跟随链接读取目录外内容。

清单规范化为 JSON，文件按名字排序打包；时间、所有者和普通权限差异不进入摘要，可执行位保留。因此相同文件的本地快照与对应 Git 提交产生相同归档。上限为：清单 128 KiB，单文件 8 MiB，整个归档 32 MiB，文件与目录合计 4096 项，路径 1024 字节/最多 32 个分隔符。

本地存储布局：

```text
<store>/
  prepare.lock
  requests/<command-id>.json
  packages/<sha256>/bundle.tar
  packages/<sha256>/content/...
  receipts/<command-id>.json
```

先持久保留命令与发布身份，再在独立暂存目录中写入并同步完整内容，原子发布内容目录，最后写回执并同步父目录。并发进程通过文件锁串行发布；第一版写入支持 Unix，其他平台明确拒绝。

如果目录同步失败，命令返回错误；同一身份重试会检查已写内容并重新确认持久性。包已发布但回执尚未生成时，也能用已保存内容继续。旧暂存目录不会参与下一次解包；进程被强制终止时留下的暂存目录暂不自动 GC，只占用磁盘。

`show` 和准备重放会验证归档及物化内容；发现损坏时拒绝，不能在同一身份下静默修复。`list` 列出并校验安装回执与原请求的关系，不代替完整内容验证。

这里的不可变指安装 API 不原地更新已保存的包，并在使用时核验内容；包存储由本机用户管理，不是对同 UID 恶意进程的防篡改沙箱。CLI 不提供激活或删除命令；这些动作使用管理页面/API 核查持久会话引用和节点状态，不能仅凭本地目录判断。

## 节点本地凭据

在执行节点本机配置凭据，不把值经过 hub。`secret-put` 从 stdin 读取一个最多 16 KiB 的 UTF-8 值，只输出随机版本引用和创建时间；末尾的一次换行会去掉。同名写入创建新版本，保留旧版本供旧部署引用：

```sh
./steve plugins secret-put -store /path/to/node-state/plugins -name github-token
./steve plugins secret-list -store /path/to/node-state/plugins
```

第一条命令从 stdin 读取至 EOF。不要把值写在命令行参数里。得到的 `{name, revision}` 可以用于节点部署配置；协调端只读取引用和可用性，不能查询凭据值。包清单的 `settings` 声明仍决定它是普通配置还是 secret，不能用普通配置字段绕过类型检查。

节点的 `plugin_packages.v1` 协议目前提供 prepare、inspect 和凭据元数据查询；它校验部署的项目范围、目标节点、包摘要、普通配置、凭据版本和平台/解释器需求。内部 `Selection` 固定会话的部署集合，通过 `plugin_runtimes.v1` 准备并绑定实际运行目录。`plugins.Library` 可把包内容按项目复制到独立节点，并从同一账本记录恢复；本地 CLI 的独立缓存不自动加入该库。

## 部署到节点需要协调者

节点只接受**已提交的集群协调者**发来的插件操作，和 node-owned 会话同一层：运行目录、凭据解析结果和固定版本都是耐久状态，不能由一个已经下台的协调者改写。检查在 `internal/node/plugins.go`，凭据引用与部署请求都过它。

因此 `steve run` 这样起的独立 hub 可以导入包、保存安装配置、管理自己这台机器的部署，但**不能把安装准备到别的节点**：它没有身份可以出示。控制台的目标状态会直接说明这一点（`this hub is not a cluster coordinator, so it cannot deploy plugins to nodes`），管理接口在请求发出前就拒绝，插件会话也用同一句失败，而不是把节点那句"协调者未授权"抛给运维。

要把插件部署到机群里的节点，hub 要作为集群应用运行（`steve peer`，见桌面多机接入）。这是第一轮的边界，不是临时故障。

## 管理 API 与运行引用

运行中的 Steve 现有以下 owner 鉴权入口：

| 路径 | 用途 |
|---|---|
| `GET /console/plugins` | 包、期望安装、节点准备记录、最近观察到的不可用状态和管理操作 |
| `POST /console/plugins/preview` | 校验来源并返回固定摘要和清单 |
| `POST /console/plugins/import` | 带 `command_id`、`project`、`digest`、`source` 导入到项目包库 |
| `PUT /console/plugins/installations/{id}` | 带 `base_revision` 和 `installation` 保存期望配置 |
| `POST /console/plugins/installations/{id}/prepare` | 重试目标机器准备并显示逐节点结果 |
| `GET /console/plugins/nodes/{node}/secrets` | 只查询该节点已有凭据引用 |
| `POST /console/plugins/installations/{id}/presets/preview` | 查看新建、更新或显式采用已有 Agent 的差异 |
| `POST /console/plugins/installations/{id}/presets/apply` | 使用稳定 `command_id` 与 Agent `base_revision` 保存预设 |
| `GET /console/plugins/installations/{id}/usage` | 会话、归档、待恢复执行、准备中引用、节点运行记录及旧版本 |
| `POST /console/plugins/installations/{id}/runtimes/{runtime}/close` | 拒绝活动回合；确认原生进程退出后释放恢复引用 |
| `DELETE /console/plugins/installations/{id}` | 使用 `base_revision` 移除已停用、无引用且节点可核实的安装 |

安装声明为 `package_id`、`digest`、`enabled`、`projects` 和 `targets`。每个 target 的配置包含普通 `values` 与 `{name, revision}` 形式的 `secrets` 引用。配置验证和 owner 项目范围检查由后端执行；没有导入到所选项目的包不能直接启用。保存配置不等于所有节点已就绪，节点离线或缺依赖会保留明确状态并重试。

已启用、符合项目和节点范围的安装会在创建新会话时解析。已有普通会话不会在中途自动增加插件。插件会话将 runtime 引用保存到 attempt 和会话记录，新版本影响新会话；旧会话继续使用原版本及目录。停用阻止新绑定，改变项目或节点授权会拒绝继续使用被撤销范围；在「会话引用与移除」中查看所有阻塞项并显式关闭空闲会话。

运行目录还保存原生会话数据、配置、固定技能、MCP 地址和本地能力 token。缺失或被改写的技能会被拒绝，不能用最新版重新创建来替代。请勿手动清理这些目录：空闲、归档或待恢复会话仍可能引用它们。移除操作只清理无引用且停止已确认的运行目录，保留包库和命令记录。不承诺强杀/人工删目录之后无损恢复。

## 在页面完成一次安装

1. 打开「插件 → 导入能力包」，选择本地目录或固定 Git 提交，预览清单、执行命令、端点及凭据用途，再选项目导入。请求结果不明时点击「重试原请求」，刷新页面也沿用原命令身份。
2. 配置安装名称、项目、目标机器和普通设置；每台机器选择它自己的凭据引用。保存与准备不会自动启用。检查目标状态，缺解释器、缺凭据或离线会分别报错。
3. 启用新会话。从「使用 Agent 预设」选择预设、机器和 Agent 名称，预览再保存。安装启用到项目后，匹配节点的新 Agent 会话可使用该包；预设的技能/MCP 列表进一步限制它所属包的组合，不扩大项目授权。
4. 更新包时先导入新版本，在安装配置选择该版本，审核清单差异、执行入口、凭据用途和范围后保存。回退同样选择旧摘要。新会话使用新默认版本，已绑定会话保持原版本；原生 `/new` 后才会重新选择。
5. 停用后到「会话引用与移除」检查阻塞项。关闭空闲或归档的原生会话会保留控制台对话记录；活动回合拒绝关闭。节点离线、停止未确认或仍有恢复引用时拒绝移除。

已有 Agent 可勾选显式采用：逐项把其旧技能路径、MCP 名称映射到预设能力。模型、选项、指令、其他能力引用保留；原目录及机器服务不删除。已采用的引用不能从旧 Agent 编辑入口重复加回。旧 Skills/MCP 页上的插件能力为来源导航，修改统一回到插件安装。移除安装保留 Agent 的当前用户配置，解除预设来源，不恢复旧引用。

预设请求字段为 `agent_id`、`node`、`preset`、固定 `digest`，可选 `model`、`options`、`system_prompt` 覆盖。采用请求额外包含 `adopt: {skills: {"/old/path": "package-skill"}, mcp: {"old-server": "package-server"}}`。如果旧 MCP 同时写在 Agent 的 `requires`，先从需求中移除该旧名。升级移除了旧预设引用的能力时，新会话会明确失败，需显式应用兼容预设后重试。

## 作者发布与恢复边界

- 用 `examples/plugins/github` 或 `examples/plugins/team-tools` 作起点；声明已有 harness，不在包内安装新的 ACP 适配器。技能资源放在技能目录中，stdio 服务随包携带固定代码，解释器由目标机器提供。
- 所有发布修改都用新版本。相同包 ID/版本不能对应不同内容。不要在清单、技能文档、归档或普通配置放入凭据值；敏感字段用 `secret` 引用。
- 当前来源是目录快照和固定 Git 提交，没有市场、自动更新、依赖求解或任意安装脚本。远程 MCP 的端点配置可固定，其服务端代码仍由服务提供方更新。
- 会话运行目录在节点 `<state>/plugins/runtimes/<opaque-id>`。其中原生配置、凭据解析结果与 broker token 只留本机。不要直接复制其私有 home 到另一机器。
- 协调切换接回原生会话，不重发已受理 prompt。跨节点恢复在计划中固定原包摘要、普通配置和目标节点的凭据引用；目标准备失败或原包缺失会明确阻塞，不以最新版替代。原生认证和 harness 配置来自目标机器，并在新运行目录中固定。
- 安装目录、进程和网络权限沿用可信作者模型，插件不是进程沙箱。关闭或撤权不能撤销已经发出的外部请求。
