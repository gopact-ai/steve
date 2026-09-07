import { request } from "../http";

export type SettingsObject = { [key: string]: string | number | SettingsObject };
export interface SettingsField {
    path: string;
    type: "string" | "integer" | "duration";
    unit?: string;
    minimum?: number;
    maximum?: number;
    enum?: string[];
    default?: string | number;
    apply_mode: string;
}
export interface HubSettings {
    revision: string;
    desired: SettingsObject;
    effective: SettingsObject;
    pending_restart: boolean;
    apply_mode: string;
    fields: SettingsField[];
    warning?: string;
}
export interface VersionNode { name: string; version: string; os: string; arch: string; online: boolean; matches_hub: boolean; protocol: number; features: string[] }
export interface ProjectOwnership { project: string; hub_id: string; epoch: number; state: string; transfer_id?: string; target_hub?: string; evidence?: string; updated_at?: string }
export interface HubPeer { id: string; name?: string; url: string }
export interface ReleaseManifest { version: string; revision: string; component: string; os: string; arch: string; url: string; sha256: string; protocol_min: number; protocol_max: number; published_at: string }
export interface Versions { hub: string; hub_id: string; protocol_min: number; protocol_max: number; nodes: VersionNode[]; discovery_configured: boolean; automatic: boolean; latest?: ReleaseManifest[]; projects?: ProjectOwnership[] | null; peers?: HubPeer[] | null }

export const fetchHubSettings = (signal?: AbortSignal) => request<HubSettings>("/console/settings", { signal });
export const saveHubSettings = (revision: string, settings: SettingsObject) => request<HubSettings>("/console/settings", { method: "PATCH", body: { base_revision: revision, settings } });
export const fetchVersions = (signal?: AbortSignal) => request<Versions>("/console/versions", { signal });

export interface ChannelValues {
    default_channel: "console" | "feishu";
    console: { enabled: true; owner_id: string };
    feishu: { enabled: boolean; app_id: string; app_secret_configured: boolean; domain: "feishu" | "lark"; owner_open_id: string; group_policy: "open" | "allowlist" | "disabled"; allow_unmentioned: boolean; allowed_senders: string[]; blocked_senders: string[] };
}
export interface ChannelSettings { revision: string; desired: ChannelValues; effective: ChannelValues; pending_restart: boolean; apply_mode: "restart"; warning?: string; runtime_error?: string }
export interface ChannelPatch { default_channel?: "console" | "feishu"; feishu?: Partial<Omit<ChannelValues["feishu"], "app_secret_configured">> & { app_secret?: { action: "replace" | "clear"; value?: string } } }
export const fetchChannels = (signal?: AbortSignal) => request<ChannelSettings>("/console/channels", { signal });
export const saveChannels = (revision: string, channels: ChannelPatch) => request<ChannelSettings>("/console/channels", { method: "PUT", body: { base_revision: revision, channels } });

export interface RestartOperation { command_id: string; state: "idle" | "accepted" | "restarted" | "failed"; incarnation: number; previous_incarnation?: number; requested_at?: string; completed_at?: string; error?: string }
export interface ManagedService { name: string; kind: "hub" | "node"; label: string; online: boolean; version: string; supported: boolean; operation?: RestartOperation }
export const fetchServices = (signal?: AbortSignal) => request<{ services: ManagedService[] }>("/console/services", { signal });
export const restartService = (name: string, commandID: string) => request<RestartOperation>(`/console/services/${encodeURIComponent(name)}/restart`, { method: "POST", body: { command_id: commandID } });
export const fetchRestart = (name: string, commandID: string, signal?: AbortSignal) => request<RestartOperation>(`/console/services/${encodeURIComponent(name)}/restart?command_id=${encodeURIComponent(commandID)}`, { signal });
