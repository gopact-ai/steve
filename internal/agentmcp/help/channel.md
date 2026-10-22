# 会话消息

最终回答由 Steve 投递。需要让用户提前知道进展、产物或决定时，可以发送进度消息；同一事项使用更新，过时消息可以撤回。

```text
channel_send(content, channel?, format?, progress?) -> message_id
channel_update(message_id, content, channel?, progress?)
channel_recall(message_id, channel?)
```

省略 channel 使用当前绑定通道，`steve_context` 可查询。没有显式通道的授权锚点使用 `gateway.default_channel`：启用飞书时默认 feishu，否则 console。

指定 channel 必须与授权目标一致，不能选择任意收件人。更新和撤回使用发送回执的原目标。message_id 是本 agent、本回合的不透明句柄，不能借用其他回合的消息身份。

format 默认为 markdown，也支持 text；更新保留原格式。通道负责渲染和投递，不支持的操作明确报错。progress 可用 `2/3` 等简短进度。提及用户、委派身份和最终回复由平台处理。
