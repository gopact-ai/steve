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
export interface EventJournalEntry extends Omit<BusinessEvent, "payload" | "attention"> { routed: boolean }
export interface EventFlowSnapshot {
    observed_at: string;
    capacity: number;
    events: { id: string; at: string }[];
    claims: { id: string; claim: string; until: string; at: string }[];
    queues: Record<"routing" | "pending" | "claimed" | "accepted" | "unknown", { pending: number; oldest?: string }>;
}
export interface EventActivitySnapshot {
    recent: EventJournalEntry[];
    attention: { id: string; event_id: string; type: string; subject?: string; source: string; rule: string; state: EventDelivery["state"]; mode: Subscription["mode"]; detail?: string; at: string }[];
    more_attention: boolean;
}
export interface EventDelivery {
    id: string; revision: number; event_id: string; rule: Subscription;
    state: "pending" | "claimed" | "accepted" | "done" | "failed" | "unknown" | "cancelled";
    claim_token?: string; claim_until?: string; claimed_at?: string; receipt?: string; detail?: string; resolution?: unknown; updated_at: string;
}
export interface EventProvider {
    id: string; principal: string; name: string; plugin_id?: string; version?: string; generation: number;
    events?: { type: string; version: string; description: string }[]; until?: string; state: "unregistered" | "online" | "offline" | "retired";
}
export interface ServiceBacklog {
    queues: Record<string, { pending: number; oldest?: string }>;
    limits: Record<string, { total: number; per_source: number }>;
    storage: { hot_bytes: number; cold_bytes: number; cold_original_bytes: number; cold_records: number };
    retention_days: number;
}
export interface EventInbox { id: string; event_id: string; delivery_id?: string; reason: string; detail?: string; state: string; at: string }
export interface ClientCapability { name: string; description: string; input_schema: unknown }
export interface ClientRegistration { id: string; name: string; generation: number; capabilities: ClientCapability[]; until: string }
export interface ClientInvocation {
    id: string; request: { key: string; client: string; capability: string; conversation: string; arguments: unknown };
    state: string; approved: boolean; receipt?: string; result?: unknown; error?: string; at: string;
}
