# 跨 agent / 跨 node 协作的底座审视（2026-09-03）

目的：真正投入工作前，把"信息缺失或不同步导致协作出非预期结果"的可能逐项过一遍。方法：按信息类别对照代码，问四个问题——权威在哪、怎么到达 node、链路断了或进程重启后处于什么状态、页面/日志能否看见。结论先行：**执行与收敛的骨架（租约、快照、落地、恢复）是可靠的；不可靠的是"agent 看到的东西"这一层——技能、MCP 工具、记忆、工具主目录在 hub 与 node 之间没有同步，而且没有任何地方记录一次会话实际拿到了什么。**

## 1. 信息清单：什么在哪、怎么动、断了怎样

| 信息 | 权威 | 到 node 的方式 | 断链 / 重启后 | 判断 |
|---|---|---|---|---|
| 身份与画像（SOUL / USER / MEMORY） | hub `~/.steve/home` | 每次开会话由 `capability.Assembler` 拼进首条 prompt | 会话丢了就重新拼；node 不持久化 | ✓ 机制对；⚠ 文本不留档，只留指纹（§3-9） |
| 记忆的写入 | hub 文件 | 无：默认权限拒写，agent 只能把建议写在回复里；远端 agent 根本摸不到文件 | — | ✗ 记忆只能靠人手贴，跨 node 更新路径不存在（§3-3） |
| skills | hub `skills.Map` + 每个 AI 工具的独立 home | **没有**：`skills.Live` 只在 hub 物化；steve-node 不调用 `runtime.Prepare` | — | ✗ 远端 agent 拿不到 skills，但指纹里含 skills（§3-1） |
| MCP 服务器 | hub 配置 `mcp_servers{}` | 随 `session/new` 传给 node 上的 agent 进程，由它在 node 上启动 | — | ✗ 命令存在性在 hub 的 PATH 上查（`assembler.go:155`），实际在 node 上跑（§3-2） |
| AI 工具自身的 home（~/.codex、~/.claude：全局配置、个人 MCP、个人 skills） | 各机器用户目录 | hub 上用隔离 home；**node 上直接用用户真实目录** | — | ✗ 行为与安全双重漂移（§3-4） |
| AGENTS.md / CLAUDE.md | 项目目录 | inplace 同目录；isolated 随快照进 worktree（须已提交） | 随仓库 | ✓（是 AI 工具的约定，Steve 不解析） |
| 任务、计划、步骤上下文（ctxpack） | hub ledger | 步骤上下文拼进 prompt，随计划修订持久化 | 计划可从 checkpoint 恢复 | ✓ |
| 项目文件（isolated） | hub 影子裸库 + 产物 | bundle 经 hub 或节点直连 | 副本按节点代数标记，代数变了视为可疑 | ✓ |
| 项目文件（inplace） | 项目主机主目录 | 不动 | 会话中途被杀，主目录可能半改；只有 git 能救 | ⚠（§3-6） |
| ACP 会话 | node 上的进程 | hub 持会话状态 | **hub 链路一断，node 就 kill 进程**（`serve.go` runAgent stop） | ⚠ 设计如此（任务迁移、会话不迁移），但后果没有被记录和收尾（§3-5） |
| 租约 / Attempt | hub ledger，用 hub 时钟 | 心跳在 hub 侧 | 断链 → 心跳失败 → 租约过期 → 步骤取消 / 接管 | ✓ node 时钟无关 |
| 落地（landing） | hub，六阶段 + WAL | — | 启动时 `RecoverLandings` | ✓ |
| 未知效果、披露 | hub ledger | — | 待处理页 | ✓ |
| 过程（推理、工具调用） | 内存事件流 | — | 只有控制台会话把过程存进转录；飞书任务、计划步骤**什么都不留** | ✗（§3-7） |
| node 日志 | node 本地文件 | 无 | 无 | ✗ 无关联 id，无法与 hub 事件对齐（§3-8） |
| node 健康（磁盘、负载） | 无 | 无 | 无 | ✗（§3-10） |
| 版本与协议 | `ProtocolVersion` 整数；`build_version` 只展示 | 握手校验协议号 | — | ⚠ 无兼容策略（§3-11） |
| hub 身份 | node 只认 token | 任何持 token 的 hub 都能连 | 两个 hub 同时连一个 node 不会被拒 | ⚠ 脑裂风险（§3-12） |

## 2. 原则：什么该共享、什么不该

- **该由 hub 单点拥有并下发的**：身份、画像、记忆、技能、任务上下文、放置决策、租约、时钟。原则是"node 无状态、可替换"；这一条现在只做了一半：下发的是 prompt 文本，技能与工具的**物化**没有随行。
- **该留在 node 本地、只申报不共享的**：AI 工具二进制与版本、机器能力、工作区路径、凭据与环境变量。这些用申报（advert）表达，hub 按申报放置。现状正确。
- **不该共享的**：agent 之间的对话历史、推理、工具输出。跨 agent 只传结构化结果（refs / findings / outcome）。现状正确，且应保持。
- **随项目走的**：仓库里的一切，包括 AGENTS.md。isolated 项目里"没提交的就不存在"，这一点要写进用户文档。

## 3. 发现（按严重度）

**3-1 高｜skills 不到 node。** `skills.Live` 只在 `cmd/steve` 里构造并物化到 hub 的工具 home；`steve-node` 不物化。远端 agent 的会话指纹却包含 skills 指纹，页面和日志都会说"带了这些技能"。后果：同一个计划，hub 上的步骤会用技能，node 上的不会，产物风格和质量分叉，且没人能从记录里看出原因。

**3-2 高｜MCP 服务器在错的机器上校验。** `capability/assembler.go:155` 用 hub 的 PATH 判断命令是否存在，然后把配置发给 node 上的 agent 进程执行。hub 有、node 没有的工具：会话启动时失败或静默降级；hub 没有、node 有的：被 hub 提前拒绝。

