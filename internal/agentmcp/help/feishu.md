# 什么时候一张卡有用

- 大多数回合不需要中间消息：最终回答由平台投递，用户会看到。
- 值得早点让用户知道的：一个阶段的结论、产出了一个产物、一个会影响方向的决定。逐步旁白反而淹没这些。
- 多阶段工作用**一张**会演进的进度卡：先 `feishu_send` 一张，之后 `feishu_update(message_id, content, progress?)` 改它。一张卡在同一个位置更新，比一串新卡好读。
- `progress` 写成 `2/3`（第几阶段 / 共几阶段），卡片徽章会显示。
- 发错了或过时了：`feishu_recall(message_id)` 撤回本轮你自己发的那条。

# 工具

```
feishu_send(content, progress?)            -> message_id
feishu_update(message_id, content, progress?)
feishu_recall(message_id)
```

`content` 是 markdown。卡片会带上你是谁：受委派的子任务发的卡会标"受某某委派"。@ 人的事平台在最终答案卡上做。

# 控制台

用户从网页控制台而不是飞书发来的会话里，这些工具同样存在：卡片会显示在控制台的对话里，用法不变。
