import type { Snapshot, Usage } from "../types";
import { request } from "../http";
import { LocalizedError, translate } from "../i18n";
export const emptySnapshot: Snapshot = {
    at: "", hub: { node: "", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [],
    facts: { reservations: [], attestations: [], replicas: [], disclosures: [], effects: [], grants: [] },
    inbox: [], schedules: [], sources: [], usage: emptyUsage(),
};

export function emptyUsage(): Usage { return { by_day: [], by_agent: [], by_model: [], total: { key: "total", tokens: {}, seconds: 0, attempts: 0 } }; }

export async function fetchState(signal?: AbortSignal): Promise<Snapshot> {
    const s = await request<Snapshot>("/state", { signal });
    s.nodes ??= []; s.agents ??= []; s.tasks ??= []; s.plans ??= []; s.projects ??= []; for (const p of s.projects) p.workspaces ??= []; s.inbox ??= []; s.schedules ??= []; s.sources ??= [];
    s.usage ??= emptyUsage(); s.usage.by_day ??= []; s.usage.by_agent ??= []; s.usage.by_model ??= []; s.attempts ??= []; s.landings ??= [];
    s.facts ??= { ...emptySnapshot.facts };
    for (const k of ["reservations", "attestations", "replicas", "disclosures", "effects", "grants"] as const) s.facts[k] ??= [];
    for (const p of s.plans) p.steps ??= [];
    return s;
}

export interface AddNodeResult { name: string; token: string; command: string; note?: string }
export interface HarnessSetting { command: string; adapter?: string; slots?: number; permission?: string; args?: string[]; env?: string[]; process_dir?: string; models?: string[] }
export interface MCPSetting { type: string; command?: string; args?: string[]; env?: Record<string, string>; url?: string; headers?: Record<string, string> }
export interface NodeSettings {
	 revision?: string;
    harnesses: Record<string, HarnessSetting>; tools: string[]; mcp_servers: Record<string, MCPSetting>; declares: string[]; capabilities: string[]; external_broker?: boolean;
}
export interface AgentSpec { harness: string; node?: string; model?: string; options?: Record<string, string>; about?: string; requires: string[]; mcp_servers: string[] }

export const addNode = (body: { name: string; addr: string; level: string }) => request<AddNodeResult>("/console/nodes", { method: "POST", body });
export const addAgent = (body: { id: string; harness: string; node?: string; model?: string }) => request<{ ok: boolean }>("/console/agents", { method: "POST", body });
export const fetchNodeSettings = (name: string, signal?: AbortSignal) => request<{ settings: NodeSettings }>(`/console/nodes/${encodeURIComponent(name)}/settings`, { signal });
export const saveNodeSettings = (name: string, settings: NodeSettings) => {
    if (!settings.revision) return Promise.reject(new LocalizedError((locale) => translate(locale, "settingsEditor.reloadBeforeSaving")));
    return request<{ settings: NodeSettings }>(`/console/nodes/${encodeURIComponent(name)}/settings`, { method: "PUT", body: settings });
};
export const updateAgent = (id: string, body: AgentSpec) => request<{ ok: boolean }>(`/console/agents/${encodeURIComponent(id)}`, { method: "PUT", body });
export const removeAgent = (id: string) => request<{ ok: boolean }>(`/console/agents/${encodeURIComponent(id)}`, { method: "DELETE" });
export const removeNode = (name: string) => request<{ ok: boolean }>(`/console/nodes/${encodeURIComponent(name)}`, { method: "DELETE" });
