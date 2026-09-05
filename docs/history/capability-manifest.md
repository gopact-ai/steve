> 记录，非现状；现状见 [docs/architecture.md](../architecture.md)。本文保留当时的方案、审计和验证记录，其中的命令、默认值与实施状态不作为当前使用指南。

# 能力清单与跨节点感知：长期方案（第 3 版）与当前实施边界

日期：2026-09-03。两轮 codex（gpt-5.6-sol，reasoning max）评审：第 1 轮 20 条、第 2 轮 18 条新发现。本文分两部分：**第一部分是生产终态**（吸收两轮评审，作为长期方案），**第二部分是本轮已落地的切片与明确的门禁**。原则不变：异构是常态；要保证的是感知、匹配、准入、留证；清单只参与调度，不授予权限；秘密不出机器。

---

## 第一部分：生产终态

### 1. 三层快照与主体

- **Offer 快照（node 报）**：`Snapshot{schema, node, revision=(incarnation, sequence), generated_at, received_at, content_digest, coverage[kind], offers[], features[]}`。`incarnation` 是每次进程启动的随机 128 位值（不依赖持久化计数）；`sequence` 单调；`content_digest` 只哈希内容，不含时间。乱序、重放按 revision 拒绝；超限（≤200 项、≤64KB、id ≤64、detail ≤200、attrs ≤16）**整份拒绝**并标 `snapshot_rejected`，保留上一份并按 TTL 过期。
- **Offer 主体**：键不是 `(kind,id,harness名)`，而是 `(kind, id, SubjectRef)`，`SubjectRef{node, harness, runtime_profile_digest}`；model / credential / mcp 再带 `authority|provider|principal`。同一 harness 的不同 API base、认证主体、隔离 home 是不同主体。
- **证据与可用性**：Capability 只携带 `configured`（是否期望存在）与 `evidence[]{kind: declared|observed|derived, method, result, ok, at, probe_epoch, valid_for}`；**可用性不由 node 自报**，由 hub 的版本化 reducer 从 typed evidence 与信任策略计算。assurance 分 `existence | launchable | functional`，各 kind 有最低等级：harness / tool 至少 launchable；mcp 至少 launchable（受限超时内 initialize）；skill 区分 `materialized | configured | loaded`；hardware 用 typed attrs（`cpu.count:int`、`mem.gb:int`、`gpu.count:int`）；network 指向具体服务的连通性探测；credential 只报非秘密的 `principal / scopes / expires_at / refreshable`，过期即不可用。**声明本身不满足任何生产硬要求**，只有 legacy tag 例外并显式标 `declared_ok`。
- **容量不在清单里**：`NodeHealth/Allocatable`（磁盘、负载、显存、槽位）独立快照；稀缺资源只由一个 allocator 通过 Attempt 租约扣减。
- **Resolved / Effective**：放置产生 `ResolvedBindings{offer_ref, witness}`；会话启动后由 ACP client 回证 `EffectiveSessionEvidence{model 实际 SetOption 后的值, MCP broker READY, skill loaded, runtime_generation, ctxpack_digest}`。清单里只有 available models，current model 只存在于会话证据。

### 2. AdmissionContract：requires 与 uses 编译成一份合同

- **Requirement AST（`requirement.v1`）**：严格 tagged-union JSON：`all / any / one_of / not / cap{kind, id|prefix, subject?, version{scheme: semver|date|opaque, constraint}, attrs{k: {op, typed value, unit}}}`。规定：空节点为 true；`one_of` 恰好一真且无 unknown 为真，多真为假，含 unknown 为 unknown；wildcard 为存在量词并输出 witness；最大深度 8、节点 64；canonical encoding 用于 digest。三值逻辑：`not unknown = unknown`。
- **安全否定不用 inventory 缺失推导**：`!network:public` 必须匹配 enforcement fact（如 `egress_policy:deny-public@digest`）。
- **uses**：`{model?: selector, mcp: [id…], skills: [name…]}`。每个 uses 自动生成不可绕过的硬约束；requires 里出现的 model / mcp / skill 若意在使用，必须有对应 binding request，否则显式 `presence_only`；ResolvedBinding 必须引用满足该 AST 分支的 `offer_ref + witness`（`any(mcp:github, mcp:gitlab)` 由 gitlab 满足就不能绑定 github）。
- agent 自身 requires、step requires、delegate requires 恒取 AND；`agent:` 只缩小候选，不豁免。

