# 本地能力包准备

已实现的范围是读取、校验和准备能力包。`prepared` 表示固定内容及本地回执已保存，不表示已安装执行依赖、已配置凭据、节点可用或会话已启用。节点准备与配置校验的底层协议已接入，项目管理入口、版本化会话与管理页继续按 [插件规划](plugins.md) 推进。

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
| `agents` | Agent 预设：harness、模型偏好、options、system prompt、包内技能/MCP 引用；此阶段只保存预设，不创建 Agent |

字段名大小写严格匹配，拒绝未知字段、任意层级的重复对象键与多个顶层 JSON 值；不通过忽略字段实现未来能力的静默降级。

MCP 支持两种声明：

- `http` / `sse`：URL、可选 headers。URL 可以是普通配置引用，不能带用户凭据或 fragment；header 名使用 HTTP 规范形式，例如 `Authorization`。保存远端配置不等于固定远端服务实现。
- `stdio`：`program.path` 指向包内实际文件；可直接执行已标记可执行的文件，或声明现有 `node` / `python3` 解释器。`args` 是参数数组，`env` 是环境映射。无 URL、headers、安装钩子或自由 shell 命令入口。解释器及环境依赖的可用性留给节点准备阶段验证。

URL、header、参数和环境值使用同一个引用结构：`text` 为字面量，`config` 引用普通设置，`secret` 引用节点凭据声明，二者必须匹配 `settings` 中的类型。`prefix` 仅用于引用值前缀，例如 `Bearer `。当前命令只校验和保存引用，不解析凭据或调用远端服务。

逻辑能力身份由包 ID、能力种类和包内名称组成；原生名称由该身份派生。最终节点装配仍需检查与既有用户资源的命名冲突。

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

这里的不可变指安装 API 不原地更新已保存的包，并在使用时核验内容；包存储由本机用户管理，不是对同 UID 恶意进程的防篡改沙箱。当前不提供激活、升级生效、停用或删除命令；这些动作需要持久会话引用和节点状态，不能仅凭本地目录判断。

## 节点本地凭据

在执行节点本机配置凭据，不把值经过 hub。`secret-put` 从 stdin 读取一个最多 16 KiB 的 UTF-8 值，只输出随机版本引用和创建时间；末尾的一次换行会去掉。同名写入创建新版本，保留旧版本供旧部署引用：

```sh
./steve plugins secret-put -store /path/to/node-state/plugins -name github-token
./steve plugins secret-list -store /path/to/node-state/plugins
```

第一条命令从 stdin 读取至 EOF。不要把值写在命令行参数里。得到的 `{name, revision}` 可以用于节点部署配置；协调端只读取引用和可用性，不能查询凭据值。包清单的 `settings` 声明仍决定它是普通配置还是 secret，不能用普通配置字段绕过类型检查。

节点的 `plugin_packages.v1` 协议目前提供 prepare、inspect 和凭据元数据查询；它校验部署的项目范围、目标节点、包摘要、普通配置、凭据版本和平台/解释器需求。内部 `Selection` 固定会话将要用的部署集合，尚未接入实际会话。`plugins.Library` 可把包内容按项目复制到独立节点，并从同一账本记录恢复；本地 CLI 的独立缓存不自动加入该库。
