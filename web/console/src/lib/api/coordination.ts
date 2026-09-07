import { request } from "../http";
export interface CoordinationNode { id: string; name: string; local: boolean; online: boolean; voter: boolean; auto_eligible: boolean; ready: boolean; reason?: string }
export interface CoordinationEvent { id: string; at: string; kind: string; actor: string; from?: string; to?: string; reason?: string }
export interface CoordinationView { enabled: boolean; cluster_id?: string; node_id?: string; coordinator_id?: string; epoch: number; revision: number; authoritative: boolean; observed_at: string; auto_failover: boolean; ready: boolean; reason?: string; nodes: CoordinationNode[]; events: CoordinationEvent[] }
export interface TransferCommand { command_id: string; expected_epoch: number; target_node_id: string }
export interface PolicyCommand { command_id: string; expected_revision: number; enabled: boolean }
export interface EligibilityCommand { command_id: string; expected_revision: number; node_id: string; eligible: boolean }
export type CoordinationCommand = { kind: "transfer"; body: TransferCommand } | { kind: "policy"; body: PolicyCommand } | { kind: "eligibility"; body: EligibilityCommand };
export const fetchCoordination = (signal?: AbortSignal) => request<CoordinationView>("/console/coordination", { signal, cache: "no-store" });
export const executeCoordination = (command: CoordinationCommand) => request<CoordinationView>(`/console/coordination/${command.kind}`, { method: command.kind === "transfer" ? "POST" : "PUT", body: command.body });