### 3. 带 fence 的两阶段准入

1. hub 取不可变 `FleetSnapshot`（各 node 最近接受的快照 + hub 侧模型观测 + hub 自身），`fleet_snapshot_digest`。
2. hub 原子复核**自己拥有的条件**：policy / ACL / 数据等级 / agent 定义 / 容量租约。
3. 打开 Attempt（`leased`），`AttemptSpec` 固定：requirement_ast、evaluator_version、fleet_snapshot_digest、matched_evidence、resolved_bindings。
4. **node 终审**只收 node 拥有的叶子谓词：`PrepareAdmission{fence=(ledger incarnation, attempt lease epoch), expected_node_revision, node_leaf_predicates, uses, deadline, nonce}` → node 在当前快照复核、为 uses 创建带 TTL 的幂等保留 → `ADMIT{node_revision, bindings}` / `REFUSE{code, atoms}`；旧 fence 一律拒绝。`CommitAdmission(token)` / `AbortAdmission(token)` 幂等；prepare 不等待 hub。
5. `AdmissionEvidence` 随 `leased→prepared` 的账本 CAS 写入；`EffectiveSessionEvidence` 随 `prepared→running` 写入；任何账本事务内不做网络 RPC。
6. 失败反馈结构化：`{code, retryable, snapshot, failures[{agent, node, reasons[{atom, code, action}]}], alternatives, retry_after}`；code 集固定并映射到 action（refresh / wait / repair / choose_other / policy）。

### 4. MCP：node 本地 broker（`node_mcp_binding.v1`）

- **binding 归 SessionLease**（`logical_session_id + runtime_generation`），不是 Attempt：聊天会话跨回合复用，step / delegate 会话与 Attempt 同寿命；`/new`、关闭、进程代数变化时释放。
- **broker 是独立 OS principal / 容器**：秘密目录只对 broker 可读；launcher 只是无秘密的 IPC 客户端，经认证的 Unix socket 请求 broker 启动 MCP backend（隔离 UID / sandbox）；HTTP / SSE 走 broker 的回环代理，与平台反向隧道（`steve:*`）分路、独立端点。
- 准入返回**可直接写入 ACP 的公开 descriptor**（launcher 绝对路径由 node 申报，binding id 随 ADMIT 返回）。
- 秘密边界的精确表述：**node-owned external credentials 不离开 broker**；平台反向 MCP 的 session token 由 hub 创建，是另一类。
- 配置：agent 新增 `mcp_bindings: [{id, required}]`；旧 `mcp_servers` 在 hub 本机保持旧语义，远端报明确迁移错误；MCP 带 contract / version / tool-set digest；只有 node 宣告特性才启用。
- broker 本地执行 `(hub identity, project, principal, mcp id)` ACL；hub 是否被完全信任要在威胁模型里写死。

### 5. 时效、心跳与漂移

- 连接心跳只证明连接活着，**不续任何 evidence 的 TTL**；每 kind 独立 `probe_epoch / checked_at / outcome / valid_for`。
- 变化立即推完整快照；重连全量同步；`(revision)` 去重；每 node jitter；探测超时 → unknown；撤权 / 过期 → 立即 unavailable；普通恢复带滞后。
- 运行中的 Attempt 不因 diff 取消：`at-risk` 按 `(attempt, offer, risk_epoch)` 边沿触发并有 cleared；node / 会话消失直接 failed / expired；无候选进入 `blocked-capability` 退避。
- 控制面与数据面分离：latest-value coalescing、独立上限与优先级，或独立 control connection，避免 mux 队头阻塞拖住 ACP。

### 6. 会话与工具 home

- (node, harness) 共享一个 ACP 进程：model 是会话状态，MCP 是会话绑定，skill / tool home 是进程启动时加载。node 上工具 home 必须隔离；skill 内容寻址、原子物化、`available_to_new_sessions`；home 变化产生新 runtime generation，drain / restart 生效；chat resume 校验原 effective hash。

