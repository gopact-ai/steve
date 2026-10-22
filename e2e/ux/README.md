# 飞书交互验收

这些脚本连接正在运行的 Steve 和真实飞书测试群，检查用户可见的消息、卡片与任务状态。它们会发送真实消息并触发 agent 执行，只用于明确授权的测试环境。

需要运行中的单实例网关、以测试提问者身份授权的 `lark-cli`，以及 `STEVE_UX_CHAT`（群 chat ID）和 `STEVE_UX_BOT`（bot open ID）。使用前核对脚本的目标配置。

| 脚本 | 检查 |
| --- | --- |
| `queue-interrupt.sh` | 排队与打断 |
| `milestone-recall.sh` | 进度消息、撤回与最终通知 |
| `evolving-card.sh` | 同一张卡片更新 |
| `order-badge-tail.sh` | 消息顺序、进度标记与旧卡片处理 |
| `task-verbs.sh` | 暂停、继续与取消 |
| `schedule.sh` | 一次性及周期定时工作 |
| `offline-reminder.sh` | 长回合结束提醒；需要对应的 `offline_reminder_after` |

输出包含观测原文与 `*-PASS` / `CHECK:` 结果。记录原任务和会话身份，失败后核对仍在执行的工作；脚本退出不证明服务端任务停止。
