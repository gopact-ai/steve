import type { Conversation, ConversationContext, Exchange, MaterialRef, QuoteRef, Reply, Selectors, Suggestion, Verb } from "../types";
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
export interface SubmissionSupport { state: "unknown" | "supported" | "unsupported"; checking: boolean; error: string; material_refs?: boolean; interactive_requests?: boolean }
let submissionSupport: SubmissionSupport = { state: "unknown", checking: false, error: "" };
let supportCheck: Promise<SubmissionSupport> | null = null;
let supportGeneration = 0;
const supportListeners = new Set<() => void>();
export const getSubmissionSupport = () => submissionSupport;
export function subscribeSubmissionSupport(listener: () => void) { supportListeners.add(listener); return () => { supportListeners.delete(listener); }; }
function publishSupport(patch: Partial<SubmissionSupport>) {
    const next = { ...submissionSupport, ...patch };
    if (next.state === submissionSupport.state && next.checking === submissionSupport.checking && next.error === submissionSupport.error && next.material_refs === submissionSupport.material_refs && next.interactive_requests === submissionSupport.interactive_requests) return;
    submissionSupport = next;
    for (const listener of supportListeners) listener();
}
export async function fetchQueue(conversation: string, signal?: AbortSignal): Promise<{ queue: Exchange[]; submission_keys?: boolean; material_refs?: boolean; interactive_requests?: boolean }> {
    const generation = ++supportGeneration;
    try {
        const data = await request<{ queue: Exchange[]; submission_keys?: boolean; material_refs?: boolean; interactive_requests?: boolean }>(`/console/queue?${channelQuery(conversation)}`, { signal, cache: "no-store" });
        if (generation === supportGeneration) publishSupport({ state: data.submission_keys === true ? "supported" : "unsupported", error: "", material_refs: data.material_refs === true, interactive_requests: data.interactive_requests === true });
        return data;
    } catch (error) {
        if (generation === supportGeneration && !signal?.aborted) publishSupport({ state: "unknown", error: error instanceof Error ? error.message : String(error) });
        throw error;
    }
}
export function checkSubmissionSupport(): Promise<SubmissionSupport> {
    if (supportCheck) return supportCheck;
    publishSupport({ checking: true });
    // A write depends on its own preflight response, never another poll's
    // mutable display state, even when their completions share a microtask batch.
    supportCheck = fetchQueue("console:main").then(
        (data): SubmissionSupport => ({ state: data.submission_keys === true ? "supported" : "unsupported", checking: false, error: "", material_refs: data.material_refs === true, interactive_requests: data.interactive_requests === true }),
        (error): SubmissionSupport => ({ state: "unknown", checking: false, error: error instanceof Error ? error.message : String(error) }),
    ).finally(() => { supportCheck = null; publishSupport({ checking: false }); });
    return supportCheck;
}
export async function requireSubmissionSupport(): Promise<SubmissionSupport> {
    // Recheck before every write: a restarted/downgraded hub must not inherit
    // the capability of the previous process. Concurrent checks share one GET.
    const support = await checkSubmissionSupport();
    if (support.state === "unsupported") throw new UnsentRequestError("Hub 需更新后才能发送指令。当前仍可查看会话和执行记录。");
    if (support.state !== "supported") throw new UnsentRequestError(`无法确认 Hub 是否支持安全提交，指令尚未发送。${support.error}`);
    return support;
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
export async function enqueue(conversation: string, input: string, quotes?: QuoteRef[], id = commandID(), refs?: MaterialRef[], locale?: string): Promise<Exchange> {
    const support = await requireSubmissionSupport();
    if (refs?.length && !support.material_refs) throw new UnsentRequestError("This Hub does not support material references; the instruction was not sent.");
    const exchange = await request<Exchange>("/console/queue", { method: "POST", body: { conversation, input, command_id: id, refs, locale, quotes: quotes?.map(({ conversation, reply_id }) => ({ conversation, reply_id })) } });
    if (!exchange || typeof exchange.id !== "string" || exchange.conversation !== conversation || exchange.key !== `client:${id}`) throw new Error("Invalid queue acknowledgement");
    return exchange;
}
export const deleteQueued = async (id: string): Promise<void> => { await write(`/console/queue/${encodeURIComponent(id)}`, { method: "DELETE" }); };
export const editQueued = (id: string, input: string) => write<Exchange>(`/console/queue/${encodeURIComponent(id)}`, { method: "PATCH", body: { input } });
export const steerQueued = (id: string) => write<Exchange>(`/console/queue/${encodeURIComponent(id)}/steer`, { method: "POST" });
