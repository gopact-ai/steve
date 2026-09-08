# Go 后端重构计划

本文是后端代码组织的重构计划：目标是让已经存在的设计（账本提交点、attempt 状态机、租约与回执）在代码形态上可见，去掉堆砌出来的重复与长函数，让后续维护有章可循。设计不变量见 [architecture.md](architecture.md)，本文不改变它们。

计划按终态写；末尾的实施顺序只是顺序，不构成设计约束。

## 现状（2026-09-08，master `f0942df`）

| 指标 | 数值 |
|---|---|
| 非测试 Go 代码 / 测试代码 | 99.5k / 64.3k 行，75 个包 |
| 超过 100 / 200 行的函数 | 67 / 12 个（最长 `serveApplication` 893 行） |
| `cmd/steve` | 11.4k 行，引用 64 个内部包；组合根里有业务逻辑 |
| 回合生命周期实现 | 5 份：`turn.prompt`、`turn.RelocateChat`、`turn.resumeRetainedChat`、`delegate.run`、`agentexec.Runner.Prompt` |
| 内联比较的字符串状态/动作 | 28 处（`"open"` / `"running"` / `"done"` …） |
| 临时接口断言 `x.(interface{ … })` | 9 处 |
| `_ =` 吞掉的错误 | 390 处 |
| `log.Printf` | 294 处，无结构化字段 |
| 未被引用的包 | `internal/sessions` |

问题不是缺少设计，而是设计停在了不变量层：概念正确，但每个概念都以过程式长函数的形态存在，模型没有落成类型。五份生命周期各自手写心跳、错误分支和清理顺序，是同一类 bug 反复出现的根源（例：取消后用已取消的 ctx 释放租约）。

## 原则

1. **行为不变**。每一步都是重构，不夹带功能；现有 64k 行测试是安全网，改动前先让它们覆盖要动的路径。
2. **可度量**。每个里程碑有进入前后的数字；`internal/architecture` 里的棘轮测试保证数字只能变好。
3. **小步合并**。每个里程碑一个或几个 PR，CI 绿 + `make e2e-fleet`（涉及回合/委派/落地时）+ 真机部署后合入。
4. **不引入框架**。用 Go 的类型、接口和包边界表达设计，不上 DI 容器、代码生成或反射。
5. **领域包不动语义**。`ledger` / `attempt` / `task` / `project` / `artifact` 的提交点和不变量原样保留，重构的是它们的调用方。

## 终态

### 执行生命周期只有一份

`internal/execution` 新增 `Run`：一次执行从开 attempt 到关 attempt 的完整顺序与错误规则。

```
Open(spec) → Admit → Prepare(workspace, base) → OpenSession → Arm(running)
  → Drive(prompt, progress) → Settle → Close(finish | fail | unsettled)
```

第一片已落地为 `internal/lifecycle`：`Keep`（租约心跳与丢失取消）、`Drive`（发 prompt、分清已结束与未确认停止）、`Usage`（最后一次报告换算成用量）、`Close`（带停止证据的关会话）、`Cleanup`（脱离取消的有界清理 ctx）；五个调用方已改用它们，各自的编排顺序仍在原处，是下一片要收敛的对象。

调用方只提供三件事：**怎么拿工作区**（主目录 / 副本 / 隔离工作树）、**怎么驱动**（prompt 文本与进度回调）、**结束后做什么**（聊天回合：after-snapshot + 回复；委派：发布产物 + 投递；规划/验证：校验输出）。心跳、租约丢失取消、`ErrStopUnconfirmed` 隔离、settle、脱离取消的清理 ctx、超时常量，全部只在 `Run` 里出现一次。

五个调用方收敛为 `Run` 的薄包装：`turn.prompt`、`turn.RelocateChat`、`turn.resumeRetainedChat`、`delegate.run`、`agentexec.Runner.Prompt`。接回原执行（resume / relocate）是 `Run` 的另一个入口 `Reattach`，不是另一份实现。

### 组合根只做装配

`cmd/steve` 只剩命令行解析和 `main`：

| 现在 | 终态 |
|---|---|
| `main.go` 的 `serveApplication`（893 行） | `internal/app.Build(cfg) (*App, error)` + `App.Run(ctx)`；每个子系统一个装配函数（账本、机群、控制台、通道、委派、清扫……），子系统间只传接口 |
| `admin_*.go`（约 4k 行，控制台管理用例） | `internal/admin`：应用服务，实现 `consoleapi.Admin` 等端口 |
| `cluster_*.go`（约 4k 行，桌面多机） | `internal/cluster`：成员、接入、内容修复、SSH 安装 |
| `application_*.go` | 归入 `internal/app` |

