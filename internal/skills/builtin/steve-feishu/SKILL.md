---
name: steve-feishu
description: 在 Steve 里通过飞书给用户看进度：feishu_send 发里程碑卡、feishu_update 改同一张进度卡、feishu_recall 撤回本轮发错的消息；什么时候该发、什么时候不该发、进度徽章怎么写、为什么不要 @ 人、最终答案为什么不用它发。做长活或多阶段工作时读这个。
---

# 原则

- 大多数回合**零条**中间消息。最终答案由平台投递，不要用工具重复发、也不要用它代替。
- 发卡片的时机：一个阶段的结论、产出了一个产物、一个值得早点让用户知道的决定。不要逐步旁白。
- 多阶段工作用**一张**会演进的进度卡：先 `feishu_send` 一张，之后 `feishu_update(message_id, content, progress?)` 改它，不要每个阶段新发一张。
- `progress` 写成 `2/3`（第几阶段 / 共几阶段），卡片徽章会显示。
- 不要 @ 任何人；@ 是平台最终答案卡专用的。
- `feishu_recall(message_id)` 撤回本轮你自己发的、后来发现错了或过时的消息。

# 工具

```
feishu_send(content, progress?)            -> message_id
feishu_update(message_id, content, progress?)
feishu_recall(message_id)
```

`content` 是 markdown。卡片会带上你是谁：受委派的子任务发的卡会标"受某某委派"。

# 控制台

用户从网页控制台而不是飞书发来的会话里，这些工具同样存在：卡片会显示在控制台的对话里，规则不变。
