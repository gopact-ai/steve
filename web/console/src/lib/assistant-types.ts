export interface Page<T> { items: T[]; next: string }
export interface CapabilityScope { client: string; capability: string; conversation: string; approval: boolean }
export interface Connection {
    id: string; idempotency_key: string; name: string; kind: "provider" | "client"; principal: string; enabled: boolean;
    event_types?: string[]; work?: Record<string, string>; capabilities?: CapabilityScope[]; created_at: string;
}
export interface Subscription {
    id: string; revision: number; name: string; enabled: boolean; source?: string; event_type: string; include_derived: boolean;
    mode: "inbox" | "work" | "provider"; handler?: string; conversation?: string; project?: string; instruction?: string; replay_safe: boolean;
}
export interface BusinessEvent { id: string; source: string; key: string; type: string; subject?: string; payload: unknown; causation?: string; depth: number; at: string; attention: boolean }
export interface EventInbox { id: string; event_id: string; delivery_id?: string; reason: string; detail?: string; state: string; at: string }
export interface ClientCapability { name: string; description: string; input_schema: unknown }
export interface ClientRegistration { id: string; name: string; generation: number; capabilities: ClientCapability[]; until: string }
export interface ClientInvocation {
    id: string; request: { key: string; client: string; capability: string; conversation: string; arguments: unknown };
    state: string; approved: boolean; receipt?: string; result?: unknown; error?: string; at: string;
}
