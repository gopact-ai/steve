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
- **资源页第三版（用户指出卡片更难读、IP 与端口混在一起、添加机器还是 JSON 且关不掉）**：机器回到一行一台的表——名称（含角色）、主机 / IP（主机名一行、首个 IP 一行，其余折叠）、版本、系统、数据等级、健康、状态；点一行开右侧抽屉：主机名、全部 IP、hub 拨号地址、版本、系统、数据等级（含 key）、租约区域（有才显示）、健康、连接时间，以及按块的"能做什么"。**添加机器 / Agent 成为真动作**：对话框（可关闭）填名称、拨号地址、数据等级 → `POST /console/nodes`：hub 生成 token、写进自己的 config.json、热登记到 registry 与 roster、立刻在列表里以"离线"出现；返回一条 `curl … /bootstrap/<name>?token=… | bash -l` 命令，脚本用 hub 的 harness 命令写 node.json、在 hub 配了 `gateway.node_binary` 时下载 steve-node、以登录 shell 启动。Agent 页签 `POST /console/agents`：校验 harness 在 hub 配置里、机器存在，catalog 整体替换后立即可用并写回配置。移除了 JSON 片段。
- **机器配置可编辑（用户指出 AI 工具、声明改不了）**：抽屉里"编辑配置"进入编辑器，分 AI 工具（名字 / 命令 / 参数）、命令、MCP 服务器（名字 / 类型 / 命令与参数或地址 / env 或 headers）、声明、标签五段。接口 `GET|PUT /console/nodes/{name}/settings`；worker 走新流 `StreamConfig`（`node_config.v1`）：node 整份校验、原子改写它启动时的 node.json、整体替换运行中的配置、唤醒探测、通知 broker，hub 随即取新快照；hub 自己写 config.json 并热更新 harness manager / MCP assembler / roster / 探测。外部 broker 的 node 其 MCP 段只读。真机验证：给 node-a 加 `git` 与 `network:lab` → node-a 的 node.json 与快照同步变化，去掉 AI 工具的坏配置被整份拒绝，改回后一致。
- **项目页第二版（用户问"项目到底是什么概念"，并指出一个项目不止一个仓库）**：定义写进页头——项目是一块干活的地方：一台机器上的一个目录，里面可以有一个或多个仓库；会话和任务属于项目，在它的目录里跑，结果落回去；hub 定两条规矩：怎么改（直接 / 隔离副本）与数据等级。列表一行一项目：名称（默认项目有徽章）、机器 / 目录、仓库（每个仓库一个芯片：路径、分支、有未提交修改的黄点）、怎么改、数据等级、活动任务、"新会话"。抽屉：仓库列表（分支、最近提交与时间、AGENTS.md、remote）、怎么改、数据等级、谁能接、权限、活动任务、最近合并、移除（确认后 hub 忘掉它，目录不动）。Steve 的家单独一行，说明它是主人私聊的默认所在、放身份与记忆。仓库信息来自 node 的 `StreamInspect`（`inspect.v1`，git 判定是否仓库、分支、最近提交、dirty、remote、AGENTS.md），hub 每分钟一轮缓存并在加项目时立刻刷新；hub 本机直接看。`POST /console/projects` 声明项目（写 config.json + 账本 Declare），`DELETE /console/projects/{id}` 退役（新增 `project.Store.Retire`；home 与默认项目拒绝）。"新会话"跳到 `#/console?new=1&project=<id>`，工作台开新线程并自动 `/project use`。线上把测试项目改名为 scratch / scratch-a / scratch-b，默认项目设为 hub 上的 scratch。
- **Agent 表第二版（用户问"需要 / 模型不固定 / 等级"是什么）**：三个词都是内部概念直出。改法：`需要` → `运行条件`，每个条件按机器逐项判定（roster `Match` 的 atom 结果），✓ / ✗ 带原因；`模型` 分两层——配置固定的偏好（开会话时经 ACP `set_config_option` 设给 AI 工具，此前就已生效）显示为"偏好 X / 未固定"，上次会话实际报告的模型显示在下面小字，"可选 N"挪进抽屉；`数据等级`从 Agent 表去掉，抽屉里写成"能接的项目：数据等级 ≤ 机器等级"。抽屉可编辑：机器、AI 工具、模型（从上次报告的可选列表里选，或不固定）、运行条件、MCP 服务器；`PUT /console/agents/{id}`（catalog 整体替换 + 写回 config.json，别名 / 提示词 / 技能 / 默认标记保留）；`DELETE /console/agents/{id}`（默认 Agent 拒绝）。readmodel Agent 新增 `preferred / observed / conditions / mcp_servers / default`，roster Candidate 新增 `Observed`。
- **Agent 的思考强度与"适合做什么"（用户指出只能固定模型）**：AI 工具通过 ACP 暴露的每个会话选项（模型、reasoning effort、mode…）现在都记进 `view.Settings.Options` → `models.Observation.Selectors`；Agent 配置新增 `options: {<option id>: <值或名字>}` 与 `about`。`harness.ApplyPreferences` 在会话打开时把模型和所有固定的选项设给 AI 工具——聊天、计划步骤、委派三条路径都调用（此前只有聊天路径设模型）。编辑器里，除模型外，AI 工具上次报告的每个选择器都各有一个下拉（"不固定"或它的选项）；没开过会话的工具会提示要先开一次。`about` 显示在 Agent 表名字下面和抽屉里，并进入规划器的候选列表（"适合：…"）和 `steve_fleet` 的输出（"good for: …"），让 leader 按它选人。
- 验证思考强度时踩到的线上问题：三台机器的 codex / claude-code 都用 `npx -y …` 启动适配器，上游发新版后每次启动都重装、卡超 60 秒初始化上限，所有会话打不开。已把三台机器的适配器改为本地安装的绝对路径（用新做的机器配置接口推过去），README 记了这条。
- **自举试验后的三处修正（2026-09-04）**：① 聊天任务不再永远"进行中"——页面上有 attempt 在跑才叫"进行中"，开着但没人说话的叫"空闲"（`taskState()`，看板 / 项目 / 关系图统一），hub 每小时把 24 小时没人说话的聊天任务（无 origin、无 live attempt）关成已完成并记进历史（`task.Store.CloseIdle`）；② 三台机器的 codex / claude-code 加 `GOCACHE=/tmp/steve-go-build`，沙箱里跑 Go 测试不再撞只读缓存；③ 同一机器上目录重叠（相同、包含、被包含）的项目拒绝登记。自举本身：steve 在自己仓库的克隆里补测试、跑测试、按要求用命令级作者身份提交，提交 87ed25c 已合回主仓库并推送。


## 13. 对话区向 Codex app 对齐，前端组件化（2026-09-04）

用户拿 Codex app 的截图要求：工具调用要像"Ran commands ⌄"那样折在正文里、每条可展开；代码块要有语言标签和复制按钮；并要求前端代码组件化、工程化，各组件风格一致。

- **对话区**：一条回复按 Codex 的顺序排——先"思考摘要 ⌄"（折叠，默认收起），再"运行了 N 条命令 ⌄"（一行一个调用：状态点、动词、等宽的命令或文件名、退出码；展开是"命令 · cwd"与"输出 · exit N"两个代码块），最后正文，末尾一行小字时间与"细节"。计划驱动的回合每个步骤一组（"repair · 运行了 13 条命令，调用了 4 次工具"，默认收起）。进行中的那一行也是同一套：头一行谁在做、多久了，随后思考摘要的最新一段、已落地的工具调用、正在成形的回答。`bash -lc "…"` 这层适配器包装被剥掉，只显示 agent 自己的命令。
- **代码块**：正文里的围栏、工具的输入输出、prompt、指令全文都经同一个 `CodeBlock`：头部图标 + 语言名（Shell / JSON / Go …，命令用终端图标）+ 元信息（cwd、exit）+ 复制按钮，超高滚动。页面里不再手写 `<pre>`。
- **组件层**：`web/console/src/components/steve/`（见该目录 README 的表与约定）。Untitled UI 原样引入；Steve 自己的组件一文件一件事：`page`（页骨架、Panel、KeyValue、Chips）、`ui`（状态徽章、Mono、空态）、`drawer`（右侧抽屉与其分节，Esc 关闭）、`markdown`（Md、CodeBlock）、`tool-calls`、`message`、`trace`（进行中、过程、给 agent 的）、`composer`、`sessions-tree`、`rail`、`call-graph`、`settings-editor`。`pages/` 只剩拼装：工作台从 783 行降到 254 行，四个页面的抽屉（机器、Agent、项目、任务）共用一个 `Drawer`；`lib/` 只放 api、类型、文案、hook、文本整理。
- 约定写在 README 里：表单控件一律 Untitled UI；字号三档（sm / xs / 11px）与语义色 token；折叠一律 `<details>` + 旋转箭头；组件不发请求。
- 没做：Codex 左侧的段落小地图；代码高亮（没有引入高亮库，只有语言名）。

## 14. 项目与工作区（2026-09-04）

用户看完组件化后的控制台说：把"一台机器上的一个目录"叫项目不对，那更像工作区；而且应该有一层在项目之上，可现在没有工作区的管理和切换。

代码里这两层其实都在（`internal/project`）：`Project` 是与机器无关的规则加一个主目录，`Workspace` 是项目在某台机器上落地的一个目录，每次执行都通过 `Materialize` 拿目录。别扭来自两处：页面和 README 把项目定义成了目录；`Materialize` 对交互回合只会给主目录，同一份东西要在别的机器上干活只能再声明一个项目——scratch / scratch-a / scratch-b 三个"项目"其实是一个项目的三个工作区。

"workspace"有两种用法，要分开：VS Code / Cursor / Codex 里它是你打开的那个文件夹，在项目之下；Linear / Slack / Multica 里它是组织或团队空间，在一切之上。steve 只有一个主人，组织这一层没有任何行为可挂，不做。本节采用前一种：**项目在上，工作区在下**。

初稿经 codex（gpt-5.6-sol）对照代码评审 24 条（原文在会话 scratchpad `workspace-codex-1.md`），采纳的结论直接写进了下面的表和规则；主要改动：副本不另立一张表，挂在项目记录上随项目一起改；放置是项目记录上的纯函数，不查库；副本的快照有自己的 head，不进主目录的合并图；克隆是异步作业，经暂存目录原子落位；左栏保持两层，工作区管理放项目页；composer 与左栏的"落点"由服务端算好下发，页面不复制规则。

### 14.1 对象

