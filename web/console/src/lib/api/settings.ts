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