### 7. 协商与信任

- 握手 `protocol_min / max / chosen / features[]`；协商结果固定为连接状态，声明依赖图；旧 node 合成的快照 `coverage=partial, source=legacy`。
- 传输不变量：mTLS 或 VPN，且 overlay identity 绑定配置中的 node / hub id；`Hello.Hub` 名字不可信。single-hub ownership 持久化 `{hub_id, incarnation, epoch, lease}`，转移是显式流程。
- `a2a` 返回 unsupported，直到有账本中介的 connector、fencing 与结果记录。

### 8. 感知策略

- 规划器：按 (node, harness) 去重，只列当前项目、调用者可用端点的正向摘要与关键不可用原因；不含 detail / command / path / IP。
- agent：`steve_fleet(requirement?)` 只读、按项目与数据等级过滤、去重；`+N` 必带查询路径；structuredContent + 兼容文本。
- ctxpack：只放本会话 effective capability 摘要，独立 4KB，稳定裁剪，带 count / digest；机群不进 ctxpack。
- 页面六态：可用于新会话 / 当前会话已启用 / 不可用 / 未知 / 声明未核实 / 过期。
- 历史：`observe.manifest{old_digest, new_digest, diff[]}`，接受的快照内容寻址持久化。

### 9. 门禁：每个 kind 的垂直链完成才开放调度

链 = 观测（到最低 assurance）→ 时效 → matcher → node 终审 → 必要绑定 → Attempt 证据 → 恢复测试。任一环缺失，该 kind 只观察不匹配。

### 10. 测试不变量

AST 的 property / fuzz（三值、版本、any / one_of / not、canonical）；快照恶意输入、重复键、乱序、超限、未来时间、部分覆盖、原子拒绝；N / N-1 协商与未知流；match 后 session/new 前能力消失；多会话不同 MCP 集合与同名 env，断言秘密不出现在 agent env / advert / 日志 / 账本；credential 到期与刷新；network flap、node 重启、乱序推送；pinned agent 仍执行全部 requirements；按 digest 重放放置理由；ctxpack ≤24KB 且关键字段不裁；shared process 的 skill generation 与 model 隔离；a2a 任何调用被拒。

---

## 第二部分：本轮已落地（2026-09-03）与门禁状态

**新增 `internal/ability`**（无内部依赖）：`Snapshot / Capability / Evidence / Coverage`，`Validate` 整份拒绝超限与畸形快照并计算不含时间的 digest；`Requirement` AST + 文本编译（`kind:id`、`prefix*`、`a|b`、`!x`、`@version` 约束）+ 三值 `Match`（scoped kinds 按 harness；未覆盖 → unknown；stale → unknown）+ 结构化 `AtomResult{code}`；`Diff`、`Compact`（只输出 id）。

**node**：`node.Snapshot` 观测 harness / tool（PATH 存在，assurance=existence）、mcp（按 harness scope，只观察）、hardware（cpu / arch / gpu，typed attrs）、declares、tags；coverage 逐 kind 声明；进程 generation + sequence；advert 携带 `snapshot` 与 `features`；**一次只服务一个 hub**。

**hub**：接收时 `Validate` 失败即拒绝整份并保留上一份；按 (generation, sequence) 拒绝旧快照；stamp `received_at`；漂移记结构化 diff 到历史；hub 自身也出快照。roster 按 harness scope 匹配、结构化解释；**pinned agent 不豁免 requires**（exec 与 delegate 两处）；plan 校验 AST；规划 prompt 与步骤上下文只给 id 摘要（本机、4KB）；`steve_fleet` 工具；资源页六态展示。

**门禁（按第 9 节）**：截至本文最后一批，`harness / tool / hardware / model / skill / mcp / tag` 参与调度（tag 为 legacy `declared_ok`）；`a2a / network / credential` 只观察、不匹配（`NOT_SCHEDULABLE`，且对其取反也是 unknown）。**assurance 仍是 existence**，`launchable` 探测、时效心跳、node 终审流、AdmissionContract 的 uses 绑定、MCP broker、工具 home 隔离、协议协商中的 `protocol_min/max`、hub 身份绑定，均未实现——这些是第一部分的内容，也是打开更多 kind 的前提。