| 对象 | 是什么 | 权威 | 一个项目里有几个 |
|---|---|---|---|
| 项目 | 一件要做的事及其规矩：仓库（外部 remote）、怎么改（直接 / 隔离）、数据等级、固定的技能、耐久落点、权限；有一个主目录；**带着它的副本表** | 账本 `project`（配置 `projects{}` 在启动时 Declare；副本在 `projects.<id>.workspaces[]`） | — |
| 主目录（canonical 工作区） | 项目在其主机器上的目录本身；合并落回这里；写锁 `canonical:<project>`，在主目录所在区域签发 | 由项目的 `home` 派生 | 恰好 1 |
| 副本（`Copy`，**新**） | 用户声明的、长期存在的另一个目录：认领某台机器上已有的目录，或从项目的 remote 克隆；交互回合可以在这里跑；有状态 ready / provisioning / failed；写锁就是它的 id `copy:<project>/<node>`，在副本所在区域签发 | 项目记录里的 `copies` 映射（按机器）；配置只声明位置，账本记来历（origin / source / by / at / state） | 0..n，每台机器至多 1 个，主机器上不能有 |
| 执行工作树（worktree） | artifact store 为一次 attempt 从某个 artifact 检出的临时目录（计划步骤、委派、验证）；结束即弃；锁 `workspace:<id>` | artifact store | 随 attempt 生灭 |
| 会话 | 绑定项目（不绑工作区）。它此刻在哪个工作区，由当前 Agent 所在机器经放置规则得出，服务端算好随会话摘要下发 | 账本 `conversation-project` | — |
| 任务 / attempt | attempt 记录它实际运行的工作区（`Spec.Workspace`，解析后的快照，不是权威）；`Open` 校验它与 attempt 的项目、机器一致 | 账本 | — |

配置与账本的关系：配置是声明，账本是记录。启动时 `Declare` 把整个项目记录按配置重写，副本按机器合并——同一位置的副本保留账本里的来历与状态，换了位置的按新认领处理，配置里没有的忘掉。页面上加删副本先写账本再写配置，配置写失败回滚账本。

### 14.2 规则（一处判定，处处一致）

- **放置**：`Project.Place(node)` 是项目记录上的纯函数——机器是主机器 → 主目录；机器上有 **ready** 的副本 → 副本；否则拒绝，错误带项目现有工作区的位置。交互回合的 `Materialize(Isolated:false)`、上下文栏每个 Agent 的可用性与落点、会话摘要的落点、readmodel 里每个工作区能接的 Agent、doctor 的探测目录，全部走它。`Isolated:true` 仍由 artifact store 开工作树。
- **写锁**：`attempt.Open` 允许 `ScopeUnrestricted` 落在主目录或副本；主目录锁 `canonical:<project>`（主目录所在区域），副本锁 `copy:<project>/<node>`（副本所在区域），工作树锁 `workspace:<id>`——三个名字空间不共用。`Open` 拒绝工作区与 attempt 项目 / 机器不一致的 spec。
- **数据等级**：副本只能放在等级不低于项目的机器上；声明（启动 Declare 与页面添加）时由 store 经 `Levels` 钩子查机器等级；sealed 项目不能有副本。
- **目录唯一**：同一台机器上，任何两个工作区（任何项目的主目录或副本）的目录不得相同或嵌套；`Store.Conflict` 一处判定，添加项目与添加副本都经它。已知不覆盖 symlink / 共享挂载 / 大小写别名。
- **快照**：直接修改模式下，回合在哪个工作区跑就给哪个工作区拍前后快照。主目录走 `SnapshotCanonical`，推进项目的 canonical 名；副本走 `SnapshotWorkspace`：parent 是该副本上一次快照（名 `workspace/<id>/head`），Manifest 记 `workspace`，**不**标 canonical、**不**推进 canonical 名、**不**触发 pending landing——副本的历史是它自己的，只有隔离步骤发布的结果进合并图。副本不自动落回主目录，靠 git 与主目录同步，页面用分支 / 未提交 / remote 把差异摆出来。
- **计划与委派的基线仍是主目录**：副本会话里发起的 `/plan`、委派、验证，以主目录当前快照为 base，结果落回主目录。副本里没推回主目录的修改，子 Agent 看不到——这是现状，页面在副本会话里要写明；改成按 attempt 固定 execution base 是下一步。
- **克隆来源**：项目配了 `external_remote` 用它；否则用主目录本身作为单个 git 仓库的 remote（inspect 报告的）；两者都没有就不能克隆，只能认领。不再用影子仓库冒充 remote。
- **克隆是作业**：请求立刻返回，副本以 provisioning 记录并显示；hub 在那台机器上经 `exec` 流跑脚本：`GIT_TERMINAL_PROMPT=0`，在目标的父目录下 `mktemp` 一个暂存目录克隆，成功后 `mv` 到目标，失败只清理暂存；10 分钟上限；结果写回副本状态（ready / failed + 输出尾部）。目标已存在拒绝克隆（认领它）；认领的目录不存在拒绝认领（克隆它）。
- **移除**：先取副本的锁（`attempt.Service.Hold`），取不到就是有回合在跑，拒绝（409）；取到后从项目记录删掉副本、写配置。目录不动。绑定过它的会话在下一回合按 `sessionDrifted` 归档重开。主目录不能移除（那是退役项目）。
- **仓库信息**：`inspect.v1` 对每个工作区各查一次（`repoCache` 以工作区 id 为键）。

### 14.3 页面

- **左栏**保持两层：项目 → 会话。项目名下一行小字列出它在哪："主目录 hub · 副本 node-a"。会话行在项目有多个工作区时显示落点徽章（服务端下发的 `place`）；Agent 所在机器上没有工作区的会话带红色标记。评审否决了三层树：会话不属于工作区（`/use` 换 Agent 就换了落点，会在树里跳），只有主目录时又平白多一层。
- **项目页**：表格"工作区"一列列出每个工作区（种类 · 机器 · 目录，非 ready 的带状态）。抽屉"工作区"一节一卡一个：种类、机器、目录、状态（正在克隆 / 克隆失败 + 输出）、有回合在跑、来源、仓库（分支 / 未提交 / 最近提交 / remote / AGENTS.md）、这里能接的 Agent；副本卡有"移除"（确认后忘掉，目录不动）。"添加副本"对话框：机器（排除已有工作区的）、怎么来（认领已有目录 / 克隆一份——没有 remote 时克隆不可选并说明）、目录。页头定义改成：项目是一件要做的事及其规矩；工作区是它在机器上的目录，主目录一个，副本每台机器至多一个。
- **composer**：项目芯片上显示落点（"home · 主目录 · n251"，或红色"node-b 上没有工作区"），来自 `/console/context` 里每个 Agent 的 `place`。不做单独的"切换工作区"：每台机器至多一个副本，换机器就是换 Agent。
- **文案**：`ProjectNotHome` 列出项目有工作区的机器，并提示可以在 Agent 所在机器上添加一个；`ContextNotHome` 同样列出位置。

### 14.4 接口与配置

- `POST /console/projects/{id}/workspaces {node, path, origin: "adopt"|"clone"}`；`DELETE /console/projects/{id}/workspaces/{node}`（忙则 409）。
- readmodel `Project.workspaces[]`：`id, node, path, kind, origin, source, state, error, busy, repos, agents`；顶层 `node / path / repos` 是主目录的，`agents` 是各工作区之并。`AgentChoice.place` 与 `Conversation.place`：`{workspace, kind, node}`。
- 配置 `projects.<id>.workspaces: [{node, path}]`。

### 14.5 顺序与线上迁移

1. 对象与规则（已做）：项目记录带副本、纯放置、锁、快照、投影、页面。
2. 认领 / 移除（已做）：线上把 scratch-a、scratch-b 退役，把它们的目录认领为 scratch 在 node-a / node-b 的副本。它们下面只有测试用的 `/project use` 会话，历史任务保留旧项目 id，不改审计记录；正式项目要迁移时得先冻结、清空在途 attempt 与 pending landing、重绑会话，再认领——这套 migration 没做。
3. 克隆（已做）：线上用 steve-self 在 node-a 克隆一个副本验收。

### 14.6 不做 / 已知缺口

- 组织 / 团队级的"空间"；多主人。
- 换主目录（退役再声明）；副本自动同步或自动落回主目录；执行工作树进左栏树。
- 评审指出但本次没动的既有问题：artifact Manifest 以裸 commit SHA 为全局键、`homeRegion` 查询失败时静默用本区域、`RepoMode=isolated` 对交互回合并未生效、目录唯一不识别 symlink 与共享挂载、机器改名会改变副本 id。

## 15. 会话的名字与归档（2026-09-04）

用户指出：会话不能归档、不能改名；标题应该像 ChatGPT 那样是摘要，而不是第一句话，最好由 agent 给。

- **名字**：第一次真正的交换（非动词的一句加上它的回复）落地后，hub 在后台让默认 Agent 在一个只为这件事开的会话里看这段开头，起一个不超过 12 个字的标题（`cmd/steve/titler.go`：新开 ACP 会话、发一个问句、拿到就关，不碰这条会话自己的上下文）。答案清理掉引号、前缀和标点后记为 `title_by: agent`。没有 titler 时仍用第一句。用户改名记为 `title_by: user`，之后 agent 不再覆盖；留空提交则交还给 agent 重新起。
- **归档**：会话行的 "⋯" 菜单：重命名（就地编辑，Enter 保存、Esc 取消）、归档 / 取消归档。归档的会话从项目下移到底部折叠的"已归档 · n"，当前打开的那条即使归档也留在原处。
- **存储**：会话的名字、谁起的、是否归档记在控制台转录文档的 `meta` 段（`console.Meta`），与转录一起持久；旧格式（只有转录）照读。`PUT /console/conversations/{id} {title?, archived?}`；变化以 `console.meta` 事件推给页面。
- 真机：codex 在 hub 上给"看一下当前目录里有几个文件"起名"统计当前目录文件数"，8 秒内出现；改名、归档、交还都经接口验过。

## 16. 技能页与档案页（2026-09-04）

用户问：还是没有 skill 管理跟 profile 管理的页面？此前两者只有聊天动词（`/skills`）和直接改文件。

- **技能**（`#/skills`）：定义写在页头——一个技能是一个目录里的一份 SKILL.md；hub 在搜索目录里找，这里打开的技能打成一个包发给每台机器、交给每个 Agent。表格一行一个技能：名称（SKILL.md 的 name / 首个标题）、说明（front matter 的 description 或第一段）、来源目录、固定在（哪些 Agent / 项目按目录另外要了它）、启用开关、"看 SKILL.md"（抽屉里渲染）。下面两块：搜索目录（列表 + 添加 / 移除）、机器上的技能包（每台机器：已同步 / 未同步 / 离线 / 不接收，按 advert 里的包哈希对 hub 最近打的包）。改动走与 `/skills` 同一把锁（`Coordinator.SkillsLock`）：有回合在跑时拒绝（409），因为改完会重启 AI 工具。接口 `GET /console/skills`、`GET|PUT /console/skills/{name}`、`POST|DELETE /console/skills/paths`。
- **档案**（`#/home`，起初叫"Steve 的家"，用户觉得怪，改名）：三份文件各一张卡——身份 SOUL.md、用户 USER.md、记忆 MEMORY.md——等宽编辑框、字节数 / 预算条（8 KB / 8 KB / 24 KB）、"还是模板" / "文件不存在"徽章、保存（超预算拒绝，因为超出的部分本来就到不了 Agent）与还原。顶部"注入"块说明：私聊注入三份（总上限 40 KB，超出从记忆末尾截）、群聊 / 访客只有身份，并给出两种模式实际的字节数与 `home.Load` 的提醒。写入是同目录临时文件 + rename，符号链接只跟到家目录之内（`home.Write`）。接口 `GET /console/home`、`PUT /console/home/{name}`。
- 导航"环境"组新增两项。
- 用户随后指出三处：SOUL 模板把 Codex / Claude Code / Grok / Kimi 写死（工具是什么由 fleet 决定，模板改成"你调度的 AI 工具是你的手，有哪些看本轮的清单"）；"主人"这个词去掉（模板、包裹、onboarding、setup 文案里改成"用户"或角色名 owner）；USER.md 模板里的 "Feishu open_id" 行去掉——渠道身份是配置的事，不是用户画像；onboarding 的起草规则同步改为"不要把渠道标识写进 USER.md"。线上的 SOUL.md / USER.md 按同样的改法改了两行。

