import { request } from "../http";

export interface DesktopStatus { enabled: boolean; node_id?: string; setup_required: boolean; agent_count: number; default_agent?: string }
export interface DesktopAgentCandidate { id: string; name: string; harness: string; executable?: string; installed: boolean; requires?: string[]; registered: boolean }
export const fetchDesktopStatus = (signal?: AbortSignal) => request<DesktopStatus>("/console/desktop", { signal, cache: "no-store" });
export const discoverDesktopAgents = (signal?: AbortSignal) => request<{ agents: DesktopAgentCandidate[] }>("/console/desktop/agents", { signal, cache: "no-store" });
export const enrollDesktopAgents = (agent_ids: string[]) => request<DesktopStatus>("/console/desktop/agents", { method: "POST", body: { agent_ids } });