**3-3 高｜记忆没有写入路径。** MEMORY.md 在 hub 文件系统，默认工具权限拒写，模板里让 agent"把建议写在回复里等主人贴"。远端 agent 连文件都看不到。跨机器长期协作时，agent 学到的东西不会沉淀，或者只沉淀在某一台机器的会话里。

**3-4 高｜node 上的 agent 用用户真实的工具 home。** hub 用 `runtime.Prepare` 给每个 AI 工具建独立 home（自己的 skills、配置），node 没有这一步，`runAgent` 直接以节点配置的 ProcessDir/Env 起进程，于是拿到那台机器用户自己的 `~/.codex`、`~/.claude`：个人 MCP 服务器（带凭据）、个人 skills、全局指令都会混进 hub 派下去的任务。这既是行为漂移，也是权限边界问题。

**3-5 中｜hub 链路一断，node 立刻 kill 所有会话，且没有收尾记录。** 设计上"任务迁移、会话不迁移"是对的，但现状：hub 重启或网络抖动 → node 侧进程被杀 → 步骤的租约随后过期被取消或接管；**聊天任务**则停在 running、没有 live Attempt，永远不结束（任务页里那一列"待继续"的旧任务就是这么来的）。启动时应把"随上一个进程消失的 Attempt"显式关闭并记入历史。

**3-6 中｜inplace 项目中途被杀会留下半改的主目录。** 回合前后有快照用于 diff 与产物，但主目录本身不是事务性的。若主目录是 git 仓库可以手工恢复；不是则无解。至少要在页面上把"该任务的会话异常结束、主目录可能有未完成修改"说出来。

**3-7 高｜过程不留档。** 只有控制台会话把推理与工具调用存进转录；飞书发起的任务、计划的每一步、delegate 的子任务，跑完只剩回答与产物。跨 node 出了问题时无法回答"那台机器上的 agent 当时做了什么"。评审也指出：过程记录需要自己的数据等级与保留策略，而不是塞进全局历史。

**3-8 中｜node 日志与 hub 事件对不上。** node 日志 17 条 printf，没有 attempt / session / step id；hub 侧事件有 operation id。排障要靠时间戳肉眼对齐。

**3-9 中｜会话实际拿到的上下文不留档。** 只存指纹。要回答"这一轮 agent 看到了什么画像、哪些技能、哪些 MCP"，只能按当时的配置重算，而配置可能已经变了。

**3-10 中｜node 没有健康申报。** 磁盘、负载、inode 都不报；worktree 目录 `wt-<base>-<attempt>` 在发布时删除，但 attempt 死在中途（进程被杀、hub 崩）就留在 node 上，没有清扫。一台 node 会被慢慢填满，而 hub 不知道。

**3-11 低｜没有版本兼容策略。** 握手只比协议号；一个旧 node 遇到新 hub，多出的流类型（如 advert 刷新）会失败但不会被识别为"版本太旧"。现在 `build_version` 已经申报，缺的是一条"最低要求"和页面提示。

**3-12 低｜node 不认 hub。** 持 token 者皆可连；两个 hub（比如一个开发、一个真用）指向同一 node 时互不知情，各自派活，租约与账本在各自 hub 里各成一套。node 应记住首次连接的 hub 身份（或 incarnation），拒绝第二个并发 hub，换 hub 需要显式动作。

**3-13 低｜过程记录的数据等级。** 控制台转录里的工具输出可能含 sealed 项目内容，存在 hub 账本文档里。控制台只对 owner 开放，暂可接受，但应标等级并有保留期，避免将来接多人时直接泄露。

## 4. 建议的最少改动集合（按先后）

1. **node 侧工具 home 隔离 + skills / MCP 随行**：steve-node 启动时 `runtime.Prepare(state_dir)`；hub 按 skills 指纹把启用技能以内容寻址推到每个 node（复用 blob 流），node 物化到各工具 home；MCP 命令存在性由 node 在申报里回报（advert.Harnesses 旁增 `mcp_servers[]` 可用性），hub 放置时按申报判断。这一组解决 3-1、3-2、3-4。
2. **会话上下文留档**：每个 Attempt 记录它拿到的上下文摘要（画像文件哈希、技能列表与指纹、MCP 列表、ctxpack 摘要），写在 Attempt 记录里；页面右栏"注入"标签读它。解决 3-9。
3. **过程留档**：把 `console.progress` / `step.progress` 的终态汇总（工具调用列表、推理尾巴、答案）按 Attempt 存一份，带数据等级与保留期；控制台、飞书、计划步骤共用同一份。解决 3-7、3-13。
4. **失联收尾**：hub 启动与 node 断连时，把没有 live 进程的 Attempt 显式标为 `expired`，聊天任务标为"会话中断，主目录可能有未完成修改"，进历史与待处理。解决 3-5、3-6 的可见性。
5. **记忆写入路径**：给 agent 一个 `steve_remember` 工具（写入 hub 的 MEMORY.md，经 owner 策略允许），远端 agent 走反向 MCP 通道，不再依赖文件权限。解决 3-3。
6. **node 健康与清扫**：申报里加磁盘剩余、负载、worktree 数；node 定期清扫无主 worktree（attempt 已终态或不存在）。解决 3-10。
7. **日志关联与版本策略**：node 日志带 attempt / session id；握手比 `build_version` 的最低要求，页面标红不兼容的机器；node 记住 hub 身份，拒绝并发的第二个 hub。解决 3-8、3-11、3-12。

前三项是"信息层"的根，做完之后才谈得上跨 node 协作可预期；4–7 是运维与排障，投入工作前也应到位。