## 17. 内置技能（2026-09-04）

用户指出：技能没有系统内置，至少要带 Anthropic 官方的 skill-creator；还要有讲 steve 怎么协作的技能，否则 agent 不知道这套东西怎么用；steve 的技能可以是套件，按功能路由。

- **随二进制发布**：`internal/skills/builtin/` 用 `go:embed` 打进 hub，启动时写到 `<state_dir>/skills-builtin/`（写到旁边再整体换名，不会出现半个目录），作为**最后一个**搜索目录——用户同名技能盖过内置。首次见到的内置技能自动启用；用户关掉过的记在 map 文件的 `builtins` 里，下次启动不再打开。内置目录不能从搜索路径移除（逐个关）。技能包按启用集打包发给各机器，内置的自然在内。
- **skill-creator**：Anthropic 官方原文（anthropics/skills，commit 见 `internal/skills/builtin/README.md`），Apache-2.0，含 scripts / references / eval-viewer；许可证随目录带着。
- **steve 套件**：`steve`（入口：一分钟模型、开工时拿到了什么、路由表、硬规则）→ `steve-delegate`（steve_fleet / steve_delegate / steve_await、选择器词表、引用不传内容、结果怎么落地、何时不委派）、`steve-projects`（项目 / 主目录 / 副本 / 工作树、只在工作区内写、被告知项目在别的机器时的三条路、数据等级、/project 与 /grant）、`steve-plans`（任务与回合预算、attempt 租约、/plan 的 DAG / 隔离工作树 / 验证 / 重规划、在步骤里该怎么做、/every /at /schedules、/repair /fleet）、`steve-memory`（三份档案何时注入、预算、群聊与访客、怎么记、写被拒怎么办、第一次见面）、`steve-feishu`（进度卡的时机与用法、不 @、最终答案不用它发）。内容只写代码里有的：动词表来自 i18n，工具参数来自 `agentmcp/server.go`，工作区规则来自 README 与 §14。
- **页面**：技能表里内置的带"内置"徽章；内置目录那行不给移除按钮，写"内置 · 随 steve 更新"。
- **从互联网安装**（用户随后指出页面只能加本地目录）：加了"来源"这一层——一个 git 仓库。接受 GitHub 链接、`owner/repo`、`owner/repo/子目录`、指到子目录的 tree 链接（带分支）、以及任何 git 地址（ssh 走 hub 自己的密钥）。hub 浅克隆到 `<state_dir>/skills-sources/<slug>/`，在克隆里找技能（顶层 SKILL.md、各子目录、或 `skills/` 下），用一个链接目录 `<slug>.skills/` 列出它们并作为搜索路径；`Available()` 把链接解析成真实目录，所以打包和 harness 看到的都是实体。安装不启用任何技能；启用仍是逐个开关。"全部更新"重新 fetch 并 reset，因为已启用技能的正文可能变了，所以走技能锁、重打包、重启 AI 工具。移除来源删掉克隆，从它启用的技能一起关掉。聊天动词 `/skills add <仓库>` 与 `/skills update` 现在真的存在（此前帮助里写了但没实现）。接口 `POST /console/skills/sources`、`POST /console/skills/sources/update`、`DELETE /console/skills/sources/{slug}`。同名冲突按搜索顺序先见者胜：用户目录 > 内置 > 来源。
- **三处再改（用户反馈）**：① 技能抽屉的表格没渲染——`Md` 挂上 `remark-gfm`；② 机器上各自装好的技能 hub 看不见——新增"机器上已有的技能"：hub 用 exec 流在每台机器（含自己）上跑一段 shell 列出 `~/.codex/skills`、`~/.claude/skills`、`~/.grok/skills`、`~/.kimi/skills`、`~/.agents/skills` 下带 SKILL.md 的目录并带回 SKILL.md 开头，页面按机器列出，"加载到 hub"把它 tar 过来解到 hub 的用户技能目录（有安全检查：不出目录、16 MB 上限、必须带 SKILL.md、同名拒绝），之后它就是一个普通技能，再决定启不启用（`GET /console/skills/machines`、`POST /console/skills/import`）；③ 来源里没启用的技能不占主表——主表只列内置、用户目录里的、和从来源启用的；来源块里每个技能一个开关。
- **内置套件改成引导而不是规矩（用户指出 skill 是软限制，强控流程该用代码）**：删掉"硬规则"一节和"不要…"式条目，换成"平台替你做的事"（投递、单写者、子任务身份、写入范围由工具权限管）与"怎么做更顺"（怎么写委派目标、什么时候一张卡有用、什么值得记）。凡是必须成立的事，由代码保证；技能只负责引导。
- **机器技能改为申报 + 缓存（用户要求缓存、异步上报刷新）**：不再在页面打开时用 exec 去每台机器上跑脚本。每台机器（含 hub 自己）用 Go 扫本机 AI 工具目录（`skills.ScanLocal`，按物理路径去重，5 分钟内复用上次结果），把结果放进申报 `Advert.OwnSkills`；hub 的注册表本来就缓存每台机器最近一次申报、每分钟催一次（`RefreshEvery`），所以页面读的是缓存、秒开，最多一分钟旧。"让机器现在重扫"走 `POST /console/skills/machines/refresh`：hub 并行向每台机器要一次新申报（20 秒上限）并重扫自己。加载仍走 exec 流的 tar。旧版本 node 不带这个字段，页面显示"没有"直到升级。

## 18. MCP 页与市场（2026-09-04）

用户问：MCP 呢？并指出不同的 coding agent 各有自己的市场。三条决定：先做 MCP 页再做市场；从注册表装 MCP 时选机器、默认 hub；Claude 插件第一版取 skills 与 .mcp.json，但插件的其它部件要按"steve 自己也是一个 agent"来设计映射，不是丢掉。

初稿经 codex（gpt-5.6-sol）对照代码评审 32 条（原文在会话 scratchpad `mcp-codex-1.md`），本节是采纳后的版本。改得最大的几处：MCP 的领域模型分三层（服务 / 部署 / 引用），不再"同名即同一服务"；平台会话级的 MCP 单列，不进机器探测；探测走 broker 的 Bind → Release，不另起进程，且只手动、不周期；工具清单不进能力快照，只是 hub 的观测投影；secret 的承诺缩到能兑现的范围并列出现有编辑路径的缺口；注册表安装按包变体逐项映射、固定版本、拦内网地址、装前显示完整命令；市场拆成目录 / 制品 / 安装 / 激活四层；插件 agent 是独立的角色模板，hooks 在事件框架前一律算不兼容。

### 18.1 MCP 页（已做）

| 层 | 是什么 | 权威 |
|---|---|---|
| 服务 | 逻辑上的一个 MCP 服务及其来历（注册表条目、纳入自哪个 coding agent、手写） | 目前只有来历字符串，随部署记录；正式的服务 id 等市场阶段一起做 |
| 部署 | 某台机器上名为 X 的一份配置：类型、命令 / 地址、env 与 headers 的**键名**、命令能否解析、最近一次探测 | 那台机器的 node.json（hub：config.json）；hub 读它的 `Settings`（机器不可达时退到申报快照里的 `mcp:` 能力） |
| 引用 | Agent 按名字接它所在机器上的部署 | agent 目录 |
| 平台会话级 | hub 为每个会话现场生成的 `feishu`（进度卡 + 委派工具） | 代码；页面单列，工具清单直接从实现列出，不装不改 |
| 机器自有 | coding agent 自己配置里的服务器 | 机器申报 `Advert.OwnMCP`，特性 `mcp_probe.v1` 说明"这台机器会报"，旧版本显示"版本不支持"而不是"没有" |
| 探测结果 | 某部署最近一次 `tools/list`：工具、输入 schema、摘要、server 名 / 版本、错误 | hub 内存缓存，键 `<机器>/<名字>` 并记探测时的形状摘要；形状变了标"旧"；失败保留上次成功的清单并标 stale |

规则：

- **同名不代表同一服务**：一行一个部署；同名部署标"同名"，是否同一服务由用户判断。平台名字 `feishu`、`steve*` 保留，纳入与安装不得使用。
- **探测**：只在用户点"探测"时做，一台机器同一时刻一个；node 上经 broker `Bind(name, "probe:<id>", "probe")` 拿到会话会拿到的同一份启动方式（stdio 经 launcher，http 经回环代理注入 headers），跑最小客户端（`initialize` → `notifications/initialized` → `tools/list`，支持 stdio / streamable-http / 旧式 sse，翻页，20 秒上限，不调用工具），然后 `Release`。hub 自己的部署 hub 本地直接起（hub 没有 broker，这是既有的双轨，见缺口）。页面写明"探测会真的启动一次服务器，可能有副作用"。
- **工具摘要**：`mcp-tools-digest.v1` = 按名排序的 (name, inputSchema 规范化 JSON) 哈希，16 位；**不进能力快照**（评审指出会搅动 admission 摘要且放置匹配不了 attrs），只在观测投影里。
- **secret**：页面任何地方只显示键名。纳入由机器自己完成（node 收到 `adopt <source> <name>` 后在本机查自己的文件、写自己的 node.json；hub 对 hub 自己同理），值不经过 hub。注册表安装时页面填的值只发给目标机器一次。**缺口**：现有的机器"编辑配置"路径（`Settings` get / `ConfigReply`）仍回传明文 env / headers 到页面，hub–node 之间是带 token 的裸 TCP；改成"键名 + keep/replace/delete"与 mTLS 是下一项。
- **机器自有 MCP**：Codex `config.toml` 的 `[mcp_servers.*]`（command / args / env / env_vars / url / bearer_token_env_var / http_headers，含多行数组与子表、带引号的名字）与 Claude Code `~/.claude.json`（顶层与各 project 的 `mcpServers`）；尊重 `CODEX_HOME` / `CLAUDE_CONFIG_DIR`；URL 去掉 userinfo 与 query，形似密钥的参数值遮掉。项目级的只展示不纳入。
- **注册表**：`GET /v0/servers?search=`；用户选一个具体的包或远端；npm → `npx -y <id>@<version>`，pypi → `uvx <id>@<version>`，oci → `docker run -i --rm -e VAR… <image>`，nuget → `dnx`；`runtimeArguments` / `packageArguments` 逐项进 argv（保留 named 参数的名与值）；`environmentVariables` 生成表单（必填 / 密码框 / 默认值）。目标机器没有所需运行时（`tool:npx` 等）直接拒绝。远端地址解析后指向本机、私网、链路本地、云 metadata 的一律拒绝。装前页面显示完整命令与来源仓库。
- **删除**：hub 先查 agent 目录，那台机器上有 Agent 引用就拒绝；然后改那台机器的配置。

