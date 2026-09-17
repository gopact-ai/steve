import { request } from "../http";
export interface NodeAgentSelector { id: string; name: string; category?: string; current?: string; choices?: string[]; values?: string[] }
export interface NodeAgentCandidate { id: string; name: string; harness: string; executable?: string; adapter?: string; installed: boolean; requires?: string[]; configured: boolean; registered: boolean; model?: string; models?: string[]; selectors?: NodeAgentSelector[] }
export interface NodeAgentDiscovery { revision: string; agents: NodeAgentCandidate[] }
export interface NodeAgentChoice { candidate_id: string; agent_id: string; about?: string; model?: string; options?: Record<string, string>; default?: boolean }
export interface NodeAgentRequest { agents: NodeAgentChoice[]; expected_revision: string }
export interface NodeAgentEnrollment { candidate_id: string; agent_id?: string; agents?: string[]; harness: string; revision: string; registered: boolean }
export const discoverNodeAgents = (node: string, signal?: AbortSignal) => request<NodeAgentDiscovery>(`/console/nodes/${encodeURIComponent(node)}/agents`, { signal, cache: "no-store" });
export const enrollNodeAgents = (node: string, body: NodeAgentRequest) => request<NodeAgentEnrollment>(`/console/nodes/${encodeURIComponent(node)}/agents`, { method: "POST", body });
