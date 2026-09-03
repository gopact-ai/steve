# Steve 控制台 v2：按用户意图组织的信息架构（终态设计，第 2 版）

版本：v2（2026-09-03）。第 1 版经 codex 评审 21 条发现后重写。本文写的是终态；实施顺序在末尾单列，不构成设计约束。
架构依据：`arch-v8.md`。本文的原则：**页面只渲染投影，不发明第二权威；每条规则只有一处；数据来源不存在的，先定义来源再画页面。**

## 0. 边界与判据

- 本期是**单 owner 管理面**：读模型 token 即 owner 身份，所有页面动作审计为 owner；同一 token 不得分发给多人。多人 Web 控制台（登录、scope、按项目过滤、真实 actor）不在本期范围，§9 记录其前置条件。
- 桌面 ~1360px；不做手机适配。
- 页面不写配置文件（§7）。金额不算，只算 token 与时间，且只显示提供方**实际报告**的数据与覆盖率。

判据：

| # | 判据 | 怎么验 |
|---|---|---|
| C1 | 六个问题各有一页首屏直接回答：我在哪干活 / 现在在做什么 / 花了多少 / 要我决定什么 / 我有什么 / 发生过什么 | 首屏，不点击 |
| C2 | 动词、可 @ 的 agent、可切的项目都能在输入框里发现；候选与可用性由服务端算 | `/`、`@`、参数补全都来自 `/console/suggest` |
| C3 | 项目主目录、工作方式、当前 agent、谁能接对话常驻可见，切换的后果写在按钮旁 | 上下文条 |
| C4 | 添加机器 / agent / 项目有页面入口，给出经服务端校验的完整步骤与生效方式；不承诺"当场生效"直到 §7 的热加载协议落地 | 资源页 / 项目页的添加面板 |
| C5 | 任何 agent 有几个在跑的 Attempt、每个在做什么；任何任务卡在哪一步、等谁 | 任务页 |
| C6 | 每个任务、agent、模型、天的 token 与耗时可查，缺报的显示"未上报"而不是 0 | 用量标签 |
| C7 | 页面用用户词汇；内部词只在悬停说明与审计层出现；每页首行一句"这页回答什么" | 术语表 §8 |
| C8 | 页面按钮是幂等 Command：重试、双击、多标签页不会重复执行 | `command_id` |
| C9 | 快照区分"确实没有"与"读取失败"；SSE 只提示刷新，不是数据源 | `source_health` |

## 1. 用户心智 ↔ 系统概念

| 用户会说 | 系统概念 | 页面上叫 | 一句话解释（悬停显示） |
|---|---|---|---|
| 一台机器 | node（hub 也是 node，角色 hub） | 机器；hub 机器写作 `<node-name>` | 跑 steve 的一台主机；hub 是负责协调的那台 |
| 机器上装的 AI 工具 | harness | AI 工具（harness） | 一个命令行 AI 程序；按机器申报"已配置且可用 / 已配置但不可用" |
| 一个"人" | agent | Agent | 一个命名执行配置：固定机器和 AI 工具，可选固定偏好模型；本次实际模型以会话报告为准 |
| 一个项目 | project | 项目 | 主机 + 主目录 + 数据等级 + 工作方式（直接修改主目录 / 隔离副本完成后合并） |
| 一段对话 | conversation | 会话 | 绑定一个项目和一个当前 Agent；控制台一个线程 = 一个会话 |
| 我让它做的一件事 | task | 任务 | **一段有目标、预算和完成条件的工作线程**；一条消息是任务里的一个回合。普通消息默认继续当前任务；`/new`、计划、定时触发新建任务；agent 委派的是父任务下的子任务 |
| 拆成好几步、跨机器 | plan | 计划 | `/plan 目标` 拆步骤、按能力放置、每步验证、失败会改计划；计划是一个任务的子结构 |
| 等我拍板的事 | HumanRequest（派生） | 待处理 | 只有 owner 能定的事：允许发送 / 外部动作对账 / 回答问题 / 接入申请 |
| 已经发生的 | ledger Event + 观测 | 历史与审计 | 人话时间线；原始记录在审计层 |

## 2. 数据权威表（§6 的替代）

每种页面数据只有一个权威；读模型只做投影、聚合与派生。

