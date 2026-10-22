# 部署与运维

Steve 可作为单机服务，也可作为持有复制账本的集群节点运行。桌面安装使用集群格式；独立服务适合明确管理配置文件的部署。使用者入口见[入门](getting-started.md)，字段和默认值见[配置参考](configuration.md)。

## 独立服务

准备 Git、可用的 agent 登录和 ACP 适配器运行环境。源码构建使用 Go 1.27+、Node.js 与 npm：

```sh
make console
make build
cp config.console.example.json config.json
chmod 600 config.json
```

填写 `gateway.owner_id` 和真实项目目录，保留需要的 agent/harness。内置适配器首次下载需要 npm registry，之后使用已校验的本地缓存。

```sh
./steve doctor -config /srv/steve/config.json
./steve run -config /srv/steve/config.json
```

将 `run` 作为前台进程交给服务管理器，使用拥有该状态目录和 agent 登录的用户。相同状态目录只允许运行一个 Hub。日常检查使用控制台、`steve top -config ...` 或已认证的 `/state`；`doctor` 会准备状态并启动工具探针，不是在线只读检查。

## 服务端集群初始化

创建新安装：

```sh
steve peer-init --state-dir /home/me/steve-service
steve peer --config /home/me/steve-service/config.json
```

将路径替换为部署用户自己的目录。初始化只创建文件，不启动服务；输出配置、身份、凭据文件和地址文件的路径，不打印凭据值。新目录必须不存在或是权限为 `0700` 的空目录；重复初始化核对并保留原身份。

生成的 `config.json` 与 `config.json.cluster.json` 属于同一安装。保留身份、状态路径和控制台配置，通过控制台管理共享声明。完整节点保存私人协作账本，数据等级至少为 `restricted`；共享账本不支持 `sealed` 项目。

监听端口首次选取后持久保存，控制台绑定 loopback。实际地址见 `endpoint_file` 或日志 `UI available at`。初始化不启用自动容灾。执行节点的 owner 绑定使用输出中的 **cluster ID**。

### 服务模式的集群初始化

已有独立服务先停止并备份完整状态和配置，然后执行：

```sh
steve peer-init -config /srv/steve/config.json -storage-level restricted
steve run -config /srv/steve/config.json
```

此模式保留原 Hub ID、owner、项目、账本和凭据，并生成集群 sidecar；`run` 检测到 sidecar 后运行集群应用。控制台必须使用 loopback，token 至少 32 字符且不含空白。需要调整监听时传入 `-ui-address 127.0.0.1:7710`。

初始 Raft 与 peer 地址为 loopback。加入其他完整节点需要互通地址或桌面的 SSH 接入，可用 `-raft-address` 和 `-peer-address` 指定初始化地址。重试初始化保留原身份；已有 Raft 数据时不能删除 sidecar 重新生成身份，也不能通过移走 sidecar 退回独立运行。

## 控制台与凭据

```sh
steve dash -config /srv/steve/config.json
```

`dash` 打印带 token 的访问地址。独立 Hub 默认 `127.0.0.1:7710`；未配置 token 时，只在 loopback 模式生成状态目录中的 `loopback-token`。非 loopback 监听必须配置 `gateway.read_model_token`。

远程访问可通过 SSH 本地转发连接实际控制台端口。浏览器入口验证 Host 和写请求来源；反向代理到 loopback 时需要将 Host、Origin 与上游地址正确对应，并在代理侧保留认证与来源校验。对外监听使用 TLS 入口并妥善保存 owner token。Steve 的本地 token 不等于外部 SSO 账号体系。

服务配置页区分保存值和当前运行值。需要重启的设置由节点与服务页显示；确认后可重启空闲服务。重连后检查版本和生效配置。

## 飞书 / Lark

`steve setup` 录入已有应用，`steve setup -create-app` 使用官方设备流。配置成对的 `app_id`、`app_secret` 与该应用下的 owner `open_id`。无应用凭据时控制台独立运行。

Channel 设置管理启停、默认通道、群聊策略、允许及阻止名单。凭据测试成功表示可连接，不代表所有群聊或项目都已授权。控制台可以查看已记录的 Channel 会话；外部消息投递仍以各通道的回执为准。

## 部署 node

目标机器需要 Git、已认证的 harness 和匹配系统/架构的 `steve-node`。配置参考 [node.example.json](../node.example.json)。

```sh
steve-node -config /srv/steve/node.json
```

在协调端资源页登记机器地址和 token，创建绑定该机器的 agent。`nodes` 只声明连接，模型、工具和技能由节点实际报告。独立 Hub 使用稳定 hub ID 绑定 owner，集群使用 cluster ID。

### 在控制台添加机器

资源页的引导命令在目标机器运行。下载用的 Hub URL 必须从目标可达；浏览器使用的 `127.0.0.1` 不能直接当远端下载地址。

