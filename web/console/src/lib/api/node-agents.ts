import { request } from "../http";
export interface NodeAgentCandidate { id: string; name: string; harness: string; executable?: string; adapter?: string; installed: boolean; requires?: string[]; configured: boolean; registered: boolean }
export interface NodeAgentDiscovery { revision: string; agents: NodeAgentCandidate[] }
export interface NodeAgentRequest { candidate_id: string; agent_id: string; expected_revision: string }
export interface NodeAgentEnrollment { candidate_id: string; agent_id?: string; harness: string; revision: string; registered: boolean }
export const discoverNodeAgents = (node: string, signal?: AbortSignal) => request<NodeAgentDiscovery>(`/console/nodes/${encodeURIComponent(node)}/agents`, { signal, cache: "no-store" });
export const enrollNodeAgent = (node: string, body: NodeAgentRequest) => request<NodeAgentEnrollment>(`/console/nodes/${encodeURIComponent(node)}/agents`, { method: "POST", body });
