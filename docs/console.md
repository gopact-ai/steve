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
