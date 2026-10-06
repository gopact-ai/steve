import { isAgentPermissionFact, type AgentPermissionFact, type AgentPermissionPolicy } from "../agent-permission.ts";
import type { HarnessSetting, NodeSettings } from "./fleet";

export interface FleetAgentBinding { id: string; harness: string; node: string }
export interface FleetAgentBindRequest extends FleetAgentBinding { expected_permission: AgentPermissionPolicy; expected_permission_revision: string }
export interface FleetAgentReceipt extends FleetAgentBinding {
    confirmation?: AgentPermissionFact;
    phase: "save-unknown" | "save-rejected" | "saved" | "bind-unknown" | "bind-rejected" | "bound";
    revision: string;
    launch: string;
}
export interface FleetAgentPorts {
    readSettings(node: string): Promise<{ settings: NodeSettings }>;
    saveSettings(node: string, settings: NodeSettings): Promise<{ settings: NodeSettings }>;
    readAgents(): Promise<{ hub: { node: string }; agents: { id: string; harness: string; node?: string }[] }>;
    readPermission(node: string, harness: string): Promise<AgentPermissionFact>;
    bind(binding: FleetAgentBindRequest): Promise<{ ok: boolean }>;
    rejected(error: unknown, step: "save" | "bind"): boolean;
}
export class FleetAgentCreateError extends Error {
    readonly code: "harnessExists" | "configurationChanged" | "invalidReceipt" | "bindingConflict" | "secureContext" | "permissionRequired" | "invalidPermission";
    constructor(code: FleetAgentCreateError["code"]) { super(code); this.code = code; }
}

// A fingerprint permits exact launch reconciliation without persisting raw env.
// It is configuration equality, not ACP evidence or a trust assessment.
export async function launchFingerprint(harness: HarnessSetting): Promise<string> {
    if (!globalThis.crypto?.subtle) throw new FleetAgentCreateError("secureContext");
    const value = JSON.stringify([harness.adapter || "", harness.command, harness.args || [], harness.env || [], harness.process_dir || ""]);
    const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
    return Array.from(new Uint8Array(bytes), value => value.toString(16).padStart(2, "0")).join("");
}
export function configuredHarnesses(settings: NodeSettings): string[] {
    return Object.keys(settings.harnesses || {}).sort((a, b) => a.localeCompare(b));
}

// The legacy endpoint also maps node transport and committed-but-durability-
// uncertain errors to 400. Never infer rejection from that status alone.
export function fleetWriteRejected(status: number, text: string, step: "save" | "bind"): boolean {
    if ([401, 403, 404, 405, 413, 415, 422].includes(status)) return true;
    if (step === "save" && status === 409) return true; // Settings CAS conflict.
    if (status !== 400 || step !== "save" || text.includes("configuration applied; directory sync failed")) return false;
    return /(?:^|: )(?:duplicate environment key |environment entry \d+ (?:must be KEY=value|contains NUL)|environment key .+ is reserved for process identity|command contains NUL|argument \d+ contains NUL|an adapter takes no args)/.test(text);
}

export async function createFleetAgent(binding: FleetAgentBinding, settings: NodeSettings, custom: HarnessSetting | undefined, ports: FleetAgentPorts, remember: (receipt: FleetAgentReceipt) => void): Promise<FleetAgentReceipt> {
    if (!settings.revision) throw new FleetAgentCreateError("invalidReceipt");
    if (custom && Object.hasOwn(settings.harnesses, binding.harness)) throw new FleetAgentCreateError("harnessExists");
    const launch = custom || (Object.hasOwn(settings.harnesses, binding.harness) ? settings.harnesses[binding.harness] : undefined);
    if (!launch) throw new FleetAgentCreateError("configurationChanged");
    let receipt: FleetAgentReceipt = { ...binding, revision: settings.revision, launch: await launchFingerprint(launch), phase: custom ? "save-unknown" : "saved" };
    remember(receipt); // Must be retained before either non-idempotent write.
    if (custom) {
        try {
            const saved = (await ports.saveSettings(binding.node, { ...settings, harnesses: { ...settings.harnesses, [binding.harness]: custom } })).settings;
            if (!saved?.revision || !saved.harnesses?.[binding.harness] || await launchFingerprint(saved.harnesses[binding.harness]) !== receipt.launch) throw new FleetAgentCreateError("invalidReceipt");
            receipt = { ...receipt, revision: saved.revision, phase: "saved" }; remember(receipt);
        } catch (error) {
            if (ports.rejected(error, "save")) remember({ ...receipt, phase: "save-rejected" });
            throw error;
        }
    }
    return receipt; // Saving a launch never confirms permission or sends POST.
}

