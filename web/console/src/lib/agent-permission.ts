export type AgentPermissionPolicy = "read" | "write" | "deny" | "auto" | "always_allow";
export type AgentPermissionSource = "shared_remote_permissions" | "hub_harness" | "default_read";
export interface AgentPermissionFact {
    node: string;
    harness: string;
    permission: AgentPermissionPolicy;
    source: AgentPermissionSource;
    revision: string;
}
export function isAgentPermissionFact(value: unknown): value is AgentPermissionFact {
    if (!value || typeof value !== "object") return false;
    const fact = value as AgentPermissionFact;
    return typeof fact.node === "string" && !!fact.node && typeof fact.harness === "string" && !!fact.harness && typeof fact.revision === "string" && !!fact.revision && ["read", "write", "deny", "auto", "always_allow"].includes(fact.permission) && ["shared_remote_permissions", "hub_harness", "default_read"].includes(fact.source);
}
export function permissionConfirmationRejected(status: number, code?: string): boolean {
    return (status === 409 && code === "agent_permission_conflict") || (status === 400 && code === "agent_permission_confirmation_invalid");
}
