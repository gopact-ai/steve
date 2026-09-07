import type { MCPRegistryEntry, MCPView } from "../types";
import { request } from "../http";
export const fetchMCP = (signal?: AbortSignal) => request<MCPView>("/console/mcp", { signal });
export const probeMCP = (node: string, name: string) => request<{ ok: boolean }>("/console/mcp/probe", { method: "POST", body: { node, name } });
export const adoptMCP = (node: string, source: string, name: string) => request<{ ok: boolean }>("/console/mcp/adopt", { method: "POST", body: { node, source, name } });
export const removeMCP = (node: string, name: string) => request<{ ok: boolean }>(`/console/mcp?node=${encodeURIComponent(node)}&name=${encodeURIComponent(name)}`, { method: "DELETE" });
export const searchMCPRegistry = (query: string, signal?: AbortSignal) => request<{ entries: MCPRegistryEntry[] }>(`/console/mcp/registry?q=${encodeURIComponent(query)}`, { signal });
export const installMCP = (body: { node: string; name: string; entry: string; package?: number; remote?: number; values: Record<string, string> }) => request<{ ok: boolean }>("/console/mcp/install", { method: "POST", body });
