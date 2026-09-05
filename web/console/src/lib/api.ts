import type { Exchange, AttemptView, ChangeIndex, QuoteRef, Selectors, FileDiff, FileView, HomeView, Task, TaskDetail, TaskMetaPatch, TreeView, MachineSkills, MCPRegistryEntry, MCPView, SkillDoc, SkillSource, SkillsView, Conversation, ConversationContext, HistoryEntry, Reply, Snapshot, Suggestion, Usage, Verb } from "./types";

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
    s.nodes ??= []; s.agents ??= []; s.tasks ??= []; s.plans ??= []; s.projects ??= []; for (const p of s.projects) p.workspaces ??= []; s.inbox ??= []; s.schedules ??= []; s.sources ??= [];
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

export async function send(conversation: string, input: string, quotes?: QuoteRef[]): Promise<Reply> {
    const res = await fetch(`./console/send${q}`, {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: JSON.stringify({ conversation, input, command_id: commandID(), quotes: quotes?.map((x) => ({ conversation: x.conversation, reply_id: x.reply_id })) }),
    });
    const data = (await res.json().catch(() => ({}))) as { reply?: Reply; error?: string };
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data.reply as Reply;
}

export async function fetchQueue(conversation: string): Promise<{ queue: Exchange[] }> {
    return json(await fetch(`./console/queue${q}${q ? "&" : "?"}conversation=${encodeURIComponent(conversation)}`, { headers }));
}

export async function enqueue(conversation: string, input: string, quotes?: QuoteRef[]): Promise<Exchange> {
    return json(await fetch(`./console/queue${q}`, {
        method: "POST", headers: { ...headers, "Content-Type": "application/json" },
        body: JSON.stringify({ conversation, input, quotes: quotes?.map(({ conversation, reply_id }) => ({ conversation, reply_id })) }),
    }));
}

export async function deleteQueued(id: string): Promise<void> {
    await json(await fetch(`./console/queue/${encodeURIComponent(id)}${q}`, { method: "DELETE", headers }));
}

export async function editQueued(id: string, input: string): Promise<Exchange> {
    return json(await fetch(`./console/queue/${encodeURIComponent(id)}${q}`, {
        method: "PATCH", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ input }),
    }));
}

export async function steerQueued(id: string): Promise<Exchange> {
    return json(await fetch(`./console/queue/${encodeURIComponent(id)}/steer${q}`, { method: "POST", headers }));
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

export async function updateConversation(id: string, patch: { title?: string; archived?: boolean }): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/conversations/${encodeURIComponent(id)}${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(patch) }));
}
export async function addWorkspace(project: string, req: { node?: string; path: string; origin: "adopt" | "clone" }): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/projects/${encodeURIComponent(project)}/workspaces${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(req) }));
}
export async function removeWorkspace(project: string, node: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/projects/${encodeURIComponent(project)}/workspaces/${encodeURIComponent(node)}${q}`, { method: "DELETE", headers }));
}
export async function removeProject(id: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/projects/${encodeURIComponent(id)}${q}`, { method: "DELETE", headers }));
}

