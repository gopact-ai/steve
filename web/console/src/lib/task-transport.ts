import type { Task } from "./types";

// Conversation IDs are opaque, even when they happen to start with "console:".
// Missing transport is unknown, not permission to send console control verbs.
export function consoleTaskConversation(task: Pick<Task, "transport" | "channel">): string | null {
    return task.transport === "console" && task.channel ? task.channel : null;
}

// Navigation is not control authority: native conversations have a read-only route.
export function taskConversationAddress(task: Pick<Task, "transport" | "channel">): { id: string; transport: "console" | "feishu" } | null {
    return task.channel && (task.transport === "console" || task.transport === "feishu") ? { id: task.channel, transport: task.transport } : null;
}