目标：`cmd/steve` 非测试代码 ≤ 2k 行、引用的内部包 ≤ 15 个。

### 状态与动作是类型

- `nodewire.SessionAction` 常量与 `SessionService` 的按动作分发表，替换 `Do` 里的字符串 switch。
- 任务、交换、会话状态在各自包里是带方法的类型（`attempt.State` 已是），跨包比较不再写字面量。
- `switch` 覆盖所有值，`default` 返回错误而不是静默。

### 端口显式

- 9 处 `x.(interface{ … })` 改为在消费方声明的接口，装配时静态满足。
- `turn.Coordinator`（31 个内部包 fan-out）按职责拆：回合执行、命令处理（`/project` `/use` `/skills` `/plan` …）、上下文装配、恢复；`node.Server`（1.5k 行）拆为传输、申报、会话、MCP 四块。

### 可观测性

- `log/slog`，每条日志带 `attempt` / `task` / `conversation` / `node` 字段；`turn: timing` 成为结构化事件。
- 排障表（[operations.md](operations.md#排障)）里列出的日志消息保持文本稳定，字段是补充不是替换。

### 错误与工具函数

- `_ =` 只允许出现在有注释说明"为什么可以忽略"的清理路径；其余记日志或上抛。
- 跨包重复的 `clip` / `truncate` / `firstLine` / `contains` 收进一个 `internal/text`。

## 护栏：`internal/architecture` 棘轮

- **函数长度**：非测试函数 > 150 行的清单写在基线文件里，测试断言当前集合 ⊆ 基线；每去掉一个就从基线删掉。阈值随里程碑下调（150 → 120 → 100）。
- **包体量**：`cmd/steve` 非测试行数与内部包 fan-out 只能下降。
- **临时接口断言**与**内联字符串状态**：同样的基线棘轮。
- 现有的领域/传输依赖方向测试保留。

## 已知抖动的测试

CI 的 GitHub 托管 runner 负载不稳时，以下时序敏感测试会偶发失败（本地与多数 CI 运行通过），属于 M6 要处理的清理项：把"租约 TTL 与续期间隔"的比值放大、或改用可控时钟，而不是靠重跑。

- `internal/artifact` `TestLandingDriverRenewsAndRejectsStaleTransitions`（"landing driver expired during work"）
- `internal/exec` `TestRunDriverRenewsAndFencesAReplacedOwner`（"driver lease expired"）
- `cmd/steve-node` `TestNodeCommandReexecutesAndKeepsDurableCommandIdentity`（"node is busy; release idle sessions before restarting"）

## 实施顺序（不构成设计约束）

| 步 | 内容 | 验收 | 估计 |
|---|---|---|---|
| M0 | 棘轮测试与基线；删除 `internal/sessions` | CI 绿；基线文件入库 | 0.5 天 |
| M1 | `execution.Run`：先从 `agentexec` 抽出，再依次迁 `delegate.run`、`turn.prompt`、relocate / resume | 每迁一个：`go test -race`、`make e2e-fleet`、真机部署；五处 `attempts.Open` 调用点归一 | 2–3 天 |
| M2 | `internal/app` / `internal/admin` / `internal/cluster`，`cmd/steve` 瘦身 | `cmd/steve` ≤ 2k 行；桌面集群测试全过 | 1–2 天 |
| M3 | 类型化动作与状态（可与 M1 并行，改的是 `node` / `nodewire` / `console`） | 内联字面量为 0 | 1 天 |
| M4 | `slog` 与 trace 字段 | 排障表消息不变；`turn: timing` 结构化 | 1 天 |
| M5 | 端口显式；拆 `turn.Coordinator` 与 `node.Server` | 临时断言为 0；两包最大文件 < 600 行 | 2 天 |
| M6 | `_ =` 清理；`internal/text` | `_ =` 只剩带注释的清理路径 | 1 天 |

M1 是收益最大的一步，也是唯一改动执行路径的一步，所以放在最前并单独验收；M2–M6 主要是搬运和替换，可以分给并行的执行者。

M3 实施结果：`state_literals.txt` 从实施前的 53 条记录降到 0 条（上方现状表保留早期统计）。会话动作、会话及命令状态、交换状态和管理操作状态使用各自的类型；任务状态与结果复用 `task.State` / `task.Outcome`。动作分发保留原有授权和回执处理顺序，`start` 仍仅用于 open 内部的二次授权。JSON 字符串值及各领域原有的终态判定保持不变。