页面（`#/mcp`）：已装的服务器表（名字 / 机器 / 类型 / 命令或地址 / 谁在用 / 工具数 / 状态：已配置 → 命令找不到 → 探测通过 / 失败）、抽屉（形状、键名、引用、来历、工具清单可展开 schema、摘要、探测、删除）、平台会话级、机器上 coding agent 自己配的（纳入）、从注册表安装。接口：`GET /console/mcp`、`POST /console/mcp/probe`、`POST /console/mcp/adopt`、`DELETE /console/mcp?node=&name=`、`GET /console/mcp/registry?q=`、`POST /console/mcp/install`。node 侧：`Advert.OwnMCP`、流 `mcp_probe`、配置流动词 `adopt`。

评审指出但本次没动的既有问题：`Coverage[MCP]` 默认 complete、stdio 只查 PATH 就算"存在"；attempt 级 BIND/RELEASE 与跨回合复用的 ACP 会话生命周期不一致；node.json 改写没有 revision CAS；hub 自己的 MCP 不走 broker。来历目前只在内存，重启后丢。

### 18.2 市场（第二阶段，设计已按评审改）

事实：Codex 的市场是 `openai/skills` 目录（system / curated / experimental，`$skill-installer`；仓库已标记弃用、转向 OpenAI Plugins）；Claude Code 是 git 仓库里的 `.claude-plugin/marketplace.json`，插件打包 skills / commands / agents / hooks / `.mcp.json`（官方 `anthropics/claude-plugins-official`）；通用的有 `anthropics/skills`、skills.sh；MCP 有官方注册表（18.1 已接）。

**四层，而不是给现有 git 来源加枚举**：`CatalogSource`（怎么列出条目：git 技能仓库、Claude marketplace.json、Codex 三层目录、MCP 注册表）→ `Artifact`（一条条目在某个 commit / 版本的内容与内容摘要）→ `Installation`（装到 hub 的那一份，带锁：目录地址 + 目录 commit + 条目名 + 来源种类 + 声明的 ref + 解析后的 commit / 精确版本与 integrity + 子目录 + 内容摘要）→ `Activation`（对谁生效：全局 / 项目 / Agent）。现有 `skills.Source` 是 git 技能仓库这一种 resolver。

**插件部件映射**（steve 自己是一个 agent，有自己的编制）：

| 部件 | 在 steve 里 | 阶段 |
|---|---|---|
| `skills/` | 技能；启用记录绑定内容摘要；同名冲突要用户选，不再默认先见者胜 | 一 |
| `commands/` | 转成技能，标"转换后的指导技能"，不宣称兼容 Claude 的斜杠命令 | 一 |
| `.mcp.json`、`openai.yaml` 的 MCP 依赖 | 生成 MCP 页的**安装提案**（目标机器、命令、所需 secret），不自动装 | 一 |
| `agents/` | 独立的 `RoleTemplate`：提示词、模型约束、技能 / MCP 依赖、来源锁、兼容矩阵。steve Agent（机器 + AI 工具 + 模型）可**引用**模板；只有用户选了机器和 AI 工具才生成持久 Agent，或委派时开临时角色会话。逐字段兼容矩阵：可精确映射 / 软映射（系统提示是拼进首轮 prompt 的软指导；工具限制要各 harness 适配器映射到原生沙箱，映射不了的标"不支持强制工具策略"）/ 不支持（阻止"完整安装"或要求接受降级） | 二 |
| `hooks/` | 在 steve 有版本化事件总线之前一律"不兼容"：不计入已安装能力，放兼容性报告，醒目写"未加载、未执行、不提供保护"；插件声明 hook 必需的默认阻止安装 | 三 |

### 18.3 顺序

1. MCP 页（已做）→ 2. 编辑配置路径的 secret 改为键名 + keep/replace/delete；来历持久化 → 3. 市场四层与内置官方目录（skills / commands / MCP 提案）→ 4. RoleTemplate → 5. 事件总线与 hooks。

### 18.4 不做

- 扫项目内 `.mcp.json`、Kimi / Grok 的 MCP 配置（格式未核实）。
- 调用 MCP 工具做健康检查；周期性自动探测。
- OAuth 类的远端认证。

## 19. steve 自己的 MCP，与钩子（2026-09-04，方案）

用户看到 botmux 的 MCP 后指出：steve 的那套"怎么协作"不该是技能，该是 MCP。技能的控制权在 client（agent 读不读、照不照做都是它的事）；MCP 工具一旦被调用，控制权在 server——steve 能给活数据、能校验参数、能记账、能拒绝。另外钩子要正式支持。

### 19.1 把套件变成工具

hub 已经为每个会话现场生成一个 MCP 服务器（今天叫 `feishu`：进度卡三件套，接了委派时再加 `steve_fleet / steve_delegate / steve_await`；每个会话一个 token，绑定会话、Agent、任务）。套件里的内容按"是活数据还是做法"分：活数据变成工具，做法留在工具的 description 与 `steve_help` 里。

| 套件里的内容 | 变成 | 为什么在 server 侧更好 |
|---|---|---|
| 你是谁、在哪台机器、哪个项目、哪个工作区、任务预算、接了哪些 MCP、启用了哪些技能、私聊还是群聊 | `steve_context()` | 全是活数据；技能里只能写"看本轮指令"，指令只在第一轮 |
| 项目 / 主目录 / 副本 / 数据等级、被告知项目在别的机器时的三条路 | `steve_projects()`：列项目、各自在哪、本机能接哪些；答复里直接给出"换 Agent / 加副本 / 换项目"的具体选项 | 放置规则由 hub 算，agent 不必复述 |
| 记住一件事、更新用户档案 | `steve_remember(section, text)`、`steve_profile(field, value)` | 预算、小节、去重、审计都在 server 做；不再依赖 agent 有写文件权限 |
| 步骤做完怎么报告 | `steve_report(changed_paths, verified_by, unfinished, notes)` | 结构化落账，重规划靠它，不靠 agent 的散文 |
| 各主题的做法（委派怎么写目标、什么时候发卡、什么值得记） | `steve_help(topic)` | 按需拉取，不占首轮窗口；内容随二进制更新 |
| 委派、看机器、等结果、进度卡 | 已是工具 | — |

规则：

- 服务器名从 `feishu` 改为 `steve`（它早就不只是飞书）；工具名保持 `feishu_*` 与 `steve_*`。改名进能力指纹，已开会话会提示 /new。
- 每个工具的 description 写清何时用、参数意义与副作用——这是 agent 真正会读的"技能"。`Instructions`（会话首轮的一段）缩成一句：先调 `steve_context`。
- `steve_remember` 只写 MEMORY.md 的三个小节之一（偏好 / 项目 / 人），server 校验预算与重复，写入用 `home.Write` 的原子改写，并记一条审计事件；群聊 / 访客会话调用直接拒绝。`steve_profile` 只改称呼与时区两项。
- `steve_report` 只在计划步骤或委派的 attempt 里可用（token 绑定的 task 能判），写进 attempt 的 Result；聊天回合调用返回"这不是一个步骤"。
- 内置技能只剩 `skill-creator`；`steve` 与 `steve-*` 删除，内容并入 `steve_help` 的主题。

### 19.2 钩子

评审（§18）说得对：先有 steve 自己的事件模型，再谈兼容谁的 hook。

| 事件 | 在哪发 | 可阻断 |
|---|---|---|
| `turn.before` / `turn.after` | 交互回合开工前 / 结束后（coordinator） | before 可 |
| `attempt.open` / `attempt.close` | 租约拿到 / 落账（attempt.Service） | 否 |
| `step.publish` | 计划步骤发布产物后、验证前（exec） | 可（拒绝即视为验证失败） |
| `landing.before` / `landing.after` | 合并落回主目录前 / 后（artifact） | before 可 |
| `delegate.start` / `delegate.done` | 委派发起 / 收回（delegate） | start 可 |
| `tool.before` / `tool.after` | steve 自己的 MCP 工具被调用前 / 后（agentmcp） | before 可 |
| `memory.write` | `steve_remember` / `steve_profile` 写入后 | 否 |

- **处理器**三种：`command`（hub 上跑一条命令，事件 JSON 进 stdin，可阻断的事件看退出码与 stdout 的 `{"decision":"block","reason":...}`）、`http`（POST 到一个地址）、`mcp`（调用某台机器上某个 MCP 服务器的工具）。每个处理器有超时（默认 10 秒）、失败策略（`ignore` / `block`）。
- **配置**在 hub：`hooks: [{event, match: {project?, agent?, tool?}, run: {...}, blocking, timeout}]`，页面上有"钩子"一节（放在 MCP 页或单独页），每条钩子显示最近触发记录。
- **审计**：每次触发进历史（事件、处理器、耗时、结论）。
- **兼容 Claude 插件的 hooks**：只有能映射到上表的才接——`PreToolUse` / `PostToolUse` 仅对 steve 自己的 MCP 工具生效（harness 内部的工具调用 steve 看不见）；其它事件标"不支持"。是否把插件 hooks 写进 harness 的原生配置（Claude Code 的 hooks、Codex 没有），由各 harness 适配器决定，第一版不做——今天隔离运行目录明确过滤了宿主的 hooks，那是为了可重复，不轻易放开。

### 19.2.1 评审结论（codex，39 条，原文在会话 scratchpad `hooks-codex-1.md`）

采纳并已改：`steve_help` 只是指南，控制仍在 coordinator / attempt / exec / artifact 里，文档不再把"工具化"说成"受控"；`Instructions` 保留完整契约，只在开头加一句先调 `steve_context`；不支持 HTTP MCP 的 AI 工具和计划步骤拿不到平台 MCP，所以保留一份只读的 `steve` 总览技能作为兼容；群聊 / 访客的 `steve_context` 不返回目录路径。

采纳、进后续顺序：平台 MCP 要有版本号进指纹（工具 schema 与 help 内容变化才会触发 /new）；引入 `InvocationBinding`（会话、回合、任务、attempt、principal、模式），有副作用的工具只在活跃回合里可调；计划步骤也注入平台 MCP（`StepRequest` 带 attempt id，一次性 token）；`steve_remember` / `steve_profile` 建在 home 包的事务写入（文件锁 + CAS + fsync）之上，按完整 owner 快照算实际能进 prompt 的预算，幂等键，审计先记 intent 再写文件；`steve_report` 只接受 agent 的声明（summary / refs / findings{invalidates} / unfinished / notes），changed paths、验证、产物由 server 填，不推进 attempt 状态，`unfinished` 产生明确的未完成结果而不是落地；事件有稳定 envelope（schema 版本、event id、correlation / causation、task / attempt / revision、principal、payload 分级），每个事件写明状态机位置，blocking 串行首个 block 即止、after 强制非阻断、安全类 fail-closed、通知类 fail-open、总耗时上限、递归深度；`command` 处理器用 argv 不经 shell、最小环境、受限 cwd、无 gateway secret；`http` 处理器 allowlist + 解析后 IP 检查 + 不跟重定向；`mcp` 处理器走 admission / Bind / Release / Intents，禁止调平台自己的 `steve`；审计落账本；Claude 的 `PreToolUse` / `PostToolUse` 只算语义子集，其余事件标不兼容；服务器改名一次性让已开会话提示 /new（单主人可接受），`reservedMCP` 下沉到配置加载。

