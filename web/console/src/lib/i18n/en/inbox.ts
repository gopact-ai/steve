import type { inboxZh } from "../zh/inbox.ts";

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
