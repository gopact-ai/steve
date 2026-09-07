export const inboxZh = {
    "inbox.answerInConversation": "去会话答复",
    "inbox.title": "待处理",
    "inbox.description": "核实隔离执行、确认数据披露请求，以及核对结果未知的外部操作。",
    "inbox.incomplete": "待处理信息尚未完整读取，当前列表可能不完整。",
    "inbox.empty": "暂无待处理请求",
    "inbox.emptyHint": "新请求会显示在这里。",
    "inbox.machine": "机器：",
    "inbox.directory": "目录：",
    "inbox.execution": "执行：",
    "inbox.writerHint": "需在指定机器核实原进程已退出。断连、关闭流或重启节点不能代替核实。多节点系统请回到原任务会话处理 Ask User 恢复问题，重新检查或等待。独立部署请参照",
    "inbox.recoveryGuide": "隔离恢复说明",
    "inbox.writerSteps": "，停止 Steve 服务后，通过 ledger confirm-stopped 记录证据并重新对账。多节点系统不支持此离线命令。",
    "inbox.otherChannels": "Agent 提问与飞书接入申请，请在对应会话中处理。",
    "inbox.projectSuffix": " · 项目 {project}",
    "inbox.taskSuffix": " · 任务 #{task}"
} as const;

export const inboxEn = {
    "inbox.answerInConversation": "Respond in conversation",
    "inbox.title": "Inbox",
    "inbox.description": "Verify quarantined executions, approve disclosures and reconcile unknown external actions.",
    "inbox.incomplete": "Attention data could not be fully read. This list may be incomplete.",
    "inbox.empty": "No pending requests",
    "inbox.emptyHint": "New requests will appear here.",
    "inbox.machine": "Machine: ",
    "inbox.directory": "Directory: ",
    "inbox.execution": "Execution: ",
    "inbox.writerHint": "Verify on the specified machine that the original process exited. Disconnection, stream closure or a node restart is not proof. In a multi-node system, return to the original task conversation and use its Ask User recovery question to check again or wait. For standalone deployments, follow the ",
    "inbox.recoveryGuide": "quarantine recovery guide",
    "inbox.writerSteps": " to stop the Steve service, record evidence with ledger confirm-stopped and reconcile again. This offline command is unavailable to multi-node systems.",
    "inbox.otherChannels": "Handle agent questions and Feishu connection requests in their conversations.",
    "inbox.projectSuffix": " · Project {project}",
    "inbox.taskSuffix": " · Task #{task}"
} as const satisfies Record<keyof typeof inboxZh, string>;