### 19.3 已做（第一步）

- 平台 MCP 服务器改名 `steve`；新工具 `steve_context`（coordinator 的 `Where`：Agent / 机器 / 模式 / 项目 / 工作区或为什么没有 / 任务预算 / MCP / 技能，外加平台替你做的事）、`steve_projects`（`WhereProjects`：每个项目在哪、本机能不能接、三条出路）、`steve_help(topic)`（六个主题的做法，随二进制发布）。`Instructions` 开头加一句"先调 steve_context"。
- 内置技能剩 `skill-creator` 和一份只读的 `steve` 总览（给拿不到平台 MCP 的环境）；`steve-*` 删除，正文并入 `steve_help` 的主题（`internal/agentmcp/help/`）。
- 会话的模式（私聊 / 群聊）在每条请求进来时记下，工具调用时查。

### 19.3.1 维护这套系统（用户：增删节点、感知其他节点的状态）

agent 得能自己维护 fleet，不只是人从页面操作。平台 MCP 再加四个工具：`steve_nodes`（每台机器：在线与否及自何时、版本、数据等级、系统、健康——空闲磁盘 / 负载 / 工作树、AI 工具及各自能否启动、MCP 服务器、自有技能数、放在上面的 Agent；`steve_fleet` 列 agent，它列机器）、`steve_node_add(name, addr, level?, hub_url?)`（登记并返回那台机器要跑的一行 bootstrap 命令）、`steve_node_remove(name)`（忘掉一台机器：上面还有 Agent、或有项目的主目录 / 副本时拒绝）、`steve_node_refresh(name)`（让机器现在重新看自己并返回摘要）。增删只允许用户在私聊里发起的会话（模式 owner，且不是受委派的子任务）；看和刷新谁都可以。页面同步补上"移除机器"（此前只有添加），接口 `DELETE /console/nodes/{name}`，注册表新增 `Remove`（关连接、停拨号、不再列出）。

### 19.4 顺序（按评审改）

1. 已做：`steve_context`、`steve_help`、`steve_projects`；改名 `steve`；套件改为 help 主题，保留只读总览技能。
2. 平台 MCP 版本化进指纹；`InvocationBinding`；计划步骤注入平台 MCP。
3. home 事务写入与耐久审计 → `steve_remember`、`steve_profile`。
4. `steve_report`（agent 声明通道）接 exec / delegate。
5. 事件 envelope 与 outbox；`tool.before/after`、`turn.before/after`；沙箱化的 command / http 处理器；配置与页面；审计落账本。
6. 其余事件、mcp 处理器、Claude hooks 的子集映射。

## 20. 记忆：作用域与提供者（2026-09-04，已评审）

用户指出：记忆现在只有一个 MEMORY.md，太简陋；要做成插件式，并且要有项目记忆和全局记忆。

### 20.1 对象

| 对象 | 是什么 | 权威 |
|---|---|---|
| 作用域 | `global`（用户的，跨项目）与 `project`（这个项目的）。agent 只能说这两个词；项目是会话绑定的那个，不由 agent 指名 | — |
| 事实 | 一条短句（≤ 500 字），属于一个作用域、一个小节，有不可变 id | 权威存储 |
| 权威存储 | 每个作用域一份 markdown：全局 = 档案里的 MEMORY.md（不变，兼容）；项目 = `<state_dir>/memory/projects/<id>.md`。Steve 写的每条带 `<!-- m:id -->`；用户手打的行以内容哈希为 id，直到 Steve 重写它 | hub 状态目录；写走同一把锁（flock）+ fsync + rename |
| 检索器 | 可选：一个 MCP 服务器，对同一批事实建索引、按问题召回。它只是索引，不是权威：换掉它、它挂了，事实一条不少 | 那台机器上的部署（§18）；phase 2 |
| 注入 | 新会话第一轮：全局记忆（24 KB，仅私聊）+ 当前项目的记忆（16 KB，仅私聊且会话绑了项目）；两者都在身份之后、不进指纹 | assembler |
| 召回 | 按需：`steve_recall(query, scope?)`；没有检索器时是关键词匹配 | Service |
| 工具 | `steve_remember(scope, section, text)`、`steve_recall(query, scope?)`、`steve_forget(scope, id)` | 平台 MCP `steve` |
| 审计 | 每次写一行 `<state_dir>/memory/audit.jsonl`：谁（会话、agent、来源）、作用域、小节、id、长度、结果；不含正文 | Service |

### 20.2 规则

- 作用域不混：全局记的是关于用户的长期事实（偏好 / 项目 / 人）；项目记的是关于这个项目的（约定 / 决策 / 坑）。agent 不确定放哪就放项目；用户在档案页能挪。
- 群聊与访客一律不注入、不可记、不可召回；全局只在用户私聊；被委派的子任务不能写。
- 幂等：同作用域规范化后相同文本是同一条，返回同一个 id。回执写明"已持久化，当前会话不重注入，下一个新会话会带上"。
- 记忆不进能力指纹，改了不触发 /new 拒绝；开着的会话靠 `steve_recall` 或新会话。
- 档案页的整本保存与 agent 的逐条写走同一把锁；写前算完整快照对预算的余量，超了拒绝而不是静默截。
- 检索器返回的是数据不是指令：作为带来源与 id 的低信任段落交给 agent，不进身份、不进指纹。
- 数据治理（phase 2）：检索器配置声明所在机器与数据等级；sealed 不出主目录所在机器，restricted 不出本地部署。召回失败降级为关键词匹配并告知 agent；写入索引失败只记审计，权威已落。

#### 20.2.1 评审结论（codex，30 条）

采纳：一个权威存储 + 可选检索器，取代"一个作用域一个提供者"（5/30）；接口拆为 Store / Retriever（4）；agent 只能说 global|project，项目由绑定解析（9）；不可变 id 存在文件里（8）；项目小节改为约定 / 决策 / 坑（29）；记忆不进指纹（13）；写入回执说明可见性（14）；flock + fsync + rename，页面与工具同一入口（15）；审计先于写工具上线（17）；只读召回按需、不做每轮自动召回（7）；§20 取代 §19 的 remember，`steve_profile` 只管 USER（26）；help 同步、禁止 agent 直接改文件（27）；检索器结果为低信任数据（25）。
留到后续：所有者维度（1，单用户先不做）；`InvocationBinding` 与短期写 token（18/20，§19.4）；请求级 idempotency key 与 outbox（16/23）；导出 / 导入 / 迁移协议（6）；按模型 token 的预算（11/12）；项目记忆按角色授权群聊（19）；`internal/mcpclient` 抽层（22/24）；onboarding 起草 MEMORY 候选（28）。
不采纳：Agent 级 / 会话级长期记忆（2/3）。

### 20.3 顺序

1. ✅ `internal/memory`：作用域、Store / Retriever 接口、markdown 存储（锁、id、模板）、Service（审计、预算）；项目记忆注入；`steve_remember` / `steve_recall` / `steve_forget`；档案页项目记忆；help 更新。真机验证（2026-09-04，codex）：未绑项目时 `scope=project` 被拒并提示 `/project use`；绑定后两条项目事实落到 `<state>/memory/projects/steve.md`（带 id 注释）、全局一条落到 MEMORY.md，审计各一行；新会话第一轮拿到 `# Memory · steve` 段并能原样引用；`steve_recall scope=project` 命中。
2. MCP 检索器（hub 本机部署；`internal/mcpclient`）；配置与页面。
3. 审计落账本；InvocationBinding 后的短期写 token；导出 / 导入。

## 21. 跨机器协作 e2e（2026-09-04）

用户要的验收：@claude 一句话，claude 自动协调不同 node 上的实例与能力，完成一个项目。跑法：控制台会话绑定 `scratch`（主目录在 hub），`/use claude`，一段话交代目标与分工（node-a 写代码、node-b 写测试、hub 上的 claude 只协调）。三轮跑下来，每轮都暴露一个平台问题，修了再跑。

### 21.1 发现与修复

| # | 现象 | 原因 | 修复 |
|---|---|---|---|
| 1 | 第一轮：claude 委派后 60 秒回合被取消，子任务在 node-a 上继续跑成孤儿 | 控制台 `send` 把回合挂在 HTTP 请求的 ctx 上；驱动脚本 60 秒超时断开就取消了回合 | `console.SendCommand` 用 `context.WithoutCancel`；回合只由 `/cancel` 停（22c690d） |
| 2 | 委派到 node 的子任务拿不到平台 MCP（"offers no reverse messaging channel"） | 节点二进制落后三个提交，没有 `Advert.MCPPort`；更新后又发现定时 advert 刷新会把端口丢掉 | 节点重新部署；`advert()` 每次都带 `MCPPort`，hub 侧刷新时保留旧值（f396fc6） |
| 3 | 第二轮：claude 等子任务等到第 10 分钟整，回合被 `prompt_timeout` 砍掉 | 超时是整回合的硬上限，协调型回合光等子任务就超过 | 改成**静默超时**：每个进度事件重置时钟，只抓卡死的 agent（f396fc6） |
| 4 | 子任务落地只有 1 个路径，主目录里 `hostline/` 是空目录 | builder 在自己目录里 `git init` 并提交，快照把它当子模块（gitlink） | 快照前拍平嵌套 `.git`（hub 本地与节点脚本）；子任务提示里说明"这是 Steve 快照的工作树，别 git init / commit"（f396fc6） |
| 5 | 之后所有对该目录的改动都"无变化"、不落地 | 父树里已有的 gitlink 读进 index 后，git 忽略该路径下的文件 | 读完父树先删掉 index 里的 gitlink 条目（本地 + 脚本，脚本用真 shell 跑测试）（17d79a8） |
| 6 | 父回合死了之后，子任务的结果 `Defer` 进队列，等下一回合落地 | 设计如此（等主目录锁释放）；但 #39 因为 5 根本没有产生 artifact | 5 修后队列路径可用；孤儿子任务的落地仍要等项目上的下一回合，见 21.4 |
| 7 | 第三轮：#41 跑到 9m30s 整被取消，codex 回 `StopReasonCanceled`，父 agent 只看到 "context canceled" | `delegate.MaxWait = PromptTimeout − 30s` 是子任务的硬上限；node 上 reasoning=max 的 codex 做三个文件要 8–10 分钟 | 子任务同样改成静默超时（`internal/idle`，父回合与子任务共用），硬上限只剩任务预算 `MaxElapsed` |

### 21.2 第三轮：跑通了

