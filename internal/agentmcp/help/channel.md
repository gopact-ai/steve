# 会话通道消息

- 大多数回合不需要中间消息：最终回答由平台投递。
- 阶段结论、可交付产物或需要提前沟通的决定，可以用一条进度消息告知用户。
- 多阶段工作先 `channel_send`，再用 `channel_update` 更新同一条消息。`progress` 可以写成 `2/3`。
- 消息过时或有误时，用 `channel_recall` 撤回本轮自己发送的消息。

# 工具

```
channel_send(content, channel?, format?, progress?) -> message_id
channel_update(message_id, content, channel?, progress?)
channel_recall(message_id, channel?)
```

省略 `channel` 时使用当前会话已绑定的通道。没有显式通道的授权锚点使用 `gateway.default_channel`，默认值是 `feishu`；控制台会话显式绑定 `console`。`steve_context` 返回当前 channel。

指定 `channel` 必须与当前授权目标一致；它不提供任意收件人选择。编辑和撤回固定使用发送回执的目标，不能改投其他通道。

`format` 默认 `markdown`，也支持 `text`。每个通道负责自己的呈现：飞书渲染卡片，控制台呈现对话消息。更新保留原格式；不支持某种更新的通道会明确返回错误。

返回的 `message_id` 是不透明句柄，专属于本 Agent、本回合。发送方和委派归属由平台补充。提及用户和投递最终回答由平台负责。
