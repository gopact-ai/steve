import { request } from "../http";
export const addProject = (body: { id: string; node?: string; path: string; repo: string; level: string }) => request<{ ok: boolean }>("/console/projects", { method: "POST", body });
export const addWorkspace = (project: string, body: { node?: string; origin: "adopt" | "clone" }) => request<{ ok: boolean }>(`/console/projects/${encodeURIComponent(project)}/workspaces`, { method: "POST", body });
export const removeWorkspace = (project: string, node: string) => request<{ ok: boolean }>(`/console/projects/${encodeURIComponent(project)}/workspaces/${encodeURIComponent(node)}`, { method: "DELETE" });
export const resolveConflicts = (project: string) => request<{ started: number; skipped?: string[] }>(`/console/projects/${encodeURIComponent(project)}/conflicts`, { method: "POST" });
export const removeProject = (id: string) => request<{ ok: boolean }>(`/console/projects/${encodeURIComponent(id)}`, { method: "DELETE" });