修完 1–5 之后第三轮（会话 `console:e2e3`，项目 `scratch`，子项目 `fleetline`）一句话交给 claude，20 分钟后回报"项目已完成"，与事实一致：

| 步骤 | 谁 / 在哪 | 用时 | 结果 |
|---|---|---|---|
| 看有谁 | claude / hub：`steve_context` → `steve_fleet` → `steve_projects` → `steve_help` | 20 s | 挑出 node-a 的 builder、node-b 的 shipper |
| 写 main.go / go.mod / README | #41 builder / node-a | 9m30s | **失败**：hub 在 `PromptTimeout − 30s` = 570 s 处取消了子任务（发现 7，已修） |
| 重试 | #42 builder / node-a | 7m49s | 成功；3 个文件落进主目录（`LandUnder`） |
| 写 main_test.go | #43 shipper / node-b，带上 #42 的 artifact ref | 1m18s | 成功；1 个文件落进主目录 |
| 复核 | claude / hub | 15 s | `go build` 通过、`TestLine` PASS、`go run` 打出 `host=n251-239-109 time=…` |

根任务 #40 共 1188 s；父回合全程没被超时砍掉（静默超时生效）；两次落地都是子任务结束时在父回合租约下直接进主目录，claude 下一步就能看到文件。

### 21.3 观察到能用的

- `steve_fleet` 列出六个 agent 及所在机器，claude 据此挑 node-a 的 builder；`steve_delegate` 立刻返回 task_id，`steve_await` 每 50 秒一轮。
- 委派时主目录快照成 git bundle 推到 node-a，物化成隔离工作树；子任务结束后快照、绑定到 `steve/<id>/result`、在父回合的租约下 `LandUnder` 到主目录、清理工作树。
- 父回合被取消，子任务不受影响跑完（`context.WithoutCancel`）。
- claude 看到落地是空目录后自己判断"结果不对"，重新委派了一次（#39），并在目标里加了"不要 git init"。

### 21.4 未解决

- 孤儿子任务（父回合已死）的结果排队等下一回合才落地；没人再对这个项目说话就一直不落。要么给项目一个"空闲时落地"的后台步骤，要么让 `/tasks` 能手动落地。
- 节点断线时正在跑的子任务标记为 failed，工作树留在节点上（`wt-770e70f8c717-392a8c4de3ecb3f3`），没有清理。
- 节点上 builder（codex）做一个三文件的 Go 程序要 8–9 分钟；`steve_await` 一次 50 秒、每次都是一次 MCP 往返，回合的 trace 里全是 await。
- 取消的根任务在读模型里一直 `running`（多轮任务的语义），但用户看不出它其实没人在跑。
- node-a 的 codex 配的是 `model_reasoning_effort = "max"`，一个三文件的程序要 8–10 分钟；子任务的回执里没有 ACP 的 stopReason 和最后一条进度，父 agent 只看到 "context canceled"，猜不出是谁取消的。
- builder 在 hub 上复核时 `go build ./...` 留下了 2.3 MB 的 `fleetline` 二进制在主目录里；下一次快照会把它记进项目。要么主目录快照忽略常见产物，要么 claude 的复核放到临时目录。

## 22. 控制台里看见每个 agent（2026-09-04，方案）

用户看完第三轮 e2e 的对话页：工具列表 30 行里 20 行是 `steve_await`，无脑堆叠；子 agent 在 node-a / node-b 上干什么完全看不见，只有主 agent 的调用；"跟 botmux 差太多"。

### 22.1 对象

| 对象 | 是什么 | 来源 |
|---|---|---|
| 子任务卡 | 一次委派在对话里的样子：谁（agent @ node）、#id、状态、用时、目标一句话、它自己的工具调用、思考尾巴、最终回答与 refs | delegate 的进度观察者 → 读模型 `delegate.progress`（带 `step` 元数据）→ console `process.steps` |
| 位置 | 嵌在父 agent 这一轮的过程里，在它自己的工具列表之前；进行中的回合（Working）和落地后的回复（AssistantMessage）用同一张卡 | 前端 `DelegationCard` |
| 工具列表 | 连续同名的非命令调用折成一行 `调用 steve_await ×20`，可展开逐条看；超过 12 行的列表限高可滚动；标题照旧统计 | 前端 `ToolCalls` |
| 计划步骤 | 已有的 `step.progress`（`plan_id` 为真计划）不变，仍按步骤分组 | 不动 |

### 22.2 规则

- 子任务的进度走父任务的会话：事件用子任务的 `Channel`（= 父的）打标，页面按会话过滤就能收到。节流沿用每步 400 ms，但终态事件（done / failed，带回答）不节流、必发。
- 卡片里的工具调用默认折叠，进行中展开；思考只显示最后一段；回答用 Markdown，refs 原样列出。
- 一个 `process` 里子任务按出现顺序排；同一子任务多次事件覆盖同一张卡。
- 父 agent 自己的 `steve_await` 只是等待，折叠后一行；等待总时长写在那一行后面。
- 读模型的 `Event.step` 是可选元数据：`kind`（delegate / plan）、`goal`、`state`、`elapsed`、`answer`、`refs`。旧事件没有它，页面按原来的步骤分组渲染。

#### 22.2.1 评审结论（codex，15 条）

采纳：独立的 `delegate.progress` 事件而不是伪装成计划步骤（2）；终态删掉节流条目（13）；`steve_await` 的返回里 `state: failed` 按失败样式显示、不折叠（9）；父回合结束后子任务的进度不再从空生成幽灵回合（7）；页面事件缓冲用到达序号做游标，修掉 400 条后停更的 bug（12）；卡片按生命周期渲染，不以有没有工具为条件（14）；成功路径上回答与 refs 已在结果里（6 已核实）。
留到后续：统一的 `Execution{id, kind, task_id, parent_task_id, turn_id, revision, result}` 投影替代 live / stored / task / delegate 四份状态（15，5，3）；回复带稳定 `turn_id`、HTTP 返回做 upsert（11）；`ToolCall` 保留起止时间（10）；群聊 / 访客的进度脱敏（8，现在控制台只有 owner）；"紧跟在 delegate 调用之后"暂不承诺，子任务卡放在父工具列表之前（4）。

### 22.3 顺序

1. ✅ delegate 观察者 + 读模型 `DelegateProgress`（`delegate.progress`）+ console `StepProcess` 扩字段；前端 `DelegationCard`、`ToolCalls` 折叠与限高。真机验收（2026-09-04，claude 委派 node-b 的 shipper 改 README）：进行中看到"委派 #45 shipper @ node-b · 进行中 · 23s"、目标、它自己的读文件调用和当前想法；结束后卡片变"完成 · 1m58s"，19 条命令 13 次工具折叠，artifact 与落地 refs；父 agent 的 `steve_await ×2` 折成一行。第一次验收发现落地路径上子任务的回答没进结果（评审第 6 条），已修（af6e82f）。
   补：用户看后要求卡片可折叠、引用别用大号斜体——卡片改成折叠行（进行中 / 失败展开，完成折起），聊天里的 blockquote 改成正常字号、不斜体、左侧一条线（7fcde65）。
   并发验证（同日 21:54）：claude 先后发出两个 `steve_delegate`（hub 的 codex 建 CHANGELOG.md，node-b 的 shipper 建 CONTRIBUTING.md），#49 21:54:33–21:55:22、#50 21:54:55–21:55:37 重叠运行，各自的工作树、租约互不相干，先后在父回合租约下落地，两个文件都在主目录；对话里是两张并列的折叠卡，过程面板里两段各自的思考与工具。并发的边界：每台机器每个 harness 的会话槽位（advert 的 slots）；子任务共用父任务预算；改同一批文件会在落地时冲突并排队等人。
2. 会话侧栏里给运行中的子任务一个角标；`/tasks` 树与卡片互相跳转。

## 23. 改动、产物与附件（2026-09-04，方案）

用户看完对话页：发出消息后到回复前没有任何等待提示；看不到 diff、文件审查、产物审查、附件。

### 23.1 对象

| 对象 | 是什么 | 来源 |
|---|---|---|
| 一轮的改动 | 这一轮 agent 在工作区改了哪些文件、每个文件的 diff。父回合 = attempt 的 `Base` → 结束快照 `Result.Artifact`；子任务 = 它的 `Base` → 发布的 artifact | 账本里的 attempt 记录 + artifact 仓库（`git diff-tree` / `git diff`） |
| 产物 | 这一轮产生的 artifact：id、落地状态（直接改主目录 / 已落地 N 路径 / 排队 / 冲突）、绑定名 | artifact 绑定与 landing 记录 |
| 审查 | 在对话里按文件看 diff，能折叠、能复制；不是编辑器 | 前端 `ChangesFold` + diff 视图 |
| 附件 | 用户发给 agent 的文件（图片、文本）与 agent 交回的非文本产物 | phase 2：blob 存储 + 消息里的引用 |
| 等待提示 | 发出去到第一个进度事件之间，对话里立即出现"进行中"的占位（本地状态，不等 SSE） | 前端 |

### 23.2 规则

- 回复带 `changes`：`{attempt, project, base, artifact, paths, landing}`；`paths` 随回复存下来，diff 按需拉（`GET /console/diff?project=&from=&to=`），限 200 KB，超出按文件截断并注明。
- 子任务卡同样带 `changes`（Child 的 `Base` / `Artifact`），落地状态来自它的 refs。
- 二进制文件只列路径与大小，不出 diff；删除的文件标 `D`，新增标 `A`。
- diff 是主目录快照之间的差异，与 agent 是否 `git commit` 无关；主目录本身是 git 仓库时也一样（我们的快照仓库独立）。
- 等待提示：发送即在本地把 `live` 置为"已发出"，SSE 的 `console.sent` 到了再补时间；回复到了清掉。

#### 23.2.1 评审结论（codex，15 条）

采纳：diff 端点按 attempt 走（`/console/attempts/{id}/changes`，服务端从 attempt 取 project / base / artifact，校验 SHA，不接受任意三元组）（7）；改动索引用 `--name-status -z` / `--numstat -z`，关掉 rename 检测，只给 A / M / D 与大小（9）；先给有界索引再按文件拉 diff，总字节、单文件、文件数、超时都在读子进程输出时就限住，`--no-ext-diff --no-textconv`（8）；回复只存 attempt id 与计数，路径与 diff 按需读（4）；无改动时 `Result.Artifact` 为空，端点如实说"没有改动"（2）；owner-only、`Cache-Control: no-store`，sealed 且主目录不在 hub 的项目拒绝网页 diff（5，6）；父回合的 diff 标为"本轮净改动"，含已落地的子任务（11）；**快照不得修改用户目录**：拍平只对平台自己的工作树，用户目录里的嵌套仓库按 pathspec 排除、不删（14）。
留到后续：显式 `finalizeAttempt` 与 `capture_error`（1）；失败回合也做结束快照（2）；`Exchange{id}` 贯穿 sent / progress / reply / HTTP（3，15）；子任务 base 的语义写清是"委派时的主目录快照"（10）；landing 状态从账本投影而不是 refs 字符串、deferred landing 独立 worker（12）；大工作树的快照成本与上限（13）。

