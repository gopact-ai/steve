---
name: steve-delegate
description: 在 Steve 里把一件事交给别的 agent（可能在别的机器上）：用 steve_fleet 看谁在哪台机器、能做什么；用 steve_delegate 委派一个有边界的目标并传引用；用 steve_await 等结果。需要你没有的机器、凭据、GPU、内网、AI 工具或技能时读这个；也说明什么不该委派、子任务花的是谁的预算、结果怎么落地。
---

# 先看有谁

`steve_fleet(requires?)` 列出别的 agent：各在哪台机器、机器有什么、此刻能不能接活。`requires` 用选择器过滤，每个 agent 都会被逐条判定并给出原因：

```
harness:codex      AI 工具          tool:docker      机器上的命令
hardware:gpu       硬件             model:claude*    模型（可通配）
skill:<name>       技能             mcp:github       MCP 服务器
tag:<name>         机器标签
```

`network:*`、`credential:*` 这些只观察、不参与放置，别拿来筛。

# 委派

`steve_delegate(goal, agent?, requires?, refs?, expect?)`：

- `goal`：**一个**有边界的目标，写给一个完全没有你上下文的人也能动手。不要一次交好几件事。
- `agent` 指名，或留空让 Steve 按 `requires` 放置。
- `refs`：子任务需要的指针——`git <commit-or-branch>`、`blob <digest>`。**传引用，不粘内容**。子任务是全新会话，只有你给它的东西。
- `expect`：一句话说清什么算做好，会成为子任务的验收线。
- 立刻返回 `task_id` 和 `state`。

# 等结果

`steve_await(task_id, wait_seconds?)`：每次最多等 50 秒，`state` 还是 `running` 就再等。`done` / `failed` 后向用户报告它返回的 task_id、agent、node 与结果，原样报，不要补。

# 它在哪跑、结果去哪

- 子任务在**它那台机器的隔离工作树**里跑：从项目当前的主目录快照物化出来，不是你眼前这个目录。你改了没推回主目录（git）的东西它看不到。
- 它的结果绑到 `steve/<子任务>/result`，排队等你这一回合释放主目录的写锁后**落地**到主目录（三方合并，冲突留档给人）。它不会直接改你的目录。
- 子任务的 token 是它自己的：不能冒充你说话；它发的卡片会标"受某某委派"。
- **它花的是你的任务预算**。委派你做不了的，不是你懒得做的。

# 什么时候不委派

- 你这台机器就能做的：自己做。
- 只是想"并行快一点"：先问自己合并成本；两个人改同一批路径会冲突。
- 需要用户决定的事：问用户，不是交给另一个 agent 猜。