| 数据 | 所属层 | 唯一权威 | 读模型角色 |
|---|---|---|---|
| 机器构建版本、协议版本、主机名、IP、在线 | 物理层 | 当前 advert（连接上的那份） | 展示，附观测时间；断开即标离线 |
| AI 工具可用性、当前模型、可选模型、适配器版本 | 物理观测 | 当前进程的申报与会话报告；`models` 文档只是历史观测缓存 | 显示时区分"当前申报"与"上次观测于 …"，旧观测不冒充当前事实 |
| project、agent、绑定及其版本 | 逻辑 / 绑定层 | ledger 中的版本化名字与绑定 | 投影 |
| 准入（谁能接对话 / 谁能跑步骤） | 协调层规则 | `admit.Decide`（§5）一处 | 只渲染结果与建议动作 |
| 用量、开始 / 结束时间 | 操作层 | `attempt.Record.Result.Usage`，与 Attempt 终态同事务落账 | 聚合；`task.Attempt.Tokens` 与 `StepResult.Usage` 只是非权威缓存，页面不读 |
| Agent 活动 | 派生态 | live Attempt（是否在忙）+ 最新进度观测（在做什么，可丢） | 可重建；重启后只剩 Attempt 时显示"执行中，过程详情暂不可用" |
| 待处理 | 派生集合 | 各 Disclosure / Effect / HumanRequest / Pairing 的 Operation | 合并投影为 HumanRequest，不另存 |
| 历史 | 派生时间线 | ledger Event、workflow run log、持久化的连接性观测 | 人话渲染，cursor 分页 |
| 配置修订 | 逻辑层声明 | ledger 中的 `config_revision`（§7） | 显示应用中的版本与错误 |

## 3. 信息架构

侧栏三组六项，外加常驻状态：

```
工作        工作台        我现在在哪、跟谁说、能做什么
            任务          现在在做什么、卡在哪、花了多少（标签：进行中 / 全部 / 已安排 / 用量）
环境        项目          活在哪里干、谁能接、谁有权
            资源          有哪些机器、AI 工具、Agent，是否健康，怎么加
关注        待处理 [N]    现在具体要我决定什么
底部        历史与审计    发生过什么、谁做的、结果怎样
            系统状态      hub、数据新鲜度、配置版本、reload 错误
```

页面级常驻：顶部紧凑上下文（会话 / 当前项目 / 当前 Agent）；左下系统状态。

## 4. 工作台

### 4.1 上下文条
```
项目 home    项目主机 n251-239-109 · 主目录 ~/.steve/home · restricted · 直接修改主目录
Agent codex  n251-239-109 · codex · 实际 GPT 5.6 Sol（偏好：未固定）· 可用
可接本会话：claude codex grok kimi     不能：builder（项目主目录在 n251-239-109）shipper（…）
```
- 项目下拉：切换即发 `project.use` Command；按钮旁说明："只切换本会话；已有任务不迁移；当前 agent 会话归档并新开；有执行中的回合时不能切换"。
- Agent 下拉 = `/use`（切换当前 Agent）；输入框里 `@agent` = 只指派下一条消息。两者分开写明。
- 运行时（有回合在跑）另显示"本次执行：node-b:/…/worktrees/wt-123"（来自 live Attempt 的 workspace）。
- 数据：`GET /console/context`，**只读**：不创建默认绑定（未绑定时显示默认项目并标"未绑定，第一条消息时绑定"）。可接性来自 `admit.Decide(workload=interactive)`。

### 4.2 补全
`GET /console/suggest?conversation=&q=` 由 CommandRegistry（§5）算：动词（含参数 schema 与说明）、`@agent`（附机器、实际模型、可用性与原因）、`/project use` 的项目、`/tasks` 只列**本会话**的任务、`/repair` 只列可修的、`/approve|/deny|/effects` 只列可处理的。页面不保存规则，只渲染。

### 4.3 过程可见
保留：进行中的回合显示推理 / 工具 / 清单 / 步骤；完成后过程折叠在回复下。回复的过程记录属于会话转录（控制台），不是全局历史。

## 5. 单一规则：CommandRegistry 与 admit

- **CommandRegistry**（`internal/command`）：每个动词一条：解析、i18n key、参数 schema、可用性、补全器、处理器。`protocol.ParseCommand`、`Coordinator.Verbs()`、页面补全全部从它派生；页面按钮发的是同一个 Command。
- **admit.Decide**（`internal/admit`）：
  ```
  Query  { principal, project(revision), agent, workload: interactive|plan_step|verify, requires }
  Result { admissible, ready_now, reason_code, reason_text, suggested_actions[] }
  ```
  合并 roster 可用性、权限（grant / default_role）、等级与 sealed、inplace 主机约束或 isolated 物化条件、能力、槽位。协调器的回合、执行器的放置、上下文条、项目页都调用它。