**本轮追加（同日）**：

- **node 终审（`execution_admission.v1`，第 3 节第 4 步的第一段）**：`nodewire.StreamAdmit`，请求携带 AST（不是文本）、harness、hub 放置所依据的 `(generation, sequence)`；node 在**此刻**的观测上求值并回 `Admission{source=node, verdict, code, generation, sequence, digest, atoms}`。hub 侧 `roster.Admit` 先用 `ability.Partition` 把 requirement 按归属拆开：node 拥有的（harness / tool / hardware / tag）交 node，hub 拥有的（model、hub 本机）在 hub 判；hub 侧先拒。不会说这一流的旧 node 得到 `source=legacy, NO_ADMISSION`（unknown），无法询问的来源得到 `source=cached`。exec 与 delegate 在 `leased→prepared` 时把 `Admission` 与 `Requires` 写进 Attempt；node 拒绝则该 Attempt 失败、步骤回到放置（不会先选同一个 agent）。**尚未做**：fence / nonce、uses 绑定的保留与 commit / abort、`EffectiveSessionEvidence`。
- **结构化拒绝**：`steve_delegate` 无法放置时返回 `{code: NO_CANDIDATE|NOT_ELIGIBLE|UNMET|UNKNOWN_AGENT|BAD_REQUIREMENT|REFUSED_AT_ADMISSION, retryable, requires, failures[{agent, node, why, reasons[{atom, verdict, code}]}]}`，文本即 JSON。
- **`steve_fleet(requires?)`**：带 requirement 时每个 agent 标"满足 / 缺什么"。
- **单 hub**：第二个 hub 在握手内被拒（`Advert.Refused`），而不是先拿到 advert 再被断开。
- 页面：attempt 表新增"准入"列（机器终审 / hub 判定 / 缓存快照 / 旧版 node，带快照版本）。

**同日第二批**：

- **时效**：hub 每分钟（带抖动）向每台在线 node 要一次新快照（`StreamAdvert`），修掉了"握手后 15 分钟所有 tool 证据过期、什么都放不下去"的线上问题。node 侧与 hub 自身跑 `LaunchProbe`：`--version`、3 秒、独立进程组、后台 10 分钟一轮；快照里 `launch` 是独立证据、独立时间；起不来 → unavailable；`npx / python / sh` 这类启动器只证明启动器能起，不报版本。**assurance 现在到 launchable**。
- **工具 home 隔离（协作审计 3-4）**：steve-node 启动时 `runtime.Prepare(state_dir)`，每个 harness 用 `state_dir/runtimes/<harness>`，进程 env 由 `runtime.ApplyEnv` 指过去；不再碰用户真实的 `~/.codex` 等。
- **技能下发与物化（协作审计 3-1，`skill_bundle.v1`）**：hub 把启用技能打成确定性 tar（`skills.Pack`，无时间无属主，按内容寻址；≤32MB），经 blob 流放到 node，再用 `StreamSkills apply <hash>` 让 node 校验哈希、安全解包（拒绝越界、拒绝非常规文件）、原子替换、软链进每个 harness home、清理旧包；node 在 advert 里报 `skills=<hash>`，快照里按 harness scope 报 `skill:<name>`（version = 内容哈希，coverage complete）。触发：node 上线、`live.Apply()`（技能变更先推 node 再重启 harness）。**`skill` 类已打开调度**。
- 未做：`launchable` 之上的 `functional`；chat 会话的 resume 校验 effective hash；跨 node 的 skill 生效仍依赖 hub 重启远端 harness。

**同日第三批：MCP broker（`node_mcp_binding.v1`，第 4 节的第一段，协作审计 3-2）**

