import { request } from "../http";
export interface SSHCandidate { alias: string; host_name: string; user?: string; port: number; proxy_jump?: string; has_proxy_command: boolean; has_identity_file: boolean; conditional: boolean; source: string; line: number }
export interface SSHWarning { source: string; line?: number; code: string; message: string }
export interface SSHDiscovery { candidates: SSHCandidate[]; warnings: SSHWarning[]; revision: string }
export interface SSHStep { id: string; status: string; message: string; suggestion?: string }
export interface SSHCheck { candidate: SSHCandidate; reachable: boolean; address?: string; os?: string; arch?: string; tools: { name: string; available: boolean }[]; existing_installation: boolean; existing_paths?: string[]; existing_node?: { name?: string; owner?: string }; installation_mode?: "peer" | "executor"; steps: SSHStep[]; checked_at: string }
export interface SSHInstallRequest { alias: string; name: string; addr: string; level: string; raft_addr?: string; workspace_dir?: string }
export interface SSHPlan { id: string; request: SSHInstallRequest; check: SSHCheck; script: string; effects: string[]; steps: SSHStep[]; ready: boolean; expires_at: string; binary?: { os: string; arch: string; sha256: string; size: number } }
export interface SSHLogLine { at: string; stream: "steve" | "stdout" | "stderr" | string; text: string }
export interface SSHInstallResult { plan_id: string; name: string; node_id?: string; registered: boolean; connected: boolean; status: string; steps: SSHStep[]; phase?: string; phases?: string[]; log?: SSHLogLine[] }
export const discoverSSH = (signal?: AbortSignal) => request<SSHDiscovery>("/console/ssh/candidates", { signal, cache: "no-store" });
export const checkSSH = (alias: string) => request<SSHCheck>("/console/ssh/check", { method: "POST", body: { alias } });
export const planSSH = (body: SSHInstallRequest) => request<SSHPlan>("/console/ssh/plans", { method: "POST", body });
export const statusSSH = (id: string, signal?: AbortSignal) => request<SSHInstallResult>(`/console/ssh/plans/${encodeURIComponent(id)}`, { signal, cache: "no-store" });
export const installSSH = (id: string) => request<SSHInstallResult>(`/console/ssh/plans/${encodeURIComponent(id)}/install`, { method: "POST" });
export const abandonSSH = (id: string) => request<{ plan_id: string; abandoned: boolean }>(`/console/ssh/plans/${encodeURIComponent(id)}`, { method: "DELETE" });
export interface SSHListingEntry { name: string; path: string }
export interface SSHListing { path: string; display: string; home: string; parent?: string; requested?: string; writable: boolean; entries: SSHListingEntry[]; truncated?: boolean }
// A directory is browsed on a machine being enrolled, named by its SSH
// alias, or on one already in the cluster, named by node ID.
export type BrowseTarget = { alias: string; node?: never } | { node: string; alias?: never };
export const browseSSH = (target: BrowseTarget, path: string, signal?: AbortSignal) => request<SSHListing>("/console/ssh/browse", { method: "POST", body: { ...target, path }, signal });
export const upgradeSSH = (nodeID: string) => request<SSHInstallResult>(`/console/ssh/upgrades/${encodeURIComponent(nodeID)}`, { method: "POST" });
export const upgradeStatusSSH = (nodeID: string, signal?: AbortSignal) => request<SSHInstallResult>(`/console/ssh/upgrades/${encodeURIComponent(nodeID)}`, { signal, cache: "no-store" });
// SSHAutoStart is how automatic start stands for a machine on the node
// serving the console: what it is doing, how many starts it made since the
// machine last stayed online, why it stopped when it did, and what clears
// what blocks it.
export interface SSHAutoStart { state: string; attempts: number; limit: number; last_error?: string; suggestion?: string; reason?: string; last_at?: string; next_at?: string; offline_since?: string }
// SSHRestartState is a machine's latest restart on that node, running or
// settled, whether automatic start ran it, and how automatic start stands.
export interface SSHRestartState { restart?: SSHInstallResult; automatic?: boolean; auto_start?: SSHAutoStart }
export const restartSSH = (nodeID: string) => request<SSHInstallResult>(`/console/ssh/restarts/${encodeURIComponent(nodeID)}`, { method: "POST" });
export const restartStatusSSH = (nodeID: string, signal?: AbortSignal) => request<SSHRestartState>(`/console/ssh/restarts/${encodeURIComponent(nodeID)}`, { signal, cache: "no-store" });
