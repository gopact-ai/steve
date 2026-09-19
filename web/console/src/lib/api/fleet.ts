import type { Snapshot } from "../types";
import { request } from "../http";
import { LocalizedError, translate } from "../i18n";
export const emptySnapshot: Snapshot = {
    task_coverage: { total: 0, live: 0, closed: 0, roots: 0, completed_roots: 0, cancelled_roots: 0, paused_roots: 0, included: 0, recent_limit: 20, recent_closed: 0, has_more_closed: false },
    plan_coverage: { total: 0, included: 0, has_more: false },
    at: "", hub: { node: "", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [], conflicts: [],
    facts: { reservations: [], attestations: [], replicas: [], disclosures: [], effects: [], grants: [] },
    inbox: [], schedules: [], sources: [],
};

export async function fetchState(signal?: AbortSignal): Promise<Snapshot> {
    const s = await request<Snapshot>("/state", { signal });
    s.nodes ??= []; s.agents ??= []; s.tasks ??= []; s.plans ??= []; s.projects ??= []; for (const p of s.projects) p.workspaces ??= []; s.inbox ??= []; s.schedules ??= []; s.sources ??= [];
    s.attempts ??= []; s.landings ??= [];
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