- node 的 `mcp_servers{}`（含 env）只在 node；`Server.serveBroker` 在 `state_dir/mcp.sock`（0600）上服务。`AdmitRequest.Uses` 列出会话要用的 MCP；node 在准入通过后为每个 id 发 binding（随机 128 位、TTL 24h、内存态、随进程消失），回 `AdmitReply.Bindings[]{name, command=steve-node 绝对路径, args=[mcp-launch, -socket, sock, id]}`；`Admission.Bound` 只记名字，binding id 不进账本。hub 侧 `roster.Admit(…, uses, …)` 把每个 uses 翻成硬约束 `mcp:<id>`，通过后把 launcher 追加进 `session/new` 的 MCP 列表（exec 的 `StepRequest.MCP`、delegate、turn 三条路径）；`capability.Assembler` 对 `Node != ""` 的 agent 不再在 hub 上解析 / LookPath；hub 配置校验只对 hub 本机 agent 检查名字。
- launcher `steve-node mcp-launch` 连 socket、写 binding id、把 stdio 接过去；broker 校验 binding、用 `os.Environ()+spec.Env` 起后端（独立进程组，连接断即杀）。http / sse 的 MCP 保持 declared → unknown，准入 uses 到它们时拒绝（UNAVAILABLE）。**`mcp` 类已打开调度。**
- 真机验证：node-a 配 `fs`（`npx @modelcontextprotocol/server-filesystem`），hub 上 builder 配 `mcp_servers: ["fs"]`；控制台 `@builder` 用 fs 列目录成功，node-a 日志显示 broker 为该 attempt 启动 fs；advert / binding 的 JSON 里没有 env。
- **hub 身份与协商（第 7 节的一部分）**：node 把服务过的 hub 记在 `state_dir/hub.json`（hub、last_seen、released）；hub 每分钟的刷新会续 last_seen；hub 静默（未 released）10 分钟内其它 hub 被拒，干净断开即交还；`steve-node adopt <hub>` 显式移交。握手 `Hello{protocol_min, protocol_max}` → node 选双方共有的最新版本作 `Advert.Version`，无交集拒绝并写明双方区间。**仍缺**：身份与 mTLS / overlay 绑定（`Hello.Hub` 名字仍不可信，部署不变量），持久 epoch / lease。
- **同日第四批**：binding 随 Attempt 结束释放（`StreamRelease`，exec / delegate / turn 三处在 Attempt 收尾时调用；node 删 binding 并杀掉其后端进程；TTL 只是兜底）；`AdmitRequest.Nonce` 回显校验；**http / sse 的回环代理**（node 在 127.0.0.1 上起 `mcp-proxy.port`，binding URL 为 `/b/<id>`，代理转发到真实 URL 并注入配置的 headers；descriptor 只有 URL）；harness 的 **functional** 证据：hub 通过 ACP 真正对话过的 harness，在 roster 的候选快照上加 `acp` 证据、assurance=functional。
- **同日第五批**：node.json `hubs: {name: token}` 把 token 绑到 hub 名字（拿着 hub-1 的 token 自称 hub-2 会被拒），单一 `token` 仍可用；advert 携带 `Health{disk_free, disk_total, load1, worktrees}`（每次 advert 刷新），roster 对空闲磁盘不足 1GB 的机器拒绝放置，资源页显示。这是第 1 节"容量不在清单里"的最小实现，还没有槽位 allocatable 与显存。
- **同日第六批：broker 独立进程**。`node.Broker` 是同一个类型的两种跑法：node 内嵌（node.json 的 `mcp_servers`），或 `steve-node mcp-broker -config mcp.json` 独立进程（意图是独立 OS 用户、mcp.json 只对它可读、socket 0660 共享组）。socket 协议一行一命令：launcher 只写 binding id；`BIND / RELEASE / LIST` 需要控制 token。node.json 配 `mcp_broker: {socket, token}` 后自身不再持有任何 MCP 配置，快照里的 `mcp:` 来自 broker 的 LIST（evidence method=broker），broker 问不到时该类 coverage=error。真正的用户隔离仍是部署动作（建用户、chown、systemd），代码已不再是阻碍。
- **仍缺（第 4 节其余）**：`(hub identity, project, principal, mcp id)` ACL；MCP contract / tool-set digest；fence（当前只有 nonce，没有 lease epoch）；broker 侧对 launcher 调用方的身份校验（同机任意进程拿到 binding id 即可用，id 是 128 位随机、随 attempt 释放）。

已回退的错误做法：把 node 全部 MCP env 注入共享 agent 进程（第 1 轮评审第 9 条）。
