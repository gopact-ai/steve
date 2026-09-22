import type { Conversation, Reply } from "../types";
import { request } from "../http";

export interface ChannelConversationHistory { conversation: Conversation; replies: Reply[]; next_cursor?: string }

export function fetchChannelConversation(id: string, cursor?: string, signal?: AbortSignal) {
    const query = new URLSearchParams({ limit: "50" });
    if (cursor) query.set("cursor", cursor);
    return request<ChannelConversationHistory>(`/console/channel-conversations/${encodeURIComponent(id)}?${query}`, { signal, cache: "no-store" });
}
