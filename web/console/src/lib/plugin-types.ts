export interface PluginSource { kind: "directory" | "git"; location: string; commit?: string; subdir?: string }
export interface SecretRef { name: string; revision: string }
export interface SecretInfo { reference: SecretRef; created_at: string }
export interface PluginSetting { description: string; secret?: boolean; required?: boolean; default?: string }
export interface PluginValue { text?: string; config?: string; secret?: string; prefix?: string }
export interface PluginPreset { harness: string; model?: string; options?: Record<string, string>; system_prompt?: string; skills?: string[]; mcp_servers?: string[] }
export interface PluginManifest {
    schema: number; api: string; id: string; version: string; description: string;
    settings?: Record<string, PluginSetting>; skills?: Record<string, string>; agents?: Record<string, PluginPreset>;
    mcp?: Record<string, { transport: string; url?: PluginValue; headers?: Record<string, PluginValue>; program?: { path: string; runtime?: string; args?: PluginValue[] }; env?: Record<string, PluginValue> }>;
}
export interface PluginPreview { manifest: PluginManifest; digest: string; source: PluginSource }
export interface PluginPackage { project: string; digest: string; manifest: PluginManifest }
export interface PluginConfiguration { values?: Record<string, string>; secrets?: Record<string, SecretRef> }
export interface PluginInstallation { package_id: string; digest: string; enabled: boolean; projects: string[]; targets: Record<string, PluginConfiguration> }
export interface PluginTarget { node: string; state: string; error?: string; receipt?: { hash: string; prepared_at: string } }
export interface PluginInstallationView { id: string; installation: PluginInstallation; targets: PluginTarget[] }
export interface PluginOperation { id: string; kind: string; state: string; error?: string; updated_at: string }
export interface PluginsView { agents?: PluginAgentView[]; revision: string; warning?: string; packages: PluginPackage[]; installations: PluginInstallationView[]; operations: PluginOperation[] }
export interface PluginAdoption { skills?: Record<string, string>; mcp?: Record<string, string> }
export interface PluginPresetRequest { adopt?: PluginAdoption; command_id: string; base_revision: string; agent_id: string; node: string; preset: string; digest: string; model?: string; system_prompt?: string; options?: Record<string, string> }
export interface PluginAgentView { skills?: string[]; mcp_servers?: string[]; origin?: { installation: string; preset: string; adopted?: PluginAdoption }; id: string; node: string; harness: string; model?: string; options?: Record<string, string>; system_prompt?: string }
export interface PluginPresetPreview { revision: string; proposed: PluginAgentView; existing?: PluginAgentView; preserved: string[]; warning?: string }
export interface PluginRuntimeRef { id: string; selection: { project: string; node: string; harness: string; deployments: string[] } }
export interface PluginRuntimeInfo { ref: PluginRuntimeRef; command_id: string; created_at: string; retired: boolean; uses: { id: string; kind: string; stopped: boolean }[]; packages?: { id: string; version: string; digest: string }[] }
export interface PluginUsage { references: { kind: string; owner: string; runtime: PluginRuntimeRef }[]; runtimes: PluginRuntimeInfo[]; errors: Record<string, string> }

export interface PluginResource { installation: string; package_id: string; version: string; name: string; enabled: boolean; projects: string[] }
