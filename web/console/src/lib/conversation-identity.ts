import type { Conversation } from "./types";

export type ConversationTransport = "console" | "feishu";
// Missing transport is the existing Console directory contract, never an ID heuristic.
export const conversationTransport = (conversation: Pick<Conversation, "transport">): ConversationTransport => conversation.transport === "feishu" ? "feishu" : "console";
export const conversationKey = (conversation: Pick<Conversation, "id" | "transport">) => JSON.stringify([conversationTransport(conversation), conversation.id]);
export const conversationURL = (id: string, transport: ConversationTransport = "console") => `/console?conversation=${encodeURIComponent(id)}${transport === "feishu" ? "&transport=feishu" : ""}`;

export const conversationExecution = (conversation: Pick<Conversation, "execution" | "running">) => conversation.execution || (conversation.running ? "running" : "idle");
