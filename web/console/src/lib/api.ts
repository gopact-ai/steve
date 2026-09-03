import type { Conversation, ConversationContext, HistoryEntry, Reply, Snapshot, Suggestion, Usage, Verb } from "./types";

// The token guards everything: it rides as a bearer header on requests and
// as a query parameter on the event stream, which cannot carry headers.
const params = new URLSearchParams(window.location.search);
const fromURL = params.get("token") || "";
if (fromURL) sessionStorage.setItem("steve.token", fromURL);
export const token = fromURL || sessionStorage.getItem("steve.token") || "";

const headers: Record<string, string> = token ? { Authorization: `Bearer ${token}` } : {};
const q = token ? `?token=${encodeURIComponent(token)}` : "";

async function json<T>(res: Response): Promise<T> {
    if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `${res.status} ${res.statusText}`);
    }
    return (await res.json()) as T;
}

export const emptySnapshot: Snapshot = {
    at: "", hub: { node: "", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [],
    facts: { reservations: [], attestations: [], replicas: [], disclosures: [], effects: [], grants: [] },
    inbox: [], schedules: [], sources: [], usage: emptyUsage(),
};

export function emptyUsage(): Usage { return { by_day: [], by_agent: [], by_model: [], total: { key: "total", tokens: {}, seconds: 0, attempts: 0 } }; }

export async function fetchState(): Promise<Snapshot> {
    const s = await json<Snapshot>(await fetch(`./state${q}`, { headers }));
    s.nodes ??= []; s.agents ??= []; s.tasks ??= []; s.plans ??= []; s.projects ??= []; s.inbox ??= []; s.schedules ??= []; s.sources ??= [];
    s.usage ??= emptyUsage(); s.usage.by_day ??= []; s.usage.by_agent ??= []; s.usage.by_model ??= []; s.attempts ??= []; s.landings ??= [];
    s.facts ??= { ...emptySnapshot.facts };
    for (const k of ["reservations", "attestations", "replicas", "disclosures", "effects", "grants"] as const) s.facts[k] ??= [];
    for (const p of s.plans) p.steps ??= [];
    return s;
}

export async function fetchReplies(conversation: string): Promise<{ enabled: boolean; replies: Reply[]; conversations?: string[] }> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/replies${q}${sep}conversation=${encodeURIComponent(conversation)}`, { headers }));
}

export async function fetchConversations(): Promise<{ enabled: boolean; conversations: Conversation[] }> {
    return json(await fetch(`./console/conversations${q}`, { headers }));
}

export async function fetchContext(conversation: string): Promise<{ enabled: boolean; context?: ConversationContext }> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/context${q}${sep}conversation=${encodeURIComponent(conversation)}`, { headers }));
}

export async function fetchSuggest(conversation: string, line: string): Promise<{ suggestions: Suggestion[] }> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/suggest${q}${sep}conversation=${encodeURIComponent(conversation)}&q=${encodeURIComponent(line)}`, { headers }));
}

export async function fetchHistory(before: number, limit = 60): Promise<{ entries: HistoryEntry[]; next: number }> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./history${q}${sep}before=${before}&limit=${limit}`, { headers }));
}

export async function fetchVerbs(): Promise<{ verbs: Verb[] }> {
    return json(await fetch(`./console/verbs${q}`, { headers }));
}

// commandID is the idempotency key a line carries: a retry or a second
// tab sending the same key gets the first answer, and nothing runs twice.
function commandID(): string {
    try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(16).slice(2)}`; }
}

export async function send(conversation: string, input: string): Promise<Reply> {
    const res = await fetch(`./console/send${q}`, {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: JSON.stringify({ conversation, input, command_id: commandID() }),
    });
    const data = (await res.json().catch(() => ({}))) as { reply?: Reply; error?: string };
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data.reply as Reply;
}

export const eventsURL = () => `./events${q}`;
export const when = (t?: string) => (t ? new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "");
export const short = (s?: string, n = 12) => (s ? s.slice(0, n) : "");
export const relative = (t?: string) => {
    if (!t) return "";
    const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000));
    if (s < 60) return `${s}s ago`;
    if (s < 3600) return `${Math.round(s / 60)}m ago`;
    if (s < 86400) return `${Math.round(s / 3600)}h ago`;
    return `${Math.round(s / 86400)}d ago`;
};

export interface AddNodeResult { name: string; token: string; command: string; note?: string }
export async function addNode(req: { name: string; addr: string; level: string }): Promise<AddNodeResult> {
    return json(await fetch(`./console/nodes${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(req) }));
}
export async function addAgent(req: { id: string; harness: string; node?: string; model?: string }): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/agents${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(req) }));
}

export interface HarnessSetting { command: string; args?: string[]; env?: string[]; process_dir?: string; models?: string[] }
export interface MCPSetting { type: string; command?: string; args?: string[]; env?: Record<string, string>; url?: string; headers?: Record<string, string> }
export interface NodeSettings {
    harnesses: Record<string, HarnessSetting>; tools: string[]; mcp_servers: Record<string, MCPSetting>; declares: string[]; capabilities: string[]; external_broker?: boolean;
}
export async function fetchNodeSettings(name: string): Promise<{ settings: NodeSettings }> {
    return json(await fetch(`./console/nodes/${encodeURIComponent(name)}/settings${q}`, { headers }));
}
export async function saveNodeSettings(name: string, settings: NodeSettings): Promise<{ settings: NodeSettings }> {
    return json(await fetch(`./console/nodes/${encodeURIComponent(name)}/settings${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(settings) }));
}

export async function addProject(req: { id: string; node?: string; path: string; repo: string; level: string }): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/projects${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(req) }));
}

export async function removeProject(id: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/projects/${encodeURIComponent(id)}${q}`, { method: "DELETE", headers }));
}

