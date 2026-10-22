# 插件作者与 API 参考

能力包使用 `steve.plugins.v1` 清单，支持本地目录快照和固定 Git 提交。使用者的安装、升级和会话行为见[能力包](plugins.md)。

## 预览和准备

在仓库根目录使用构建后的 CLI：

```sh
./steve plugins preview -source examples/plugins/github
./steve plugins preview -source plugins/steve-dev
```

预览返回 `manifest`、`digest` 和解析后的 `source`。将实际摘要填入准备命令：

```sh
./steve plugins prepare -source examples/plugins/github \
  -store /tmp/steve-plugins -command-id github-001 -digest <preview-digest>
./steve plugins list -store /tmp/steve-plugins
./steve plugins show -store /tmp/steve-plugins -digest <preview-digest>
```

存储父目录须存在。CLI 不加载 Hub 配置或创建模型会话；`prepared` 只表示本地内容及回执已保存，不表示项目已启用。相同命令 ID、来源和摘要重试返回原回执，内容变化则拒绝。

Git 来源使用绝对本地仓库路径或无凭据 HTTPS URL，并指定完整小写 40/64 位 commit ID 与可选 `-subdir`。分支、标签、SSH helper、符号链接和 submodule 不被接受。私有源码可以先通过授权方式获取到本机，再读取固定提交。

## 清单

包根目录放置 `plugin.json`。完整样例见 [GitHub](../examples/plugins/github/plugin.json) 和 [team-tools](../examples/plugins/team-tools/plugin.json)。

| 字段 | 契约 |
| --- | --- |
| `schema` / `api` | `1` / `steve.plugins.v1` |
| `id` | `namespace/name`；每段 1–64 个小写字母、数字、点、下划线或连字符，以字母或数字开头 |
| `version` | 固定 `major.minor.patch`，可带小写预发布标识；无范围、latest、前导零或 build metadata |
| `description` | 非空说明 |
| `platforms` | 可选 linux/darwin 与 amd64/arm64 组合 |
| `settings` | 普通或 secret 设置声明；secret 无默认值 |
| `skills` | 包内名称到包含 `SKILL.md` 的相对目录 |
| `mcp` | 包内名称到固定服务定义 |
| `agents` | harness、模型偏好、选项、指令和包内能力引用 |

字段名严格区分大小写。未知字段、重复 JSON 键和多个顶层值会被拒绝。预设只保存声明，应用由控制台明确执行。

MCP 的 HTTP/SSE 定义包含 URL 和可选 headers；URL 不含用户凭据或 fragment。stdio 定义使用包内 `program.path`，可执行文件或已有 `node` / `python3` 解释器，加参数数组和环境映射，无自由 shell 或安装钩子。

值使用 `text`、`config` 或 `secret` 引用；类型必须与 settings 一致，`prefix` 可添加如 `Bearer ` 的前缀。远端端点配置固定不代表远端实现代码固定。

## 内容约束

包只包含普通文件，排除根 `.git`。隐藏项、符号链接、设备、路径逃逸、大小写或 Unicode 规范化冲突均被拒绝。清单规范化，文件按名称排序，可执行位保留；时间与普通权限差异不影响摘要。

| 限制 | 上限 |
| --- | --- |
| 清单 | 128 KiB |
| 单文件 / 整个归档 | 8 MiB / 32 MiB |
| 文件与目录总数 | 4096 |
| 路径 | 1024 字节，最多 32 个分隔符 |

存储包含 `packages/<sha256>/bundle.tar`、`content/`、`requests/` 和 `receipts/`。Unix 文件锁串行发布，内容先写临时目录再原子发布。相同请求可恢复未完成的回执；损坏内容被拒绝，不在原身份下静默修复。

## 节点本地凭据

```sh
./steve plugins secret-put -store /path/to/node-state/plugins -name github-token
./steve plugins secret-list -store /path/to/node-state/plugins
```

`secret-put` 从 stdin 读至 EOF，接受最多 16 KiB UTF-8，去除末尾一次换行，仅输出版本引用。每次写入生成新修订，旧修订供原部署使用。不要把值放入命令参数、清单或普通设置。

协调端使用 `{name, revision}` 选择目标节点凭据，无值读取接口。节点准备校验项目、摘要、配置、平台和解释器；原生会话按固定运行集合启动。

## 部署到节点需要协调者

跨节点插件操作需要已提交的集群协调身份。独立 `steve run` 能导入并管理自身部署，不能准备远端插件。新服务使用 `steve peer-init --state-dir ...`，已有服务按[集群初始化](operations.md#服务模式的集群初始化)保留状态转换；桌面集群使用现有接入入口。

## 管理 API

以下接口要求 owner 认证：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /console/plugins` | 包、安装、节点状态和操作 |
| `POST /console/plugins/preview` | 解析来源并返回清单、摘要 |
| `POST /console/plugins/import` | 用 `command_id`、`project`、`digest`、`source` 导入 |
| `PUT /console/plugins/installations/{id}` | 用 `base_revision` 和 `installation` 保存声明 |
| `POST /console/plugins/installations/{id}/prepare` | 准备或重试目标节点 |
| `GET /console/plugins/nodes/{node}/secrets` | 节点凭据引用 |
| `POST /console/plugins/installations/{id}/presets/preview` | Agent 预设差异 |
| `POST /console/plugins/installations/{id}/presets/apply` | 用稳定 `command_id` 与 Agent `base_revision` 应用 |
| `GET /console/plugins/installations/{id}/usage` | 会话、运行引用与旧版本 |
| `POST /console/plugins/installations/{id}/runtimes/{runtime}/close` | 关闭空闲原生会话，确认退出后释放引用 |
| `DELETE /console/plugins/installations/{id}` | 用 `base_revision` 移除已停用且无引用的安装 |

安装声明包含 `package_id`、`digest`、`enabled`、`projects`、`targets`。目标配置使用普通 `values` 和版本化 `secrets`。包必须已导入到授权项目；保存不等于节点准备完成。

预设请求包含 `agent_id`、`node`、`preset`、固定 `digest`，可覆盖 `model`、`options`、`system_prompt`。显式采用可增加 `adopt.skills` 和 `adopt.mcp` 的旧引用到包内名称映射。旧 MCP 若也在 Agent `requires` 中，先移除该旧需求。

请求超时后保留原命令 ID 查询或重试。配置修订冲突需读取新版本再合并。能力引用不兼容时应用兼容预设；不能靠跳过校验启动。

## 发布与运行状态

发布修改使用新版本。包不提供市场、自动更新、依赖求解或任意安装脚本。执行依赖由目标机器提供。

运行目录位于节点 `<state>/plugins/runtimes/<opaque-id>`，保存原生状态、固定能力和本地 token。协调切换核对原会话，跨节点恢复固定原包与目标凭据引用。移除只回收无引用且已停止的目录；不要直接复制私有 home 或删除恢复记录。
