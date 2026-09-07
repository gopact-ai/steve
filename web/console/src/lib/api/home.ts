import type { HomeView } from "../types";
import { request } from "../http";
export const fetchHome = (signal?: AbortSignal) => request<HomeView>("/console/home", { signal });
export const saveProjectMemory = (project: string, text: string) => request<{ ok: boolean }>(`/console/memory/${encodeURIComponent(project)}`, { method: "PUT", body: { text } });
export const saveHomeFile = (name: string, text: string) => request<{ ok: boolean }>(`/console/home/${encodeURIComponent(name)}`, { method: "PUT", body: { text } });