**事故记录（2026-09-04）**：第 14 条评审时已经发生——拍平逻辑跑在 scratch 主目录的回合后快照上，删掉了用户目录 `steve-work/steve-self/.git`（steve-self 项目的主目录，一个 steve 仓库的克隆）。已按 917475f 恢复 `.git`（`git clone -n` 后 `git reset`，工作文件一字未动，37 处未提交改动保留为工作树差异）；node-a 上的副本没受影响。修复：`Repo.Snapshot(…, flatten)` 与 `Script.Snapshot(…, flatten)`，只有 `KindWorktree` 传 true；其余路径用 `:(exclude)` 排除嵌套仓库，测试覆盖本地与脚本两条路径、含无提交的嵌套仓库。

### 23.3 顺序

1. ✅ `Repo.Changes` / `FileDiff` + `Store.Changes` / `FileDiff`（按 attempt，`/console/attempts/{id}/changes` 与 `/diff?path=`，有界、no-store、sealed 远端拒绝）；`turn.Result.Attempt`；console 记录回复时经 `Inspector` 取 `changes`；`delegate.Child.Attempt` → 子任务卡的 `attempt` / `files`；前端 `ChangesFold`（文件列表 A/M/D、+/−、按文件展开 diff）。真机验收（2026-09-04 22:26，claude 自己改 CHANGELOG.md 并委派 node-b 的 shipper 改 CONTRIBUTING.md）：回复显示"改动了 2 个文件 · 本轮净改动，含已落地的子任务"，展开 CHANGELOG.md 看到 `+0.1.1：补文档`；子任务卡显示"改动了 1 个文件"。等待占位：核实 3 秒截图已有"正在放置… · 3s"与右侧"进行中"面板，此前看不到是事件游标在 400 条后失效（§22 已修），未另加代码。同批修掉：hub 启动时先拨节点后建 gate，反向 MCP 通道从未在启动连接上服务（节点回环端口接了不答，子任务拿不到平台工具）——`SetMCPDialer` 对已在线连接补起服务；hub 本机子任务卡带上机器名。
   补（2026-09-05）：左侧菜单栏可收成图标栏（56 px）、会话栏可收成细条（40 px），状态记在 localStorage；关系页点开的任务抽屉与右侧栏同宽（380 px），页面右边不再跳。聊天正文字号统一为 14 px（`.prose.md` 显式 `text-sm`，此前继承 16 px，比用户气泡大）；关系页的任务 / 委派行可点开任务抽屉（`TaskDrawer` 从看板页抽成共享组件）。
2. 附件：上传到 blob、消息里引用、agent 侧作为图片/文件送入；agent 交回的非文本产物在卡片里可下载。

## 24. 工作台：线程与它的活（2026-09-05，已评审）

用户拿另一个产品的看板做对照（任务卡片下挂会话、会话详情有"文件 / 变更"tab、跨会话批注引用、发送框选模型与推理档位、预览模式），并说"我们的控制台也是工作台来的"。第一版方案写的是"以任务为一级对象、一条会话对应一个任务"，核对数据后不成立：任务按 `(Channel, Member, Origin)` 开闭，一条消息一个任务，一条飞书会话里已有 11 个聊天任务。codex 评审 22 条（24.2.1）后改成下面这个模型。

### 24.1 模型

```
线程（ChannelThread）1 ── N 根任务（Task）
                          ├── N 子任务（委派）
                          ├── 0..1 计划 ── N 步骤
                          └── N attempt（账本；每个有 Base / Result.Artifact）
Agent 会话（AgentSession）按 (线程, agent) 独立管理，与任务生命周期无关
```

| 对象 | 是什么 | 在工作台里 |
|---|---|---|
| 线程 | 一次"这件事"的对话：控制台会话（有 transcript、agent 起的标题、归档）或飞书会话 / 话题（只有锚点，没有 transcript） | 左栏一级条目，按项目分组；飞书线程显示锚点与"在飞书打开" |
| 任务 | 线程里被跟踪的一段活：编号、目标、四轴状态（Lifecycle / Execution / Attention / Lane）、预算、父子、计划 | 线程下的一行，只在"值得看"时出现：在跑、待你处理、有子任务或计划；一句话的聊天任务折进线程不单列 |
| 任务元数据 | 标题、优先级、标签、归档时间、排序 | 新增 `TaskMeta`，与状态机分开；标题缺省回退到目标 |
| 子任务 / 步骤 | 委派与计划步骤，各有 agent、机器、过程、回答、改动 | 任务下缩进一层；选中时中栏显示它的过程与回答 |
| attempt | 一次执行：前后快照、机器、工作区、结果 | 变更 / 文件 tab 的单位 |
| 文件 | **所选 attempt 的结果快照**（没有结果就是它的 Base）的目录与文件，不是"此刻的磁盘" | 右栏 tab；逐层目录、有界内容 |
| 变更 | 任务所有 attempt 的改动，最新在前；每个 attempt 复用 §23 的索引与 diff | 右栏 tab；回复下的折叠行保留 |
| 引用 | 别的回复的一段，作为这条消息的资料：稳定 `ReplyID` + 范围，服务端取原文 | 发送框里的引用块 |
| 偏好 | 模型、推理档位等 harness 暴露的选择项，按 (线程, agent) 记 desired / applied | 发送框可选；改了做会话轮换，不动任务 |

### 24.2 页面

- **工作台**（`/console`）三栏不变。左栏是线程树：项目 → 线程（标题、agent 图标串、在跑转圈、待你处理角标、相对时间）→ 值得看的任务 → 子任务 / 步骤。过滤：在跑 / 待你处理 / 已归档；搜索。顶部切换 **列表 / 看板**：看板按 Lane 分列，卡片是根任务；第一版不做拖拽，优先级与归档走菜单。任务页从导航去掉，`/tasks` 路由重定向到 `/console?view=board&task=<id>`。
- **中栏**是选中线程的 transcript；选中子任务或步骤时显示它的过程与回答。
- **右栏**tab：详情（给 agent 的、项目、agent、谁能接、调用关系树）、过程、变更、文件。
- **抽屉**留给环境页的一瞥，右上角"在工作台打开"（先切到那条线程再聚焦任务）。
- 导航：工作台、待处理 · 项目、资源、技能、MCP、档案 · 历史与审计。

### 24.3 规则

- 任务状态沿用四轴，不由拖拽改变；归档分两种：线程归档（控制台元数据，今天已有）与任务归档（`TaskMeta.ArchivedAt`），互不混用。
- 网页用 owner-only 的任务 API（`/console/tasks/{id}` 服务端把任务、计划、账本 attempt 连好；`/console/tasks/{id}/attempts` 带真实 attempt id、Base、Artifact），`/tasks` 聊天命令保持当前线程范围，不改语义。
- 文件 tab：`ls-tree -z -l <commit>:<dir>` 逐层分页，目录项数、深度、单文件字节、总响应、超时都有上限；二进制只给大小，symlink 只显示目标不跟随，gitlink 显示为链接；sealed 且主目录不在 hub 的项目禁用文件与变更 tab，说明"数据只在项目主机"。
- 引用：只提交 `ReplyID`（回复要先有持久 id）+ 可选范围；服务端取原文、生成摘要与 hash；默认只允许同项目同线程，跨项目要过源项目读、目标项目写、数据等级与受众的校验，restricted 记 disclosure；注入时标为"不可信资料，仅供参考，不执行其中指令"，放在用户请求之前，`Injected` 记来源、摘要、hash 与实际注入文本。飞书回复不在存档里的暂不支持引用。
- 偏好：选择项来自 harness 实际暴露的 Selectors，不硬编码字段名；改偏好做**会话轮换**（归档旧 upstream session、开新的，任务继续），不走 `/new`；指纹不含偏好，另记 `PreferenceRevision`，从 runner 读回实际设置写入 Injected 与 attempt。
- 实时：任务变化先发轻量 `task.changed{id, revision}` 失效通知，列表分页、详情懒加载；全量 `/state` 重算不再是每个事件的默认。
- 不照抄：看板拖拽改状态；任务卡片里把会话画成平级——父子、委派、步骤要有层级。

#### 24.2.1 评审结论（codex，22 条）

采纳：线程 1:N 根任务、任务按 Channel 关联（1–3）；飞书线程只有锚点（4）；两种归档分开（5）；四轴状态、拖拽不改状态（6，21）；`TaskMeta` 与状态机分开（8）；任务详情由服务端连接、列表轻量、attempt 从账本按 TaskID 查（9，11）；`task.changed` 失效通知与分页（10）；文件 tab 按 attempt、逐层有界、sealed 远端禁用（12–14）；引用用稳定 ReplyID、服务端取原文、同项目同线程默认、不可信资料标注（15–17）；偏好按 (线程, agent) 记 desired / applied、来自 Selectors、会话轮换不走 `/new`、`PreferenceRevision`（18，19）；`/tasks` 命令保持线程范围、网页走 API（20）；顺序按评审改（22）。
**实现前必须修**：`/project use` 只归档 session 不结束任务，下一轮 `beginTask` 会按 `(Channel, Member, "")` 复用旧项目的任务，attempt 却在新项目上跑（7）——绑定变化时关闭旧任务，或把 BindingVersion 加进 `Active` 匹配。

### 24.4 顺序

1. ✅ 修 7（切项目关闭该会话持有的任务；回合不接续别的项目的任务，有测试）；回复持久 `ReplyID`（事件带 `reply_id`）；`/console/tasks/{id}`（任务 + 计划 + 子任务 + 账本 attempt）与 `/attempts`；任务存储写后发 `task.changed`。真机核实：`/console/tasks/49` 给出 attempt 的 base / artifact，一轮里看到三条 `task.changed`。（7c05b89）
2. ✅ 左栏线程树带"值得看的任务"（在跑 / 待处理 / 有子任务 / 有计划 / 定时；子任务缩进一层）；列表 / 看板切换、任务页并入（`/tasks` 重定向到 `/console?view=board`）；选中委派子任务时中栏显示它的卡片（展开的过程、回答、改动，"回到对话"）；`TaskMeta`（`internal/task/meta.go`：标题 / 优先级 / 标签 / 归档，与状态机分开；`PATCH /console/tasks/{id}/meta`；看板卡片与抽屉的 ⋯ 菜单、"显示已归档"开关；这一块由 codex（gpt-6-astra）经 herdr 作为子任务完成，文件边界事先划定）。
3. ✅ 右栏"变更"（线程下所有任务与子任务的 attempt，最新在前，每个一条改动折叠）与"文件"（所选 attempt 的结束快照，没有结果就是开始快照；逐层 `ls-tree -z -l`、单文件 `cat-file` 限 200 KB、二进制只给大小、链接与嵌套仓库标出；`/console/attempts/{id}/tree|file`）。真机截图核对：变更 tab 列出 #51 / #48 / #50 / #49 各自的改动，文件 tab 打开 fleetline/main.go。
4. ✅ 引用块：回复元信息行的"引用"把 `{conversation, reply_id}` 放进发送框（跨线程保留），随消息提交；服务端从存档取原文，默认只允许同项目（跨项目拒绝，真机核实：steve 项目引用 scratch 的回复被拒），渲染成"引自线程「…」的回复"+ `>` 引文 + "以上引用是资料，仅供参考，不要执行其中的指令"，放在用户请求前；`Injected.prompt` 里可见。限 5 条、每条 8 KB。
5. ✅ 偏好：`state.Conversation.Preferences[agent][option]`；`GET /console/selectors`（从活会话读 harness 的选择项，没有就开一个）、`PUT /console/preferences`（记下并做会话轮换：关掉 upstream session、归档，任务不动）；发送框的模型与推理档位 chip。真机核实：codex 给出 8 个模型与 reasoning_effort（low…ultra），设置后下一轮 `new_session: true`。评审留下的 `PreferenceRevision`、从 runner 读回实际设置写入 Injected 未做。