export interface AgentSpec { harness: string; node?: string; model?: string; options?: Record<string, string>; about?: string; requires: string[]; mcp_servers: string[] }
export async function updateAgent(id: string, spec: AgentSpec): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/agents/${encodeURIComponent(id)}${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(spec) }));
}
export async function removeAgent(id: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/agents/${encodeURIComponent(id)}${q}`, { method: "DELETE", headers }));
}


export async function fetchSkills(): Promise<SkillsView> { return json(await fetch(`./console/skills${q}`, { headers })); }
export async function fetchSkill(name: string): Promise<SkillDoc> { return json(await fetch(`./console/skills/${encodeURIComponent(name)}${q}`, { headers })); }
export async function setSkill(name: string, enabled: boolean): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/skills/${encodeURIComponent(name)}${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ enabled }) }));
}
export async function addSkillPath(path: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/skills/paths${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ path }) }));
}
export async function removeSkillPath(path: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/skills/paths${q}${q ? "&" : "?"}path=${encodeURIComponent(path)}`, { method: "DELETE", headers }));
}
export async function fetchHome(): Promise<HomeView> { return json(await fetch(`./console/home${q}`, { headers })); }
export async function fetchSelectors(conversation: string, agent: string): Promise<Selectors> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/selectors${q}${sep}conversation=${encodeURIComponent(conversation)}&agent=${encodeURIComponent(agent)}`, { headers }));
}
export async function setPreferences(conversation: string, agent: string, patch: Record<string, string>): Promise<{ ok: boolean; note?: string }> {
    return json(await fetch(`./console/preferences${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ conversation, agent, patch }) }));
}
export async function fetchTask(id: string): Promise<TaskDetail> {
    return json(await fetch(`./console/tasks/${encodeURIComponent(id)}${q}`, { headers }));
}
export async function patchTaskMeta(id: string, patch: TaskMetaPatch): Promise<Task> {
    return json(await fetch(`./console/tasks/${encodeURIComponent(id)}/meta${q}`, { method: "PATCH", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(patch) }));
}
export async function fetchTaskAttempts(id: string): Promise<AttemptView[]> {
    return json(await fetch(`./console/tasks/${encodeURIComponent(id)}/attempts${q}`, { headers }));
}
export async function fetchAttemptTree(attempt: string, dir: string): Promise<TreeView> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/attempts/${encodeURIComponent(attempt)}/tree${q}${sep}path=${encodeURIComponent(dir)}`, { headers }));
}
export async function fetchAttemptFile(attempt: string, path: string): Promise<FileView> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/attempts/${encodeURIComponent(attempt)}/file${q}${sep}path=${encodeURIComponent(path)}`, { headers }));
}
export async function fetchAttemptChanges(attempt: string): Promise<ChangeIndex> {
    return json(await fetch(`./console/attempts/${encodeURIComponent(attempt)}/changes${q}`, { headers }));
}
export async function fetchAttemptDiff(attempt: string, path: string): Promise<FileDiff> {
    const sep = q ? "&" : "?";
    return json(await fetch(`./console/attempts/${encodeURIComponent(attempt)}/diff${q}${sep}path=${encodeURIComponent(path)}`, { headers }));
}
export async function saveProjectMemory(project: string, text: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/memory/${encodeURIComponent(project)}${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ text }) }));
}
export async function saveHomeFile(name: string, text: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/home/${encodeURIComponent(name)}${q}`, { method: "PUT", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ text }) }));
}

export async function addSkillSource(spec: string): Promise<SkillSource> {
    return json(await fetch(`./console/skills/sources${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ spec }) }));
}
export async function updateSkillSources(): Promise<{ sources: SkillSource[] }> {
    return json(await fetch(`./console/skills/sources/update${q}`, { method: "POST", headers }));
}
export async function removeSkillSource(slug: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/skills/sources/${encodeURIComponent(slug)}${q}`, { method: "DELETE", headers }));
}

export async function fetchMachineSkills(): Promise<{ machines: MachineSkills[] }> { return json(await fetch(`./console/skills/machines${q}`, { headers })); }
export async function refreshMachineSkills(): Promise<{ machines: MachineSkills[] }> { return json(await fetch(`./console/skills/machines/refresh${q}`, { method: "POST", headers })); }
export async function importSkill(node: string, path: string): Promise<{ ok: boolean; name: string }> {
    return json(await fetch(`./console/skills/import${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ node, path }) }));
}

export async function fetchMCP(): Promise<MCPView> { return json(await fetch(`./console/mcp${q}`, { headers })); }
export async function probeMCP(node: string, name: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/mcp/probe${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ node, name }) }));
}
export async function adoptMCP(node: string, source: string, name: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/mcp/adopt${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify({ node, source, name }) }));
}
export async function removeMCP(node: string, name: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/mcp${q}${q ? "&" : "?"}node=${encodeURIComponent(node)}&name=${encodeURIComponent(name)}`, { method: "DELETE", headers }));
}
export async function searchMCPRegistry(query: string): Promise<{ entries: MCPRegistryEntry[] }> {
    return json(await fetch(`./console/mcp/registry${q}${q ? "&" : "?"}q=${encodeURIComponent(query)}`, { headers }));
}
export async function installMCP(req: { node: string; name: string; entry: string; package?: number; remote?: number; values: Record<string, string> }): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/mcp/install${q}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(req) }));
}

export async function removeNode(name: string): Promise<{ ok: boolean }> {
    return json(await fetch(`./console/nodes/${encodeURIComponent(name)}${q}`, { method: "DELETE", headers }));
}
