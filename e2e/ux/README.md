# 真机 UX 场景验证

这些脚本对**在跑的网关**和**真实飞书测试群**执行端到端场景，并断言用户可见面：
卡片顺序、状态（完成/已取消）、@ 目标、`updated`/`deleted` 标志、阶段角标、时序。
它们是 `go test` wire 防线之上的第二层——wire 测试证明机器对，这里证明人看到的对。

前置条件：
- 网关正在运行（单实例！先 `pgrep -af 'steve[0-9]+ run'` 确认）
- `lark-cli` 已授权为提问人身份
- 环境变量可覆盖默认目标：`STEVE_UX_CHAT`（测试群 chat_id）、`STEVE_UX_BOT`（bot open_id）

每个脚本输出逐次轮询的原文与最终 `*-PASS` / `CHECK:` 行——这就是验证记录，
跑完把它贴进当轮的记录/台账。脚本会向测试群发送真实消息。

场景：
- `queue-interrupt.sh` — 排队为默认；`!` 打断替换
- `milestone-recall.sh` — 里程碑卡（不 @ 人）+ channel_recall + 最终卡 @ 提问人
- `evolving-card.sh` — 单卡演进（channel_update），断言恰一张卡且 updated=true
- `order-badge-tail.sh` — 最终卡落在里程碑下方、几分之几角标、同族尾标、旧占位卡撤回
- `task-verbs.sh` — `/tasks pause` 停下当前轮、`resume` 真的续跑、`cancel` 是终局
- `schedule.sh` — `/at` 建的一次性任务到点自己开跑；`/every` 进列表、能取消
- `offline-reminder.sh` — 长轮次结束后补一条 @ 提问人的纯文本提醒
  （需要网关的 `offline_reminder_after` 比这轮耗时短，测试时设 `1m`）