- **Command 幂等**：`POST /console/send` 带 `command_id`（页面生成）；同一 `command_id` 重复到达返回首次结果；危险动作（取消、effect 重发、强制移除机器）需确认。长操作返回 `accepted` 并用事件流报告进度，不绑死在一个 HTTP 请求上。

## 6. 任务页

- 首屏汇总：执行中 N · 排队 N · 等你 N · 今日用量。
- **看板四列：待执行 / 执行中 / 等你处理 / 已结束**。卡片单位**只有顶层任务**；计划步骤、子任务在卡片内展开；待处理事项只在待处理页持有，卡片只显示"有 2 项待处理 →"。
- 卡片状态由后端算（`WorkItem.Status`），优先级：① 有未解决 HumanRequest → 等你处理 ② 有 live Attempt → 执行中 ③ 已暂停 → 已暂停（在"等你处理"列，标注"你暂停的"）④ 计划可运行但未开始 / 任务 draft → 待执行 ⑤ 终态 → 已结束（done / failed / cancelled 各带徽标）。页面不枚举状态字符串。
- 卡片正面：`#16` 目标 · Agent@机器 · 项目 · 用时/预算 · token（未上报则显示"未上报"）· 此刻在做什么（live Attempt + 最新观测）。
- 抽屉首屏是**结果**：产物、变更文件、验证结论、合并状态（"已合并到项目主目录 / 合并冲突，需要你选择处理方式"）、目标主目录、最后错误、下一步动作（重试 / 继续 / 取消；不提供一键回滚，给出 ref 与 CLI 恢复步骤）。其后：任务树、计划树与修订原因、每步过程、用量、原始事件。
- 标签：**进行中**（默认）/ **全部** / **已安排**（`/every`、`/at`：启用状态、下次触发、上次结果、用哪个项目与 agent、暂停 / 删除）/ **用量**（按天 / agent / 模型；覆盖率；时区按 hub 本地日）。
- 页首说明："任务是一段有目标和预算的工作线程；一条消息是其中一个回合。`/new` 开新任务，`/plan` 拆步骤跨机器，agent 也会派子任务。"

## 7. 项目页、资源页、待处理、历史

### 7.1 项目
当前项目置顶。每个项目：主机与主目录、工作方式、数据等级、访问权限摘要（grant 不是账本细节，是用户管理的权限，在这里管理）、**可接对话的 Agent** 与 **可跑计划步骤的 Agent** 分开列（`admit.Decide` 两种 workload）、活动任务、最近合并。"添加项目 / 停用项目"面板（§7.3）。项目删除只允许 retire，历史、任务、绑定不消失。

### 7.2 资源
- 机器：名字（hub 徽标）· 主机名 / IP · steve 构建版本（`build_version` + commit）· 协议版本 · 系统 / 架构 · git · 等级 · 区域 · 能力 · 在线 · 连接时长；版本不兼容时标出。
- 每台机器下嵌套 AI 工具三类：**已配置且可用**（当前模型 · 可选数 · 适配器版本及观测时间）/ **已配置但不可用**（原因；有同机健康 helper 时给 Repair）/ **可添加**（产品目录，不进入机器状态）。Starter 写入的默认工具是产品默认配置，不是夹具，不隐藏。
- Agent：可用 / 不可用（原因）/ 忙 N/M（live Attempt 数 / 槽位）· 实际模型 · 此刻活动。
- 添加机器 / Agent 面板与配置应用状态（§7.3）。

### 7.3 配置与热加载（终态协议）
- hub 读取并校验整个候选配置，构建差异；全部通过后原子发布 `config_revision` 到 ledger（含应用者、digest、时间）；失败则继续 last-known-good 并在系统状态里显示错误。
- 新增立即启用；删除先 `disabled/draining`：停止新放置，live Attempt 清零后 retire；强制断开机器是独立的高风险动作。项目只 retire 不删除。旧任务固定其 ProjectRevision。
- 热更新范围：`nodes{}`、`agents{}`、`projects{}`、`harnesses{}`；`gateway`、`feishu` 需重启，页面如实说明。
- 页面不写文件：添加面板收集完整字段（机器名、地址、监听端口、等级、区域、工作区、运行方式、平台），生成 `steve config apply …` 命令与那台机器上要跑的命令；应用后显示 `applied_revision` 与 reload 结果。热加载落地前，面板明确写"保存后重启 hub 生效"。