// Reads policy only after proving that this is still the saved launch. A fact
// is not consent; the caller must display it and require a separate user action.
export async function readFleetAgentPermission(receipt: FleetAgentReceipt, ports: FleetAgentPorts): Promise<AgentPermissionFact> {
    if (receipt.phase !== "saved" && receipt.phase !== "bind-rejected") throw new FleetAgentCreateError("invalidReceipt");
    const current = (await ports.readSettings(receipt.node)).settings;
    if (!current?.revision) throw new FleetAgentCreateError("invalidReceipt");
    const launch = current.harnesses?.[receipt.harness];
    if (!launch || await launchFingerprint(launch) !== receipt.launch) throw new FleetAgentCreateError("configurationChanged");
    const fact = await ports.readPermission(receipt.node, receipt.harness);
    if (!isAgentPermissionFact(fact) || fact.node !== receipt.node || fact.harness !== receipt.harness) throw new FleetAgentCreateError("invalidPermission");
    return fact;
}

export async function bindFleetAgent(receipt: FleetAgentReceipt, ports: FleetAgentPorts, remember: (receipt: FleetAgentReceipt) => void, confirmation?: AgentPermissionFact): Promise<FleetAgentReceipt> {
    if (receipt.phase !== "saved" && receipt.phase !== "bind-rejected") throw new FleetAgentCreateError("invalidReceipt");
    if (!confirmation) throw new FleetAgentCreateError("permissionRequired");
    if (!isAgentPermissionFact(confirmation) || confirmation.node !== receipt.node || confirmation.harness !== receipt.harness) throw new FleetAgentCreateError("invalidPermission");
    const current = (await ports.readSettings(receipt.node)).settings;
    if (!current?.revision) throw new FleetAgentCreateError("invalidReceipt");
    const launch = current.harnesses?.[receipt.harness];
    if (!launch || await launchFingerprint(launch) !== receipt.launch) throw new FleetAgentCreateError("configurationChanged");
    const pending: FleetAgentReceipt = { ...receipt, revision: current.revision, confirmation, phase: "bind-unknown" }; remember(pending);
    try {
        const result = await ports.bind({ id: receipt.id, harness: receipt.harness, node: receipt.node, expected_permission: confirmation.permission, expected_permission_revision: confirmation.revision });
        if (result?.ok !== true) throw new FleetAgentCreateError("invalidReceipt");
        const bound: FleetAgentReceipt = { ...pending, phase: "bound" }; remember(bound); return bound;
    } catch (error) {
        if (ports.rejected(error, "bind")) remember({ ...pending, confirmation: undefined, phase: "bind-rejected" });
        throw error;
    }
}

// Unknown writes are only read back. Absence never authorizes another POST.
// A rejected bind may be explicitly retried, but only in its original scope.
export async function inspectFleetAgent(receipt: FleetAgentReceipt, ports: FleetAgentPorts): Promise<FleetAgentReceipt | null> {
    const [current, snapshot] = await Promise.all([ports.readSettings(receipt.node), ports.readAgents()]);
    const settings = current.settings;
    if (!settings?.revision) throw new FleetAgentCreateError("invalidReceipt");
    const agent = snapshot.agents.find(agent => agent.id === receipt.id);
    if (agent && (agent.harness !== receipt.harness || (agent.node || snapshot.hub.node) !== receipt.node)) throw new FleetAgentCreateError("bindingConflict");
    const launch = settings.harnesses?.[receipt.harness];
    if (!launch) {
        if (agent) throw new FleetAgentCreateError("configurationChanged");
        if (receipt.phase === "save-rejected") return null;
        return receipt;
    }
    if (await launchFingerprint(launch) !== receipt.launch) throw new FleetAgentCreateError("configurationChanged");
    // A matching name binding does not confirm the original launch save.
    if (agent && (receipt.phase === "bind-unknown" || receipt.phase === "bound")) return { ...receipt, phase: "bound", revision: settings.revision };
    // An existing binding cannot skip this operation's permission confirmation.
    // Only a dispatched binding can be reconciled as complete; a saved launch
    // still needs the owner's separate policy confirmation and asserted POST.
    if (receipt.phase === "save-unknown" || receipt.phase === "save-rejected") return { ...receipt, revision: settings.revision, phase: "saved" };
    return { ...receipt, revision: settings.revision };
}
