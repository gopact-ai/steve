import { request } from "../http";

export type SetupStep = "identity" | "workspace" | "agents" | "machines" | "preferences" | "finished";
export const setupSteps: SetupStep[] = ["identity", "workspace", "agents", "machines", "preferences", "finished"];
export interface DesktopSetup { step: SetupStep; done: boolean }
export interface DesktopStatus { enabled: boolean; node_id?: string; setup_required: boolean; agent_count: number; default_agent?: string; workspace_path?: string; workspace_managed?: boolean; setup?: DesktopSetup }
export interface DesktopAgentCandidate { id: string; name: string; harness: string; executable?: string; installed: boolean; requires?: string[]; registered: boolean }
export const fetchDesktopStatus = (signal?: AbortSignal) => request<DesktopStatus>("/console/desktop", { signal, cache: "no-store" });
export const discoverDesktopAgents = (signal?: AbortSignal) => request<{ agents: DesktopAgentCandidate[] }>("/console/desktop/agents", { signal, cache: "no-store" });
export interface DesktopEnrollAgent { candidate_id: string; agent_id: string; about?: string; default?: boolean }
export const enrollDesktopAgents = (agents: DesktopEnrollAgent[]) => request<DesktopStatus>("/console/desktop/agents", { method: "POST", body: { agents } });
export const saveDesktopSetup = (step: SetupStep, done = false) => request<DesktopStatus>("/console/desktop/setup", { method: "PUT", body: done ? { step, done } : { step } });
export const saveDesktopWorkspace = (path: string) => request<DesktopStatus>("/console/desktop/workspace", { method: "PUT", body: { path } });

// The macOS shell offers a native directory chooser; browsers do not.
declare global { interface Window { steveDesktop?: { pickDirectory?: (directory?: string) => Promise<string | null> } } }
export const canPickDirectory = () => typeof window !== "undefined" && typeof window.steveDesktop?.pickDirectory === "function";
export const pickDirectory = (directory?: string) => window.steveDesktop!.pickDirectory!(directory);
