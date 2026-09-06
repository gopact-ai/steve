import type { Conversation, ConversationContext, Exchange, QuoteRef, Reply, Selectors, Suggestion, Verb } from "../types";
import { request, UnsentRequestError } from "../http";

async function write<T>(path: string, options: { method: string; body?: unknown }): Promise<T> {
    await requireSubmissionSupport();
    return request<T>(path, options);
}

const channelQuery = (conversation: string) => `conversation=${encodeURIComponent(conversation)}`;
export const fetchReplies = (conversation: string, signal?: AbortSignal) => request<{ enabled: boolean; replies: Reply[]; conversations?: string[] }>(`/console/replies?${channelQuery(conversation)}`, { signal });
export const fetchConversations = (signal?: AbortSignal) => request<{ enabled: boolean; conversations: Conversation[] }>("/console/conversations", { signal });
export const fetchContext = (conversation: string, signal?: AbortSignal) => request<{ enabled: boolean; context?: ConversationContext }>(`/console/context?${channelQuery(conversation)}`, { signal });
export const fetchSuggest = (conversation: string, line: string, signal?: AbortSignal) => request<{ suggestions: Suggestion[] }>(`/console/suggest?${channelQuery(conversation)}&q=${encodeURIComponent(line)}`, { signal });
export const fetchVerbs = (signal?: AbortSignal) => request<{ verbs: Verb[] }>("/console/verbs", { signal });
export interface SubmissionSupport { state: "unknown" | "supported" | "unsupported"; checking: boolean; error: string }
let submissionSupport: SubmissionSupport = { state: "unknown", checking: false, error: "" };
let supportCheck: Promise<SubmissionSupport> | null = null;
const supportListeners = new Set<() => void>();
export const getSubmissionSupport = () => submissionSupport;
export function subscribeSubmissionSupport(listener: () => void) { supportListeners.add(listener); return () => { supportListeners.delete(listener); }; }
function publishSupport(patch: Partial<SubmissionSupport>) {
    const next = { ...submissionSupport, ...patch };
    if (next.state === submissionSupport.state && next.checking === submissionSupport.checking && next.error === submissionSupport.error) return;
    submissionSupport = next;
    for (const listener of supportListeners) listener();
}
export async function fetchQueue(conversation: string, signal?: AbortSignal): Promise<{ queue: Exchange[]; submission_keys?: boolean }> {
    try {
        const data = await request<{ queue: Exchange[]; submission_keys?: boolean }>(`/console/queue?${channelQuery(conversation)}`, { signal, cache: "no-store" });
        publishSupport({ state: data.submission_keys === true ? "supported" : "unsupported", error: "" });
        return data;
    } catch (error) {
        if (!signal?.aborted) publishSupport({ state: "unknown", error: error instanceof Error ? error.message : String(error) });
        throw error;
    }
}
export function checkSubmissionSupport(): Promise<SubmissionSupport> {
    if (supportCheck) return supportCheck;
    publishSupport({ checking: true });
    supportCheck = fetchQueue("console:main").then(() => submissionSupport, () => submissionSupport).finally(() => { supportCheck = null; publishSupport({ checking: false }); });
    return supportCheck;
}
export async function requireSubmissionSupport(): Promise<void> {
    // Recheck before every write: a restarted/downgraded hub must not inherit
    // the capability of the previous process. Concurrent checks share one GET.
    const support = await checkSubmissionSupport();
    if (support.state === "unsupported") throw new UnsentRequestError("Hub 需更新后才能发送指令。当前仍可查看会话和执行记录。");
    if (support.state !== "supported") throw new UnsentRequestError(`无法确认 Hub 是否支持安全提交，指令尚未发送。${support.error}`);
}
export const updateConversation = (id: string, body: { title?: string; archived?: boolean }) => write<{ ok: boolean }>(`/console/conversations/${encodeURIComponent(id)}`, { method: "PUT", body });
export const fetchSelectors = (conversation: string, agent: string, signal?: AbortSignal) => request<Selectors>(`/console/selectors?${channelQuery(conversation)}&agent=${encodeURIComponent(agent)}`, { signal });
export const setPreferences = (conversation: string, agent: string, patch: Record<string, string>) => write<{ ok: boolean; note?: string }>("/console/preferences", { method: "PUT", body: { conversation, agent, patch } });

function commandID(): string {
    try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(16).slice(2)}`; }
}
export async function send(conversation: string, input: string, quotes?: QuoteRef[], id = commandID()): Promise<Reply> {
    await requireSubmissionSupport();
    const data = await request<{ reply: Reply }>("/console/send", { method: "POST", body: { conversation, input, command_id: id, quotes: quotes?.map(({ conversation, reply_id }) => ({ conversation, reply_id })) } });
    return data.reply;
}
export async function enqueue(conversation: string, input: string, quotes?: QuoteRef[], id = commandID()): Promise<Exchange> {
    await requireSubmissionSupport();
    const exchange = await request<Exchange>("/console/queue", { method: "POST", body: { conversation, input, command_id: id, quotes: quotes?.map(({ conversation, reply_id }) => ({ conversation, reply_id })) } });
    if (!exchange || typeof exchange.id !== "string" || exchange.conversation !== conversation || exchange.key !== `client:${id}`) throw new Error("Invalid queue acknowledgement");
    return exchange;
}
export const deleteQueued = async (id: string): Promise<void> => { await write(`/console/queue/${encodeURIComponent(id)}`, { method: "DELETE" }); };
export const editQueued = (id: string, input: string) => write<Exchange>(`/console/queue/${encodeURIComponent(id)}`, { method: "PATCH", body: { input } });
export const steerQueued = (id: string) => write<Exchange>(`/console/queue/${encodeURIComponent(id)}/steer`, { method: "POST" });
