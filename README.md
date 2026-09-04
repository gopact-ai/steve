# Steve

飞书（Lark）⇄ [ACP](https://agentclientprotocol.com) 网关：把 codex、Claude Code 等
coding agent 接进飞书聊天，像同事一样给它派活。

与「把消息转发给模型」的做法不同，Steve 的核心承诺是**结构性保证**：

- **送达不靠模型自觉**——最终答案由 Steve 从 ACP 事件流渲染成卡片并 @ 提问人；
  agent 中途可以主动发里程碑卡，但答案的送达是平台行为。
- **越权在结构上不可能**——agent 的发送能力经每会话独立 token 注入，token 与会话
  绑定；A 会话的 agent 无法发进 B 会话，撤回只能撤自己本轮发的消息。
- **审批由策略决定**——工具调用的批准走 permission broker（auto/read/write/deny），
  需要人的才上卡片按钮，其余不打扰。
- **崩溃不丢任务**——任务锚点持久化；网关重启后关闭孤儿 attempt、恢复会话、
  清理残留卡片，续跑并照常送达。

## 快速开始

前置：Go 1.27+、Node.js（`npx` 拉起 ACP 适配器）、一个飞书自建应用
（`steve setup -create-app` 可走官方设备流现场创建）。

```bash
go build -o steve ./cmd/steve
./steve setup     # 交互式：应用凭据、群策略、主人 open_id（本人自填绑定）
./steve doctor    # 体检：凭据、home、每个 agent 拉起一次会话
./steve run       # 启动网关（flock 单例，重复启动会被拒绝）
```

绑定主人后首次 `run` 会主动私聊建立 **home 会话**（身份初始化：SOUL/USER/MEMORY）。
不知道自己的 open_id？先 `run` 起来私聊 bot 一句话，网关日志里
`gateway: message ... sender=` 就是（open_id 按应用作用域，别处查到的无效）。

## 聊天即界面

| 动作 | 含义 |
|---|---|
| 直接说话 | 派活给当前 agent（默认排队，不打断进行中的轮次） |
| `!` 前缀 | 打断并替换当前轮次（全角 `！` 同） |
| `@codex` / `@claude` | 指定或切换 agent |
| `/t 任务` | 在群里种一个话题会话并行跑 |
| `/new` `/clear` | 重置会话（归档可恢复，卡片带恢复按钮） |
| `/status` `/model` `/history` `/skills` | 状态、模型切换、历史恢复、技能管理 |
| `/tasks` | 列任务；`/tasks 12` 看进度，`/tasks pause\|resume\|cancel [编号]` 管理（中文动词同样认：暂停/继续/取消） |
| `/every 30m …` `/at 09:00 …` | 定时任务：`30m`、`2小时`、`09:00`、`每天 09:00`、`周一 09:00`；`/schedules` 查看与取消 |
| `/plan 目标` | 多步计划：拆成步骤，按能力放到能跑的机器上，回一棵带「谁在哪台跑的」的树；`/plans` 看历史与修订 |
| `/fleet` | 现在能到哪些机器、哪些 agent 可用 —— 读的是活的申报，不是配置 |
| `/project` | 这个会话在哪个项目上干活；`/project use 名字` 切换（会话随之重开，身份与记忆保留） |
| `/grant 项目 open_id 角色` | 授权（read / write / admin / none），owner 或项目 admin 可用；`/grant 项目` 看授权 |
| `/approve 编号` `/deny 编号` | owner 批准或拒绝 sealed 项目的答案离开 |
| `/effects` | 结果未知的对外动作；`/effects 编号 happened\|new` 由 owner 裁决 |

以上动词在 dashboard 的控制台和 `steve say` 里同样可用。

多阶段任务里 agent 会用内置的 `feishu_send` / `feishu_update` / `feishu_recall`
维护一张演进的进度卡（带 `2/3` 阶段角标、与最终卡同族的尾标）。

长任务不需要你守着：跑够 `offline_reminder_after`（默认 15 分钟）且期间你没再说话，
答案卡之外还会补一条 @ 你的纯文本提醒；预算用满时的刹车卡会附上「跑到哪了」
（轮次、耗时、每次尝试的结果）。定时任务每次触发都开一个新任务，不会把一个任务的
预算按天磨光，也不会并进你自己正在做的那件事。

## 多主机

一个 hub 带若干 `steve-node`。hub 也是一个 node，只是角色是 hub（协调）：它的机器名、主机名、IP、
能跑哪些 harness，都和别的 node 一样申报、一样出现在 `/fleet`、Fleet 页和 `steve top` 里，配置里
声明了但本机 PATH 上没有的 harness 一样标 missing。node 上跑的是本机的 agent 进程，hub 通过一条长连接
把整个 **ACP 会话**隧道过去 —— 不是自己发明一套 RPC，因为 Steve 用到的 ACP 面本来
就是位置无关的（只 advertise `Elicitation`，从不让客户端读写文件或开终端）。

```bash
# 每台机器
./steve-node -config node.json     # 见 node.example.json

# hub 的 config.json
"nodes":    { "host-3": { "addr": "10.0.0.3:7701", "token": "…" } },
"agents":   { "builder": { "node": "host-3", "harness": "codex", "requires": ["gpu"] } },
"projects": { "lab": { "home": { "node": "host-3", "path": "/srv/steve-work/lab" } } }
```

- **node 的申报是唯一权威**：它自查哪些 harness 真能起，起不来的带原因暴露而不是隐藏。
  配置里写了不代表机器上有。
- **反向 MCP 隧道**：远端 agent 调的是**它自己机器**的 loopback，node 沿同一条连接转发回
  hub —— 「只走 loopback + 每会话 token」的安全性质原样保住，没换成对外暴露。
- **node 掉线**：其上会话立即失败（不挂起），后台自动重连，恢复后无需重启 hub。
- `steve doctor` 会逐台真实拨号，把 advert 和「这台跑不了什么」打出来。
- **node 要在登录环境里起**（`bash -lc`、systemd 的 `EnvironmentFile`，或 harness 的 `env`）：
  agent 以这台机器上的这个用户身份跑，它的凭据和代理都是这个用户 profile 里的东西。
  裸环境起的 node，advert 会说 harness 在，但一问就是 `Authentication required`。

## 项目

工作发生在**项目**里，不在 agent 里。agent 是 `(node, harness, model)`，没有目录；
一个会话绑定一个项目，项目的 `home` 说它的规范工作区在哪台机器的哪个目录。

```json
"projects": {
  "steve": { "home": { "path": "~/dev/steve" }, "level": "internal", "repo": "inplace" },
  "lab":   { "home": { "node": "host-3", "path": "/srv/steve-work/lab" } }
},
"gateway": { "default_project": "steve" }
```

- 新会话绑到 `gateway.default_project`（只有一个项目时可省）；owner 的私聊绑到 Steve
  自己的 home 目录 —— 它也是一个项目，名字 `home` 保留。
- `/project use lab` 切换：绑定带版本，旧会话归档，下一轮在新目录重开。任务在创建时
  固化项目，会话换了项目，已开的任务不跟着变。
- **目录只在 home 所在的机器上存在**：项目 `lab` 住在 host-3，让 hub 上的 agent 去做
  它，Steve 会说清两个地点和两条出路（换一个在 host-3 上的 agent，或换项目），
  而不是在 hub 上凭空开一个目录。跨机器物化与隔离工作树由 artifact 层提供。
- 旧配置里 `agents[].workspace` 启动时自动迁成 `projects{}`（每个 agent 一个项目，
  默认 agent 的成为默认项目），日志会打印该粘回配置文件的片段；两种写法并存会被拒绝。

所有名字与绑定 —— 会话、任务、计划、定时、项目绑定 —— 都记在 `state.json` 旁的
**ledger**（SQLite）里，它是唯一权威；旧的 `state.json` / `tasks.json` / `plans.json` /
`schedules.json` 首次启动时导入一次并改名为 `.migrated`。账本旁有一个不随备份回滚的
`incarnation` 计数和一份 `effects.log`（对外动作先记 started、有回执再记 confirmed）：
从备份恢复后 `steve ledger rotate` + `steve ledger recover`，恢复前签发的租约全部作废，
outcome-unknown 的对外动作列出来给人对账；`steve ledger status` 看现状。

## 工作区、产物与落地

每一次执行都是账本里的一个 **attempt**：拿到租约（自己的、项目的规范写锁、或它的隔离
工作树）才开始，运行中续约，租约丢了就取消，结束时带着结果落账。同一项目同一时刻只有
一个原地写者：另一条会话来了会被告知是谁在改、等一等或换项目。

- **聊天回合**在项目的规范目录原地运行，前后各拍一次快照，差异是一个产物，绑到
  `steve/<任务>/turn/<消息>`。快照用的是项目的**影子仓库**（hub 上每项目一个裸库，
  临时索引 + `add -A`），目录本身不需要是 git 仓库，也不会多出 `.git`。
- **计划步骤永远在隔离工作树里跑**：从计划开始时的快照（或它接续的那一步的结果）
  物化出来，不在 home 的机器上也一样——产物打成 bundle 经 node 链路送过去，node 只需要有
  git。并行分支各自一棵树；merge 步骤在 `inputs/<步骤>/` 下看到每个分支的结果。
  步骤跑完先 **publish**（快照回 hub、拿到 hub 的耐久回执），再验证，再绑名
  `steve/<任务>/<步骤>`；验证 agent 在自己机器上的另一棵树里看同一个产物。
- **委派**同样在隔离树里跑，结果绑到 `steve/<子任务>/result`，排队等父回合释放规范锁后落地。
- **落地**是一个六阶段操作：提议 → 上锁 → 三方合并（以出发的快照为基）→ 按路径写入
  （每条路径先记 started、写完记 confirmed）→ 提交（规范名 CAS 前进）。冲突不硬来：
  merge-conflicted / apply-conflicted / commit-conflicted 三种状态各自留档，路径列出来给人。
- 步骤可以声明 `touches`（它会写的路径），并行步骤不得重叠。
- **等级**：`projects[].level` 与 `nodes[].level` / `gateway.level`（public < internal < restricted < sealed）。
  放置与物化都先看等级：机器够不到项目的等级就不去；`sealed` 项目只在它的 home 执行，
  hub 只存元数据（回执由 home 节点签）。restricted / sealed 项目的回合答案发出去会记一条
  disclosure。`gateway.level` 不写时取 hub 要耐久保存的最高等级。
- **槽位**：`harnesses[].slots`（hub）与 node 配置里的 `slots` 限制同一台机器同一 harness 的
  并发会话，attempt 开始时租一个槽位，满了就明说。
- **权限**：`projects[].grants` 把 open_id 映到角色，`default_role` 给其他人（public/internal 默认 write，
  restricted/sealed 默认 none）；owner 到处都是 admin。切项目要 read，开一轮要 write，授权要 admin。
- **披露**：sealed 项目的答案不直接进聊天——记一条 disclosure 请求（只有元数据，内容留在内存），
  提问者收到编号，owner `/approve` 后答案才发到原会话；hub 重启会清空等待中的披露。
  Derive（改内容降密）是产物层的操作，降到源等级以下必须带 approval 编号。
- **对外动作即 intent**：agent 发的每条飞书消息先由当前 attempt 认领、记 dispatched、拿到回执记
  succeeded；没等到回执的是 outcome-unknown。同一任务的下一个 attempt 若要发同样的调用会被挡住，
  直到 `/effects 编号 happened|new`（或 `steve ledger resolve`）裁决。
- **attestation**：验证结果先作为独立事实（带 hub 回执）落账，步骤只在有本 attempt 的通过记录时才绑名。
- **副本**：产物在每个节点的副本有记录（transferring / present / verified / quarantined / lost / evicted）；
  节点每次重连是一个新 generation，旧 generation 的副本先隔离再校验。
- **容量预留**：计划开始前 supervisor 为每一步在放置会落到的机器上预留一个槽位（`endpoint:<node>/<harness>:slot:<n>` 租约，持有者是预留本身），
  步骤开始时把预留原子转成自己的租约（epoch 前进）；抢不到槽位的步骤等，不失败。计划结束释放剩余预留。
- **区域租约**：`gateway.region` 是本 hub 的区域，`nodes[].region` 说机器归谁签租约，`gateway.regions` 列出其他区域 hub 的
  `url`/`token`，`gateway.issuer_addr` + `issuer_token` 把本 hub 的租约签发给别人。attempt 的槽位与工作树租约由机器所在区域签，
  规范锁由项目 home 所在区域签；本地转换若 fence 到外区域租约，先问签发方再提交（这个窗口是多区域的代价，也是为什么外区域租约
  守着的东西没有该区域回执不绑名）。
- **直连**：`gateway.direct_transfer` 打开后，某个节点已经持有（副本 verified）的产物往另一个节点物化时不再经过 hub：
  源节点打 bundle，hub 给源节点签一次性 grant（token + 文件名 + 有效期），目标节点凭 token 直连源节点的监听端口拉走，
  拉完 grant 作废；任何一步失败都退回 hub 中转。`nodes[].peer_addr` 是其他节点能到它的地址（默认同 `addr`）。
  hub 仍是决定方：谁能拉什么、拉一次，都是 hub 说的。
- **恢复**：hub 重启时先扫过期 attempt，再把中断在写入阶段的 landing 按 WAL 逐路径收尾
  （已是新内容的跳过、还是旧内容的重写、被人改过的算冲突留档）；重试一个步骤是**接管**——
  旧 attempt 记为 superseded 并指向新的，它的租约全部作废。飞书出口的每次发送都先记
  started、拿到 message_id 再记 confirmed，`steve ledger effects` 列出 outcome-unknown 的。
  计划的 workflow 检查点落在 `workflows.db`，账本记着哪些 run 还开着：hub 重启后从检查点续跑
  （运行时不肯接的检查点就按计划存档重跑一次，做完的步骤直接跳过），进行中的步骤接管旧 attempt，
  结果沿任务锚点回到聊天。sealed 项目 home 在老版本 git（< 2.38）的节点上照样能落地，走的是
  read-tree + merge-one-file 的老路径。

## 控制台

页面按用户要回答的问题组织，六个入口：**工作台**（我在哪干活、跟谁说、能做什么）、**任务**（现在在做什么、卡在哪、花了多少；
标签：进行中 / 全部 / 已安排 / 用量）、**项目**（活在哪台机器哪个目录、谁能接）、**资源**（机器、AI 工具、Agent 与版本，怎么加）、
**待处理**（只放要你拍板的）、**历史与审计**。输入框上方常驻当前项目与 Agent，输入 `/` 与 `@` 有服务端补全。设计文档见 `docs/console.md`。

页面是 React（Vite + TypeScript，源码在 `web/console/`），组件来自 [Untitled UI React](https://www.untitledui.com/react)
（react-aria + Tailwind v4，随浏览器/系统切换明暗），构建产物嵌进二进制（`internal/readmodel/web/dist`，已提交），
运行时不需要 Node；改了前端就 `make console` 再 `go build`。左栏 Console / Fleet / Tasks / Plans / Ledger / Activity，
左下是连接状态；Ledger 的角标是等你处理的事（待批准的披露 + 结果未知的对外动作）。

dashboard 不只看：页面顶部的 Console 用 owner 身份把一行文字送进同一个 coordinator——说话、`/plan`、`/project use`、
`/tasks pause|resume|cancel`、`/grant`、`/approve`、`/effects`，回复和 agent 发的里程碑都留在页面上（`console:<name>` 会话，
锚点不是飞书消息，所以不会漏到群里）。对话记录存在账本里，hub 重启后还在。一行在跑的时候页面上就能看到过程：
agent 的推理、工具调用和它自己的清单实时刷新，计划类动词（`/plan`、`/repair`）按步骤显示每一步的 agent 在干什么；
回复落下后过程折叠在回复下面，随时展开。任务行、披露行、对外动作行、agent 行上带按钮，一键发对应动词。
shell 里同样能用：

```bash
steve say -url http://<hub>:7710 -token <token> /fleet
steve say -url http://<hub>:7710 -token <token> "@builder 把 README 补一段安装说明"
```

控制台要 `feishu.owner_open_id`：它以 owner 的身份行事，读模型的 token 就是它的凭据。

### 模型从哪来，harness 坏了怎么办

Fleet 页的 Model 列不是配置写的，是看出来的：ACP 的 `session/new` 会报当前模型和可选模型，每开一个会话就记一笔
（存账本，重启不丢）；hub 启动后还会在后台把没用过的 harness 各拉起一次问一遍（只开关会话，不发 prompt）。
`/fleet probe` 手动再问一轮。harness 不报模型（比如 grok）就显示"—"，不猜。

harness 在某台机器上起不来（配置里有、PATH 上没有）时，agent 标 blocked 并说明原因；如果同一台机器上还有健康的
agent，行尾出现 **Repair with …**，`/fleet` 里也提示 `/repair <agent>`。`/repair` 起一个钉在那个健康 agent 上的
单步计划：目标是把命令装好、让正在跑的 steve 进程的 PATH 能找到；修没修好由 `command -v` 在那台机器上说了算，
然后让机器重新申报一次。这个计划是声明式的（`fixed`），失败了就失败，不会送去 planner 改写。
node 掉线、模型不提供，这类不是装个东西能解决的，不给按钮。

### Agent 是什么

一个 Agent 是一份有名字的执行配置：在哪台机器上、用哪个 AI 工具、（可选）固定哪个模型、它所在的机器必须提供什么（运行条件，
写 kind:id 选择器）、开会话时接哪些 MCP 服务器。模型有两层：配置里固定的是偏好，开会话时 Steve 会把它设给 AI 工具；AI 工具
报告的才是实际。AI 工具暴露的其它会话选项（思考强度、模式等）同样可以按 Agent 固定（`options`），聊天、计划步骤、委派开会话时都会设。
`about` 一句话写清这个 Agent 适合做什么，规划器和其它 Agent 派活时会读它。Agent 能碰哪些项目由它所在机器的数据等级决定。资源页里点一个 Agent 可以改这些，改完立刻生效并写回配置。

### 项目是什么

一个项目是一块干活的地方：**一台机器上的一个目录**，里面可以有一个或多个 git 仓库。会话和任务都属于某个项目，在它的目录里跑，
结果落回它的目录。hub 给每个项目定两条规矩：Agent 怎么改它（直接改主目录，或在隔离副本里改完再合并回来），以及数据等级
（公开 < 内部 < 受限 < 密封，决定哪些机器能持有它的文件）。项目页上能看到每个项目里的仓库（分支、最近提交、有没有 AGENTS.md），
"添加项目"和"移除项目"都是即时生效并写回配置的动作。Steve 自己的家（`gateway.home_path`）也是一个项目，主人的私聊默认在那里。

### 加一台机器

资源页"添加机器 / Agent"：填名字、hub 拨号地址（`ip:7701`）、数据等级，hub 立刻登记并写回自己的 config.json，然后给你一条命令
到那台机器上执行——它会写好 node.json（AI 工具命令沿用 hub 的配置）、从 hub 下载 steve-node（hub 配了 `gateway.node_binary`
时）并以登录 shell 启动。Agent 同样在这里加，加完立即可用。不需要重启 hub。

### 机器各不相同：能力清单与选择器

机器上有什么 AI 工具、MCP、命令、硬件、网络、凭据，本来就不同，也不该拉平；要保证的是每台机器把"会什么、不会什么"说全说准，
放置、规划、agent 派活都按它决定。每台机器（hub 也算）申报一份**清单**，一条能力一项：`kind:id`，kind 有 harness / model / mcp /
skill / tool / hardware / network / credential / a2a / tag。能观测的必须观测（AI 工具、命令、MCP 命令在不在 PATH，有没有 GPU），
只能声明的才允许声明（node.json 的 `declares`），页面上分别标"已观测 / 声明 / 已配置但不可用"，放置从不信任没有观测支撑的工具声明。

```jsonc
// node.json
"tools":       ["docker", "gh"],                       // 要观测的命令
"mcp_servers": { "github": { "type": "stdio", "command": "github-mcp-server", "env": { "GITHUB_TOKEN": "…" } } },
"declares":    ["network:internal", "credential:prod"] // 只能声明的
```

MCP 服务器定义在拥有它的机器上，env 不出机器，也不注入 agent 进程。node 上跑一个 MCP broker（`state_dir/mcp.sock`）：
agent 配置 `mcp_servers: ["fs"]` 指的是**它所在机器**的 `fs`；准入时 node 为该次执行发一个 binding，会话里 MCP 的启动命令只是
`steve-node mcp-launch -socket … <binding>`，broker 再用本机的 env 起真正的服务器并把两头接起来。node 没有这个服务器就拒绝准入；
hub 本机的 agent 仍用 hub 自己的 `mcp_servers{}`。http / sse 类型的 MCP 经 node 本机回环代理转发并注入 `headers`，
会话里只看到 `http://127.0.0.1:<port>/b/<binding>`。binding 随这次执行结束一起释放。要把秘密和 node 进程彻底分开，
用 `steve-node mcp-broker -config mcp.json` 以另一个用户单独跑 broker（mcp.json 放 `socket`、`token`、`mcp_servers`），
node.json 里改成 `mcp_broker: {"socket": "…", "token": "…"}`，node 本身就不再持有任何 MCP 配置。旧的 `capabilities: ["gpu"]` 仍可用，
等价于 `tag:gpu`。

计划步骤和 `steve_delegate` 的 `requires` 用同一套选择器：`tool:docker`、`hardware:gpu`、`model:claude*`、`a|b`、`!x`、
`tool:docker@>=27`。**当前 harness / tool / hardware / model / skill / mcp / tag 参与调度**；a2a / network / credential
只观察、只在页面上显示，写进 requires 会得到 `NOT_SCHEDULABLE`，等各自的探测、绑定与秘密隔离链条完成后再逐类打开
（见 `docs/capability-manifest.md`）。匹配是三值的：机器没查这一类、或证据过期，是"不知道"，不是"没有"。

AI 工具与命令按"能启动"核实（后台跑 `--version`，3 秒内起得来就是 launchable，起不来标不可用并写明原因；版本只从真二进制
解析，`npx` 这类启动器不算）；hub 每分钟让每台 node 重新看一次自己，证据不会过期。每台 node 的 AI 工具都跑在 node 自己的
home 里（`state_dir/runtimes/<harness>`），不再用那台机器用户的 `~/.codex`、`~/.claude`；hub 启用的技能打成按内容寻址的包，
node 连上时与技能变更时下发、校验、物化到每个 home，并在快照里按内容哈希上报，`requires: ["skill:deploy"]` 只会落到真有它的机器。

放置只是决定；每次执行前，机器要在**此刻**的观测上对它拥有的条款做终审（`execution_admission.v1`），hub 判它拥有的
（模型、hub 本机），结果连同快照版本写进 Attempt。不满足时原因是结构化的：`steve_delegate` 返回
`{code, retryable, failures[{agent, node, reasons[{atom, code}]}]}`；`steve_fleet(requires)` 能先看谁满足、其他人缺什么。
清单变化记进历史；一台 node 一次只服务一个 hub，并记住它：hub 静默（没有干净断开）10 分钟内别的 hub 连不上，
`steve-node adopt <hub>` 可显式移交。node.json 里 `hubs: {"<hub 名>": "<token>"}` 能把 token 绑到 hub 名字，防止拿着 token 冒名。
每台机器还申报健康（空闲磁盘、负载、持有的工作树）；空闲不足 1GB 的机器不再接新活。

## 计划与协作

`/plan` 把一个目标交给规划 agent 拆成步骤（`gateway.planner` 指定哪个 agent 负责拆解；
不配则只放置声明式 workflow，开放目标视为一步）。每步按 `requires` 的能力**在执行前**
解算放置 —— 所以一台机器中途消失是正常情况，不是异常。

```
✓ build · builder@node-a · gpu
✓ stage · shipper@node-b · internal-net
✓ ship  · shipper@node-b · internal-net
```

执行引擎是 [gopact](https://github.com/gopact-ai/gopact) 的 workflow：Steve 把 plan
编译成拓扑，gopact 负责调度、join、检查点与运行日志。失败的一步 **retry 时只重跑它自己**，
已完成的步骤复用检查点。

- **验证是 plan 的义务**：`verify` 是 Step 上的字段，agent 说做完了不算数。
  `command` 在**步骤所在的机器上**跑（不是 hub），`agent` 让另一个 agent 独立审核并给出
  PASS/FAIL；想不验证必须显式写 `kind: none` 并给理由。
- **上下文由 hub 装配**：goal + 祖先链 + 结构化 refs + 前序发现 + 冷启动定向 + 该机器的能力事实，
  有大小上限，超了要求先落成 ref 而不是静默截断。
- **恢复在步骤内部，分支互不牵连**：某步失败 → 在节点体内换一个 agent 重试（没别人时同一个再试），
  同时在跑的分支照常跑；只有 Steve 对某步彻底放弃，run 才失败。已完成的步骤在修订里复用，不重跑。
- **两种报告行**：`FINDING:` 是给后续步骤的事实，只往下传；`REPLAN:` 才是"后面的步骤已经错了"，
  停下让规划 agent 出新修订（保留已完成的步骤，修订原因入档，`/plans N` 可看）。修订有界。
- **agent 之间没有旁路信道**：唯一的交互是 `steve_delegate` / `steve_await` —— 把一件有界的活交给
  另一个 agent（可能在另一台机器），立刻拿回 task id，再 await 结果。子任务挂在调用方任务下、从调用方
  **剩余预算**里出、拿自己的 token、里程碑卡标明「受谁委派」，而且**与发起它的工具调用解耦**：
  客户端超时断开不会取消另一台机器上做了一半的活。深度上限与环检测（A→B→A）在结构上拒绝。

## 可观测

一个读模型，两个渲染器 —— 谁都没有特权，所以终端和浏览器不会各说各话。

```bash
steve top          # 终端：机群、agent、任务树、预算条，掉线实时变红
steve dash         # 打印 dashboard 地址（页面由网关自己服务）
```

默认只绑 loopback；要对外看得配 `read_model_addr` + `read_model_token`。

## 架构

```
飞书长连接 → gateway（卡片/审批/锚点）→ coordinator（轮次/任务/预算/计划）
           → capability assembler（身份+技能+MCP，指纹化）→ acphost（ACP 会话）
                                                          ├─ 本机子进程
                                                          └─ nodewire → steve-node（远端）
supervisor：planner（规则/声明式）+ gopact workflow（调度/检查点/运行日志）
readmodel：快照 + 变更流 —— steve top 与 dashboard 的共同底座
ledger：SQLite 单一权威（Command / Operation / Event、名字 CAS、租约、绑定）+ incarnation + 效果日志
project：项目与 ProjectHome；Materialize 是「这一步在哪个目录跑」的唯一入口
attempt：唯一的执行 Operation —— 租约、心跳、状态机、接管与过期清扫
artifact：每项目一个影子裸库；快照 / 工作树物化（本机或经 node 的 bundle）/ publish / 落地
agentmcp：内置 loopback MCP server（每会话 token），远端经反向隧道抵达
```

能力装配全部进指纹：能力变了会话即显式漂移（提示 `/new`），绝不静默换底。

## 安全模型

- `config.json` 含应用凭据：0600、已 gitignore，绝不入库
- 状态目录 `~/.steve/` 持锁（flock）：同一状态只允许一个网关进程
- 内置 MCP server 只绑 127.0.0.1，每会话独立 bearer token，随会话持久化
- 权限策略按 harness 配置；owner 与 guest 的 home（身份/记忆）严格隔离
- agent 内容中的 `<at>` 会被剥离：@ 人是平台最终卡的专属行为

## 测试

```bash
go test -race ./...        # 全部 wire 级防线（含 mockagent 真子进程往返）
```

`e2e/mesh/` 是三台真机的验收（场景清单见 `e2e/mesh/SCENARIOS.md`）：连接层、
读模型与两个渲染器、跨机放置与真实并行、掉线重放置、命令验证在 node 上跑、跨机交叉审核、
REPLAN 触发修订并复用已完成步骤、规划 agent 自动拆解、以及 agent 经 MCP 工具跨机委派。

再加 `STEVE_MESH_REAL=1` 跑**真模型**：hub 的 claude 拆解、节点上的 codex / kimi 执行、
`go test` 在节点上验证；以及 claude 在回合中自行调用 `steve_delegate` 把活交到有能力的机器。
约 10 分钟，花真 token。

```bash
# 先停掉正在用这两台 node 的 hub：一台 node 一次只服务一个 hub（干净停掉即交还；hub 崩溃后要等 10 分钟或 steve-node adopt）
STEVE_MESH_E2E=1 STEVE_MESH_NODE_A=host-a:7701 STEVE_MESH_NODE_B=host-b:7701   go test ./e2e/mesh/ -v
```

`e2e/ux/` 是对真实飞书的场景验收（卡片顺序/状态/@/撤回等用户可见面）。
需要 `lark-cli` 授权与一个专用测试会话：复制 `e2e/ux/.env.example` 为
`e2e/ux/.env` 填入标识（已 gitignore）。脚本会发送真实消息。

## Harness 支持

| Harness | 状态 |
|---|---|
| codex（@agentclientprotocol/codex-acp） | ✅ 真机验证 |
| Claude Code（@agentclientprotocol/claude-agent-acp） | ✅ 真机验证 |
| grok / kimi | ⚠️ 已接线，未实测 |

## License

Apache-2.0