引导脚本只复制 harness 的 `command` / `args`，其他环境、凭据、模型、技能和 MCP 需在节点准备。已有可执行的 `steve-node` 不会被引导脚本更新。桌面 SSH 接入的完整节点见[桌面指南](desktop.md#接入机器)。

单个 node 实例只归属一个协调身份；多实例需要独立端口、状态目录和运行环境。移交前停止实例并确认旧 agent 进程退出，再执行：

```sh
steve-node adopt -config /srv/steve/node.json \
  -evidence "已核实旧实例及其 agent 进程停止" hub-b
```

`adopt` 更新归属记录，不修改认证映射或迁移项目。先配置目标 owner 的认证再启动。

### 节点协议版本

协调端与执行端使用节点协议 v2，不兼容的连接在握手时拒绝。资源页显示双方版本与需升级的一方。手工部署需要替换匹配架构的二进制并重启；桌面 SSH 接入的机器可使用其升级入口。

节点软件版本不同不必然表示协议不兼容。原生历史、服务重启等可选能力仍以节点报告为准。

### MCP 部署

简单部署在 `node.json` 的 `mcp_servers` 配置 stdio、HTTP 或 SSE 服务。Agent 引用目标节点的服务名称，远端服务定义和敏感值由节点持有。

独立 broker 使用 `steve-node mcp-broker -config mcp.json`，节点仅配置 `mcp_broker` 的 socket/token，并清空 `mcp_servers`。两种方式互斥。Agent 获得绑定的本机地址或启动器，平台 MCP 通过认证回程连接访问协调端。

### 连接中断与恢复

执行节点持有 agent 进程和输入/输出回执。连接恢复后按原身份与序号续接，默认进程流宽限为 10 分钟。宽限结束、节点进程丢失或日志不可用均可能阻止续接。

无法确认停止的执行保持隔离，防止另一执行同时写同一目录。不要通过删除状态、重复发送 prompt 或缩短租约冒充恢复。使用原任务的恢复入口核对；详细操作见[中断与恢复](desktop.md#中断与恢复)。

## 插件安装与会话版本

控制台管理包导入、节点配置、启用和会话引用。跨节点准备需要已提交的集群协调者；独立 `steve run` 只能管理自身的插件部署。节点凭据用 `steve plugins secret-put` 从 stdin 写入，协调端选择版本引用。

安装保存、节点准备和新会话启用分别显示。升级影响新会话，旧会话保持原包摘要和运行目录；缺失旧版本会阻止恢复，不自动换成最新版。参见[插件指南](plugins.md)与[作者参考](plugins-local.md)。

## 备份与迁移

正常关闭服务并确认执行停止后，备份配置、集群 sidecar、完整状态目录、项目主目录和节点私有状态。凭据与原生 agent 登录也需要由所属机器的备份策略覆盖。账本副本不是所有项目文件的备份。

跨独立实例迁移使用[项目迁移](project-migration.md)。从账本备份恢复使用 `steve ledger status`、`rotate`、`recover` 和 `effects`，命令参数见 [ledger CLI](../cmd/steve/ledger.go)。Rotate/recover 会影响租约，应在停服维护中使用；普通断线不需要它们。

## 排障

| 状态 | 核查与处理 |
| --- | --- |
| Agent 显示可用，执行却要求登录 | 在执行机器的部署用户环境核对认证和隔离 home；检测到程序不证明模型登录有效。 |
| 工具缺失或启动失败 | 核对目标机器上的绝对路径、解释器、PATH 和代理配置。 |
| 提示能力或身份文件变化 | 新建原生会话，让技能、MCP 和插件按当前配置装配。 |
| 节点显示协议不兼容 | 升级提示指明的一端，并检查实际运行进程版本。 |
| 节点属于另一个 Hub | 核对稳定 owner 身份；需要移交时使用停服后的 adopt，不删除归属记录。 |
| 子任务完成，文件尚未出现 | 查看落地记录：目录可能正忙、待恢复或存在合并冲突。 |
| 停止或外部动作结果不明 | 保留原任务/操作 ID，核对原执行或外部回执；重试观察不等于重做动作。 |
| 插件准备失败 | 查看目标节点的解释器、平台、凭据引用及协调者授权。 |
| 配置保存冲突 | 读取最新修订并合并草稿；不要覆盖外部修改。 |
| 共享账本无法写入 | 检查完整节点及多数派；离线不会自动缩小投票成员数。 |
| 影子仓库初始化或锁异常 | 停止使用该目录的进程后检查完整性；保留未知锁与数据，不直接删除重建。 |
| 服务启动失败 | 核对私有目录权限、单实例锁、监听地址和端口，以及日志中的首个错误。 |

保留问题对应的任务、attempt、操作 ID 与日志，避免附带 token、邮件正文或私有配置。服务接口只返回受理时，要继续读取原记录确认执行和外部结果。