### 7.4 待处理
只放**仍可解决且 owner 有权处理**的 HumanRequest，统一投影：
```
HumanRequest { id, type, source_operation_id, project_id, task_id, summary, choices[], required_role, created_at, expires_at, status, resolvable }
```
按"允许发送 / 外部动作对账 / 回答问题 / 接入申请"分组；按钮文案：披露"批准发送 / 拒绝"；未知 effect"确认已发生 / 重新执行"；重启后内容丢失的披露显示"内容已失效，只能关闭或要求重跑"，不再提供批准。角标 = 可解决项数。

### 7.5 历史与审计
- 数据源：ledger Event（全局按序，cursor 分页）、workflow run log、**持久化的连接性观测**（机器上线 / 离线 / 版本变化、探测结果）。SSE 只提示刷新。
- 默认人话时间线（`event_type + structured_args` 本地化），筛选任务 / 项目 / 机器 / Agent / 操作者，可展开原始记录。"审计"标签：Attempt、租约、副本、见证、原始 Event。
- 推理与工具输入输出不进入全局历史；它们属于会话转录，有自己的等级与保留策略。

## 8. 术语表（页面用词 ↔ 内部词）

工作台 ↔ console；任务 ↔ task；回合 ↔ turn；计划 ↔ plan；子任务 ↔ delegate；资源 ↔ fleet；机器 ↔ node；AI 工具 ↔ harness；Agent ↔ agent；项目主机 / 主目录 ↔ project home；工作方式：直接修改主目录 ↔ inplace，隔离副本完成后合并 ↔ isolated；可用 / 不可用 ↔ ready / blocked；已配置但不可用 ↔ missing；已合并到项目主目录 ↔ landed；待处理 ↔ HumanRequest；历史与审计 ↔ events。建议动作一律来自结构化 `suggested_actions`，不拼字符串。

## 9. 本期不做、但必须写明的边界

- 多人使用（登录、scope、按项目过滤、真实 actor、SSE 授权、CSRF、撤权、token 一次性交换）。
- awaiting-human 的问答通道、pairing 的控制台动作：先定义 HumanRequest 与其回答 Command，再进待处理页。
- 首次启动引导（无项目、无可用 agent）、大规模搜索与虚拟滚动、断网 / 陈旧快照提示、多标签并发提示、刷新恢复、时区与无障碍、数据保留与脱敏、通知策略。每条在实施时以独立条目验收。

## 10. 实施顺序（不构成设计约束）

1. 投影模型冻结：`WorkItem`、`ProjectSummary`、`AgentActivity`、`HumanRequest`、`UsageAggregate`、`HistoryEntry`、`SourceHealth`；快照补齐字段与 `source_health`。
2. `admit.Decide` 与 CommandRegistry；`/console/context` 只读；`/console/suggest`；`command_id` 幂等。
3. 用量落 `attempt.Record.Result.Usage`；读模型从 Attempt 聚合；覆盖率。
4. 任务页（看板 + 抽屉 + 已安排 + 用量）、项目页、资源页三类工具与版本、待处理投影、历史（ledger Event + 连接性观测）。
5. 术语与文案全面切换；页首说明；系统状态。
6. 配置修订与热加载协议；添加面板生成 `steve config apply`。

## 11. 实施记录（2026-09-03）与偏差

两轮 codex（gpt-5.6-sol，reasoning max）评审：第 1 轮 21 条、第 2 轮 13 条新发现。结论：§3、§4、§7 的页面组织可作为 IA 基线；数据契约层面有 6 项必须先改（权威表拆分、四轴状态、HumanRequest 真实来源、admit 拆分、统一 AuditEntry、实施顺序）。本轮落地的与保留的：

已落地：
- 六页 IA（工作台 / 任务 / 项目 / 资源 / 待处理 / 历史与审计）与系统状态；术语切换。
- 上下文条与服务端补全（`/console/context` 只读、`/console/suggest`）；`command_id` 幂等。
- 任务四轴投影 `lifecycle / execution / attention / lane`，自后代任务向上汇总；没有 "queued"。
- 用量写在 ledger Attempt 的终态转换里（成功与失败都记）；页面只显示提供方实际报告的量，ACP 适配器目前只报上下文占用，页面如实标注。
- 待处理只投影有真实来源的两类：披露（批准 / 拒绝）、结果未知的对外动作（确认已发生 / 允许再次调用）。
- 历史：ledger 全局事件按 seq 分页 + 持久化的机器上线 / 离线观测；审计标签放原始记录。
- 申报带 `build_version`、hostname、IP；适配器版本随模型观测记录。

