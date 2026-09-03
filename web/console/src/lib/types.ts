// Mirrors internal/readmodel: the snapshot the hub serves at /state and the
// events it streams at /events. Every list is present, empty or not.
export interface Advert { node?: string; hostname?: string; ips?: string[]; os?: string; arch?: string; harnesses?: Harness[]; capabilities?: string[] }
export interface Hub { node: string; started: string; capabilities?: string[]; level?: string; advert?: Advert }
export interface Harness { id: string; command?: string; version?: string; model?: string; models?: string[]; missing?: string; slots?: number }
export interface Node {
    name: string; role?: string; addr?: string; host?: string; ips?: string[]; up: boolean; since?: string; os?: string; arch?: string;
    capabilities?: string[]; harnesses?: Harness[]; last_error?: string; level?: string; region?: string;
}
export interface Agent {
    id: string; node?: string; harness: string; model?: string; models?: string[]; eligible: boolean; why?: string;
    requires?: string[]; level?: string; slots?: number; region?: string; repair?: string;
}
export interface Task {
    id: string; goal: string; state: string; member?: string; node?: string; channel?: string; parent?: string;
    turns: number; max_turns: number; elapsed?: string; max_elapsed?: string; project_id?: string;
    plan_id?: string; attempts?: number; updated_at?: string; origin?: string;
}
export interface StepContext { goal: string; ancestry?: string[]; refs?: string[]; findings?: string[]; facts?: string[]; bytes?: number }
export interface Step {
    id: string; goal: string; state: string; agent?: string; node?: string; needs?: string[]; merge?: string[];
    requires?: string[]; verify?: string; error?: string; context?: StepContext; artifact?: string; attempts?: number;
}
export interface Plan { id: string; task_id: string; rev: number; goal: string; by: string; because: string; steps: Step[]; created_at?: string; base?: string }
export interface Attempt {
    id: string; kind: string; state: string; task_id?: string; project: string; agent?: string; node?: string;
    scope: string; workspace?: string; leases?: string[]; started_at: string;
}
export interface Landing { id: string; project: string; artifact: string; state: string; paths: number; error?: string; at: string }
export interface Reservation { id: string; endpoint: string; for: string; region?: string; expires_at: string }
export interface Attestation { artifact: string; step?: string; kind: string; verifier: string; verdict: string; detail?: string; attempt: string; at: string }
export interface Replica { artifact: string; node: string; generation: number; state: string; note?: string; at: string }
export interface Disclosure { id: string; project: string; task_id?: string; requester: string; bytes: number; at: string }
export interface Effect { id: string; tool: string; task_id: string; attempt: string; error?: string; at: string }
export interface Grant { project: string; principal: string; role: string; by: string }
export interface Facts {
    reservations: Reservation[]; attestations: Attestation[]; replicas: Replica[];
    disclosures: Disclosure[]; effects: Effect[]; grants: Grant[];
}
export interface Snapshot {
    at: string; hub: Hub; nodes: Node[]; agents: Agent[]; tasks: Task[]; plans: Plan[];
    attempts: Attempt[]; landings: Landing[]; facts: Facts;
}
export interface Event {
    at: string; kind: string; seq?: number; run_id?: string; task_id?: string; plan_id?: string; step_id?: string;
    state?: string; conversation?: string; text?: string; title?: string; detail?: string;
}
export interface Reply { at: string; conversation: string; input?: string; title?: string; text: string; error?: string; kind: string }