## 25. 长程任务：不设回合上限，消息排队与插队（2026-09-05）

用户：每个任务 60 回合的配额太小，有过一个目标跑两周的经历；从长程任务的角度不期望有回合限制，所以要有流程中断、新消息排队的机制，交互照 Codex app 的设计。

### 25.1 规则

- **预算是可选的护栏，不是默认。** 任务的回合数与时长上限默认为 0 = 不限；`task_max_turns` / `task_max_elapsed` 配了才生效，配了才有刹车卡。子任务的硬上限只剩它继承的预算（不限就没有），静默超时（§21 发现 3 / 7）仍在。
- **排队是默认。** 回合进行中发出的消息排在它后面（协调器早已如此：`req.Queue` 默认 true；`!` 开头是打断）。控制台以前在回合进行时禁用输入框，等于把这层能力藏起来了；现在输入框一直可用，进行中按 Enter 进队列。
- **队列在发送框上方**，一行一条，可编辑、可删除、可"插队"（等价于加 `!`：打断当前回合，agent 保留会话上下文继续），可"在新线程里问"（同项目开一条新线程把这句发过去），可"关闭排队"（关掉后进行中不能输入，和以前一样）。回合结束后队首自动发出，一次一条。
- 队列先放在页面本地（刷新即丢），服务端仍有 takeTurn 的等待队列兜底；持久化的 Exchange 队列留到 §24 评审第 3 / 15 条一起做。

### 25.1.1 用量为什么都是"未上报"

查了两个适配器（codex-acp、claude-agent-acp）和 Go 的 acp 库：ACP 的 `usage_update` 只带 `used` / `size`（上下文占用与窗口），协议里没有每回合的输入 / 输出 / 缓存 token 字段；codex-acp 内部有 `thread/tokenUsage/updated` 的完整计数，但发到 ACP 时只折成 `used`。所以是**协议层本身不上报**，不是框架没适配。已改：有上下文占用时显示"上下文 17.3k"而不是"未上报"；要拿到真实 token 数，得给适配器加 vendor `_meta`（或直连 codex app-server），留作后续。

### 25.2 顺序

1. ✅ 默认不限预算（`DefaultMaxTurns/MaxElapsed = 0`，e2e 配置也去掉了 60 回合 / 6 小时；界面上无上限时只显示已用回合数）；发送框排队、插队、编辑、删除、新线程、关闭排队。真机核实：回合进行中第二条消息出现在发送框上方，带"插队"与菜单，输入框提示"先排着"。
2. 服务端持久队列（Exchange），刷新不丢、多标签页一致；进行中的回合被"插队"时把已排队的其余消息保留。

## 26. 阶段小结（2026-09-05）

从 §13 到 §25 这一段（分支 `p1/transport-seam`，一百个提交）合入 master 时的状态。

**落地了的**：控制台组件化与 Codex 式 transcript（§13）；项目 / 工作区模型与副本放置（§14）；会话标题与归档（§15）；技能页、档案页、内置技能与来源、机器技能缓存（§16–17）；MCP 页与市场、平台自己的 MCP 与钩子（§18–19，含 fleet 工具）；记忆子系统：权威 markdown + 检索器接口、全局 / 项目两层、工具与审计（§20）；跨机器协作 e2e 三轮跑通，修掉七个平台问题（§21）；子 agent 在对话里实时可见、工具列表折叠（§22）；改动与产物：attempt 级索引与 diff、文件浏览（§23）；工作台：线程 1:N 任务、看板并入、变更 / 文件 tab、任务元数据、引用块、偏好与会话轮换（§24）；长程任务：默认不限预算、发送框排队与插队（§25）。

**留着的**（各节末尾有明细）：统一的 Execution 投影与持久回复 / 交换 id（§22.2.1、§23.2.1、§24.2.1）；服务端持久队列（§25.2）；MCP 检索器与 `internal/mcpclient`（§20.3）；InvocationBinding 与短期写 token、事件总线与钩子（§19.4）；附件（§23.3）；孤儿子任务的落地与断线残留工作树（§21.4）；每回合 token 用量——协议与适配器都有，是我们的 acp 库没解 `PromptResponse.usage`（§25.1.1），已作为独立任务交给 codex。

## 27. 稳定性治理（2026-09-05）

用户要做项目治理、把稳定性搞好。先修已经发现的问题，能并发的并发：

| # | 问题 | 修法 | 谁 |
|---|---|---|---|
| 1 | 每次真机跑都撞出平台 bug，没有常态化回归 | `e2e/fleet/` 一键脚本：绑定 scratch、委派 node-b 改一行文件、断言落地 / 卡片 / 改动索引 / 用量已上报；`make e2e-fleet`；合入前必跑写进 CONTRIBUTING | codex（worktree `steve-gate`） |
| 2 | 父回合死后子任务结果排队没人落地；节点断线留下工作树没人清 | hub 后台清扫：有排队落地的项目在主目录锁空闲时 `LandPending`；节点上线后清掉没有活 attempt 的 `worktrees/wt-*` | 我（`fix/orphans`） |
| 3 | 发送框队列只在页面本地，刷新即丢；回复 / 交换没有贯穿的 id | 队列持久化到 console 文档，带 `ExchangeID`：入队 / 删除 / 插队走 API，页面只是投影；`console.sent` / `reply` 事件带同一个 id | codex（worktree `steve-queue`） |
| 4 | 写入保障薄：记忆写入没有请求级幂等键；快照对大工作树没有上限 | `steve_remember` 加 `idempotency_key`，审计记 key→结果、重放返回同一回执；快照限制文件数 / 单文件字节 / 总字节，超出拒绝并说明 | codex（worktree `steve-guard`） |
| 5 | steve 仓库没有合入规则（私有仓库无 ruleset），和 acp 不一致 | GitHub 的分支规则要 Pro；先在 CONTRIBUTING 写清：PR、CI 绿、真机门禁、merge commit；acp 那边保持 squash + 线性 | 文档（并入 1） |

§19.4 的 InvocationBinding 与短期写 token、评审多次提的统一 Execution 投影，工作量属于下一阶段，这轮不做。

### 27.1 落地记录

| # | 状态 | 在哪 |
|---|---|---|
| 1 | ✅ `e2e/fleet`（标准库 Go 客户端）+ `make e2e-fleet` + CONTRIBUTING；首轮真机就抓到了下面的预算 bug，修复部署后 `FLEET PASS elapsed=1m54s task=#57`（claude 委派 node-b 的 shipper 建文件，卡片 / 改动索引 / 用量 / 主目录文件五处一致） | PR #6（codex） |
| 2 | ✅ `artifact.Store.SweepWorktrees` + hub 的 `sweepWorktrees`（启动时清本机、节点上线时清该节点）、`sweepLandings`（每 30 秒 `LandPending`，锁被占就跳过）。上线当场清掉了 node-a、node-b 各一个前几轮留下的工作树 | PR #5 |
| 3 | ✅ 交换持久化到 console 文档，`Exchange{ID, State, ReplyID}`；入队 / 删除 / 编辑 / 插队走 `/console/queue` API；重启时正在跑的交换记为失败、排队的按序续跑 | PR #8（codex） |
| 4 | ✅ `steve_remember` 的 `idempotency_key`（24 小时、同作用域、跨进程）；`artifact.Limits`（2 万文件 / 2 GB / 单文件 200 MB），本地与节点脚本同一套检查，超出返回 `TooLarge`，回合后快照失败的原因进 `changes.note` | PR #7（codex） |
| 5 | ✅ 并入 CONTRIBUTING：PR、CI 绿、涉及委派 / 落地 / 超时 / 快照 / 节点通道必跑门禁并贴输出；steve merge commit，acp squash + 线性 | PR #6 |

验证第 2 条时顺手撞出来、当天修掉的三个：

| 问题 | 现象 | 修法 | 在哪 |
|---|---|---|---|
| 重启后的幽灵 attempt | hub 在租约期内重启，旧 attempt 仍"活着"并持有项目锁；续跑的任务被自己的幽灵拒绝："项目 home 正在被 claude（任务 #55）修改" | 启动时 `attempts.ExpireAll("hub restarted")`：不看租约、全部过期并切断租约；`Sweep` 只管租约到期的，留给周期清扫 | PR #9 |
| 控制台任务重启后续不上 | 中断的任务一律走飞书通道回复锚点续跑，控制台的锚点是 `web-…`，飞书拒收，任务就此停住 | `console.Service.Resume`：先 `ReviveSession`，再把一条"继续"交换**插到队列里排着的消息前面**——页面上显示的是"⟳ 网关重启，继续任务 #N"，agent 收到的是 `@member` + 续跑提示（`Exchange.Prompt` 与 `Input` 分离）；`Persist` 只装载，`Drain` 在续跑入列之后再放行队列；`/tasks resume` 对控制台任务同路 | PR #10 |
| 无上限的父任务不能委派 | 预算改成可选（0 = 无上限）后，`Spawn` 仍用"父上限 − 已花"算子任务上限，0 减任何数都是负，所有根任务的委派都被拒："task 54 has no budget left to delegate"。跨机器委派从 996295a 起在 master 上一直是坏的，门禁第一次跑就抓到了 | 父任务没有上限就不往下传上限；有上限的照旧封顶、花完拒绝 | PR #11 |

另两处顺手修的：#8 新加的"排队已打开"chip 在 1440 宽下逐字换行（PR #12）；任务通知（跑了多久、结果送到哪）一律落到 main 会话，现在 `TaskNotice.Conversation` 带着任务自己的会话，落到那里（PR #13）。

真机验证（hub d6f2580）：在页面上让 claude 前台跑 50 秒的循环，30 秒时杀掉 hub 重启——日志依次是 `expired attempt of the previous process … hub restarted`、`console: resuming task #55`，页面上先是旧交换的"console restarted before this exchange completed"，然后 "⟳ 网关重启，继续任务 #55" 一行，1 分钟后 agent 的回复和"任务 #55 跑了 1m0s"的通知落在同一条交换上。

还留着的：重启时 ACP 子进程一并死掉，agent 靠 session/load 重放历史续跑，长回合里 agent 自己起的后台命令会丢；`Exchange.Prompt` 目前只有续跑在用。
