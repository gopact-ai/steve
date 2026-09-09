# 插件第一轮验收记录

2026-09-09，`feat/plugin-packages` / [MR #51](https://github.com/gopact-ai/steve/pull/51)。本记录覆盖隔离环境中的第一轮 Skills + MCP + Agent 预设实现。真实 GitHub/团队服务及真机 fleet 门禁尚未执行，按本会话约定由主会话提供上线前证据。

## 已交付阶段

| 阶段 | 实现与证据 |
|---|---|
| P0 | 严格清单、包/发布/部署/运行身份、项目与节点范围、两个可复用样例；`internal/plugins/manifest_test.go`、配置校验与命名测试 |
| P1 | 本地目录/固定 Git 导入、不可变内容、持久命令、摘要和发布冲突校验；`source_test.go`、`store_test.go`、CLI 测试 |
| P2 | `plugin_packages.v1`、节点凭据版本、完整准备回执、协调任期校验、授权内容复制与恢复；`node/plugins_test.go`、`cluster/peer_plugins_test.go`、`contentreplica/plugins_test.go` |
| P3 | `plugin_runtimes.v1`、每会话隔离 home、固定技能及 MCP 路由、持久运行引用、进程使用/退出证据；本机、普通远端、节点自持会话测试与应用集成 |
| P4 | 页面/API 导入、CAS 配置、准备、版本与范围、预设更新/用户覆盖、显式引用迁移、来源导航、关闭与安全移除；管理单元测试和 `e2e/console/plugins-ui.mjs` |
| P5 | 两个样例同时使用真实 mock ACP 与 HTTP MCP 替身，实际经过节点 broker；完整应用管理、协调切换、原节点丢失、规划与步骤恢复集成 |

## 用户流程与故障证据

- `TestThreePeerCoordinatorTransferResumesOriginalNodeCommandAndExchange`：通过 owner HTTP API 导入两个样例、配置节点密钥引用、采用 Agent 预设；v1 接受 prompt 后升到 v2 并切换协调者，原命令、会话、包版本和回合计费保持不变，实际工具调用不重复。随后新会话用 v2，回退默认后新会话用 v1，已绑定 v2 的会话仍用 v2。
- 同一应用测试在无授权项目运行实际 ACP，会话没有插件技能和工具；恢复到插件项目后按原范围运行。停用后，有空闲引用时删除被拒绝；通过 API 关闭原生会话、确认退出并移除安装，保留四个包版本、用户 Agent 和对话记录。
- `TestSourceNodeLossUsesPlanScopedApprovalAndContinuesInIsolatedWorkspace`：默认版本已为 v2，原节点退出后在恢复计划固定 v1 与目标节点凭据引用；经原有计划级批准，在另一节点独立目录继续相同任务。原工作计费和后续工作目录连续，输出及实际 MCP 请求仍来自 v1。
- `TestPlanStepHandoverUsesOriginalQuestionAndExchange` / `TestPlanningCallHandoverPreservesOriginalRequestAndExecutesItsPlan`：规划调用和计划步骤都持久固定插件运行引用；协调切换保留已受理执行，后续步骤各自固定新版本。步骤执行器显式转发准备接口，避免包装会话接口时遗漏插件绑定。
- `internal/node/plugin_runtime_close_test.go`：真实执行节点的插件关闭 RPC 使用插件鉴权，拒绝过期协调请求，原生进程明确退出后才标记 stopped，响应丢失可重试。
- `internal/plugins/runtime_usage_test.go`：空闲使用、退休状态与退出证据彼此独立；重启保留不明使用；删除目录中断、未完成准备均可重试清理；独立 tombstone 阻止原命令重新创建运行目录。
- `internal/admin/plugin_usage_test.go`：归档会话仍占用旧版本，离线节点和未确认退出阻止移除；从当前范围移除的历史节点仍纳入检查，即使没有收到原生会话回执。
- `internal/admin/plugin_presets_test.go` / `plugin_adoption_test.go`：保留用户模型、选项、指令；拒绝未审核的旧引用、他人预设和过期修订。配置提交与回执保存之间中断可恢复，已保存结果可补完 pending 操作状态。
- 控制台浏览器用例覆盖导入响应丢失后刷新重试、凭据引用、离线/准备与启用的区别、CAS 草稿、预设与逐项采用、旧版本引用和移除、窄屏、深浅主题、焦点和过期读取保护。Skills/MCP 页面显示来源并链接到插件安装，沿用现有设计组件。

## 门禁

本轮反复运行受影响包与 `internal/architecture` 的 `-race -count=1` 测试。实现及嵌入资源 `b6645d5`、升级浏览器断言 `d0c6f18` 已验收：后端完整 race 75 个测试包通过、7 个无测试包；`make test-console` 全部门禁通过；Go build/vet、gofmt 与 diff 检查通过。随后审查补修的最终代码为 `f07079c`：插件、app 与架构包的竞态测试及 build/vet/gofmt 通过；完整后端竞态结果记录在 MR。前端代码没有变化。验证命令如下。

```sh
export PATH=/usr/local/go/bin:$PATH
RATCHET_UPDATE=1 TMPDIR=/tmp go test -count=1 ./internal/architecture/
go test ./internal/architecture/
TMPDIR=/tmp go test -race -count=1 ./cmd/... ./internal/... ./e2e/...
go build ./...
go vet ./internal/... ./cmd/...
gofmt -l .
NODE_OPTIONS=--experimental-strip-types \
LD_LIBRARY_PATH=<Chromium 动态库目录> make test-console
```

本机 Node 22.16 需要上述 TypeScript 类型剥离选项，Chromium 使用环境已有动态库（把 `LD_LIBRARY_PATH` 指向它们所在目录）。未添加依赖或修改系统库。前端 build 重写嵌入资源，Go build/vet 应在它完成后执行。首次并行执行遇到资源文件切换，已改为前端构建后执行 Go 检查。

架构度量保持：非测试函数不超过 120 行；`long_functions.txt`、`interface_assertions.txt`、`state_literals.txt` 均为 0 字节；`cmd/steve` 上限仍为 670 行 / 15 个内部依赖。不把阈值下调到 100。

## 边界和后续

本轮没有更改既有日志文本。审查补修了两项插件输入行为：存储父目录不存在时拒绝创建；`prepare` 缺少 `-command-id`/`-digest`、`show` 缺少 `-digest` 时在 CLI 层指出参数名。对应复现和测试见下表；其他非插件流程沿用原逻辑。新增插件协议和配置字段属于本轮功能；插件会话不会被全局 Skills 刷新重启，普通会话沿用原逻辑。没有 SSH、线上配置编辑、hub/节点重启或生产 e2e 操作。

显式采用的单位是 Agent 的 Skills/MCP **引用**，不是仍被其他 Agent 使用的用户源目录或机器服务定义。删除安装解除预设来源，保留 Agent 当前配置；不会自动恢复已采用的旧引用。共享包库、凭据版本与命令记录保留，没有自动包库/凭据 GC。强制终止留下的暂存目录及短 Unix socket 目录可能需要离线维护，不能按年龄删除运行状态。

真机验收仍需主会话对真实 GitHub/内网服务及 fleet 执行，并在合并前补充证据；此处的服务替身测试不能替代它。插件市场、自动更新、递归依赖、新 harness 安装、自定义前端、hooks、验证器和外部触发器不在第一轮。下一轮先规划验证器契约、提交后事件和自动化受理身份。

## 审查补修

| 审查项 | 复现与修正 | 回归证据 |
|---|---|---|
| 存储父目录契约 | 给合法的 `plugins prepare` 请求指定 `<不存在的父目录>/plugins`，原实现递归创建父目录；现在返回目录不存在，且不创建父目录。显式创建父目录后同一请求可成功。 | `TestPrepareRequiresAnExistingStoreParent` |
| CLI 必填参数提示 | `plugins prepare -source <目录> -store <路径>` 漏掉 `-command-id` 或 `-digest`，以及 `plugins show -store <路径>` 漏掉 `-digest`，原先进入底层泛化错误；现在返回 `plugins <动作> requires <参数>`，不访问源或修改存储。 | `TestPluginCommandsNameMissingRequiredFlagsBeforeAccessingFiles` |
