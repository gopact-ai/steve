# 三节点 e2e 验收场景

状态：mock 17/17 + 真模型 3/3 在真机通过。
mock：`STEVE_MESH_E2E=1 go test ./e2e/mesh/`；真模型：再加 `STEVE_MESH_REAL=1`（约 10 分钟）。
一台 node 一次只服务一个 hub：跑真机场景前先停掉正在用这两台 node 的 hub，否则握手被拒（`this node is served by hub …`），套件会报"node is down"。
A1–A5 连接层；B1/B3 读模型与两个渲染器（B2 变更流由 C1 覆盖）；C1+C2 跨机放置与实测并行重叠；
C3 命令验证在 node 上跑（ssh 核实标记文件）+ 跨机 agent 审核 FAIL 有约束力；C4 无处可跑指名原因；
C5 委派子任务预算从父任务扣减并回记；C6 环在结构上拒绝；C7 REPLAN 触发规划 agent 修订、已完成步骤复用；
聊天面 `/plan`（声明式 + 自动拆解）与 `steve_delegate` 经真实 MCP 服务端跨机委派。
真模型：Real-0 逐台探测哪些 harness 真能答；Real-1 真 claude 规划、真 codex/kimi 在两台 node 执行并在节点上验证；
Real-2 真 claude 回合中自行 `steve_delegate` → `steve_await`，子任务在 node-b 完成并回记预算。

真模型撞出、mock 撞不出的四个缺陷（已修）：FINDING 一律触发重规划；gopact fail-fast 取消同胞分支而
Retry 不复活伤员；同步工具调用扛不住分钟级子任务；Bearings 让 agent 因"不是 git 仓库"停手。

拓扑：

| 角色 | 地址 | 说明 |
|---|---|---|
| hub | 10.251.239.109（本机） | 网关、协调层、读模型、agentmcp |
| node-a | 10.37.124.132 | 声明 capability `gpu` |
| node-b | 10.37.97.2 | 声明 capability `internal-net` |

每条场景标注：**怎么触发** → **必须观察到什么**。没有"应该差不多对"的判据。

## A. 连接层

**A1 三节点注册**
`steve doctor` → 两台都 up，各自 advert 打印 os/arch、harness 列表、models、capabilities；
故意配一个不存在的 harness → 标 `(missing)` 并说明原因，而不是消失。

**A2 远程会话往返**
`@agent-a 说句话` → agent 进程真的起在 node-a（在 node-a 上 `pgrep` 得到）；答案回到 hub 的卡片。

**A3 反向 MCP**
远端 agent 调 `feishu_send` → 请求从 node-a 的 127.0.0.1 出发、经隧道到 hub 的 agentmcp；
hub 侧收到的 Authorization 是该会话自己的 token。

**A4 node 掉线与重连**
执行中 `kill` node-b 的 steve-node → 其上会话立即失败（不是挂起）；
attempt 记为 interrupted；`/status` 与 TUI 显示 down。重启 steve-node → 下一次派活自动重连。

**A5 能力隔离**
`requires: [gpu]` 的放置只能落 node-a；要求 node-b 跑它 → 被拒且错误指名原因。

## B. 读模型与两个渲染器

**B1 快照完整**
`GET /state` 含：三节点及其 advert、agents 与放置、任务树（含子任务）、预算、attempts。

**B2 变更流**
一次派活期间订阅变更流 → 收到 task 创建、step 状态迁移、budget 变化的事件，不靠轮询。

**B3 TUI**
`steve top` → 显示三节点状态、活动任务树、预算条；
kill 掉 node-b → 该行在一个刷新周期内变为 down，不需要重启 TUI。

**B4 Dashboard**
浏览器打开 → 与 TUI 同源同数据；任务树可展开看到子任务的 context 对象；
默认只绑 loopback，配了 addr+token 才对外。

## C. 协调层（supervisor）

**C1 规则放置**
一个 `requires:[gpu]` 的 step → 自动落 node-a，`/tasks` 里 attempt 的 Node 字段是 node-a。

**C2 fan-out → merge**
plan：step1(node-a) 与 step2(node-b) 并行，step3 `merge:[step1,step2]` 收敛。
→ 两步真的并行执行（时间重叠），step3 在两者都完成后才起。

**C3 verify 失败走重规划边**
step 的 `verify` 命令返回非零 → step 不进 done，产生新的 Plan Rev，
`Because` 字段写明触发原因；换一个满足同能力的 agent 重试。

**C4 node 掉线重放置**
执行中杀掉承载 step 的 node → 该 step 被重新放置到另一台满足 requires 的节点，
Plan Rev +1，旧 attempt 记为 interrupted。

**C5 预算树**
子任务从父任务剩余预算扣减；父的 MaxElapsed 是硬上限；
耗尽 → 刹车卡带"跑到哪了"（轮次、耗时、每次尝试的结果）。

**C6 环检测**
A delegate B，B delegate A → 结构上被拒绝（不是靠提示词），错误说明是环。

**C7 上下文可枚举**
`/tasks N` → 能看到该子任务实际拿到的 Context：goal、祖先链、refs、findings、bearings、预算余量。
载荷超限 → 被要求先落成 ref，而不是静默截断。

## D. 回归

**D1** `go test -race ./...` 全绿。
**D2** 现有单机路径不受影响：不配 `nodes{}` 时行为与今天一致。