有意保留（按评审意见，先定义契约再做）：
- `admit.Decide` 的完整拆分（只读评估 / 原子准入）与 `queueable`：当前可接性 = roster 可用 ∧ `project.NotHome`，是评估而非最终裁决，最终裁决仍是 Attempt 租约 CAS。
- CommandRegistry：动词目录与补全已在协调层，解析仍在 `protocol`；三者尚未合并为一个注册表。
- 统一 AuditEntry 与复合 cursor：历史目前只覆盖 ledger Operation Event 与连接性观测，不含文档更新、绑定变更、命令记录、效果日志。
- 配置修订 / 热加载：添加面板生成片段与命令，明确写"重启 hub 生效"。
- 回答 agent 提问、飞书接入申请：待处理页不显示，页面如实说明。
- 多人使用与 token 会话化。

## 12. 工作台 v3（2026-09-03，向 Codex app / Multica 对齐）

用户反馈：没有新建会话的入口；agent 之间的调用关系看不出来；聊天框要学 Codex app，整体学 Multica。

- **三栏**：左栏是会话列表——顶部"新会话"，其下按项目分组、最近优先，每条显示标题（第一句非动词输入）、Agent、最近时间，正在跑的带转圈；数据来自新接口 `GET /console/conversations`（`console.Service.Summaries`：标题、项目、Agent、最近时间、条数、是否在跑）。新会话在前端生成 `console:<base36 时间>`，第一句发出才在服务端出现。当前会话记在 sessionStorage。
- **composer**：贴底的一张卡，输入框随内容长到 6 行；卡的下沿放项目与 Agent 两个选择（= `/project use` 与 `/use`），让上下文在第一句之前就可见（Codex app 的 issue 里正是这个抱怨）；进行中显示"停止"（发 `/cancel`）；顶栏显示会话标题与项目 / Agent 徽章。
- **关系页签**：右栏第三个页签画本会话的调用树——任务 → 计划步骤（哪个 agent 在哪台机器、状态、依赖 / 汇合）→ 委派出去的子任务（递归，"X 委派 →"），进行中的转圈。组件 `lib/tree.tsx` 的 `CallGraph`，任务看板抽屉里复用（替换原来只列子任务名的"子任务"节）。数据全部来自已有投影（Task.parent/children/member/node，Plan.steps.agent/node），没有新增后端字段。
- **顺手修的线上崩溃**：`roster.addModels` / `markFunctional` 浅拷贝快照但共用 `Coverage` map，多个请求并发 `All()` 时 fatal "concurrent map writes"，hub 进程直接退出。已深拷贝并加 16 协程并发读的 race 测试。
- 仍然明显的问题（不属于本次）：聊天类任务永远停在"进行中"（协作审计 3-5），关系页签里一眼就能看到。
- **composer 第二版（用户对照 Codex app 截图后）**：去掉大按钮与带边框下拉。底部一行全是文字级控件：左 "+"（动词菜单，来自 `/console/verbs`）、项目（文件夹图标 + 名字 + 机器，菜单切换）、右侧 Agent（名字 + 模型灰字，菜单里带机器 / 工具 / 模型，不可用的置灰并写原因）、圆形箭头发送键（进行中变成方块"停止"）。消息不再用彩色气泡：用户消息是浅灰圆角块靠右，回复是正文加一行小字时间与"过程"链接。左栏"新会话"是带图标的普通行，选中项只有浅底。
- **资源页机器区（用户指出表格混乱后）**：不再是表。一台机器一张卡：头部是名称、角色、在线状态、主机名 / 拨号地址 / 首个 IP（其余折叠进提示）；右侧小定义表分开写版本、系统、数据等级（带解释：公开 < 内部 < 受限 < 密封，项目等级高于机器的活不放这里）、健康（磁盘空闲 · 负载 · 工作树）；租约区域只有在有机器配了区域时才出现。正文按"AI 工具 / 命令 / 硬件 / MCP / 技能 / 声明"分块，声明块把网络、凭据、标签合成一块并标出类别。Agent 表的"等级"改名"数据等级"。

