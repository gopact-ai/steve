import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useI18n } from "@/providers/locale-provider";
import { useFleet } from "./fleet";
import { HTTPError } from "./http";
import { executeCoordination, fetchCoordination, type CoordinationCommand, type CoordinationView } from "./api/coordination";

export type CoordinationOperation = CoordinationCommand & { cluster_id: string; state: "pending" | "rejected"; error?: string; at: string };
interface OperationStore { pending?: CoordinationOperation; history: CoordinationOperation[] }
interface CoordinationState {
    view: CoordinationView | null; error: string; busy: boolean; notice: string; pending?: CoordinationOperation; history: CoordinationOperation[];
    refresh: () => Promise<void>; run: (command: CoordinationCommand) => Promise<boolean>; retry: () => Promise<boolean>; acknowledgeRejection: () => void;
}
const CoordinationContext = createContext<CoordinationState | null>(null);
function storageKey() { return `steve.coordination.operations:${new URL(".", window.location.href).href}`; }
function readOperations(): OperationStore {
    try { const value = JSON.parse(localStorage.getItem(storageKey()) || "null"); return { pending: value?.pending, history: Array.isArray(value?.history) ? value.history : [] }; }
    catch { return { history: [] }; }
}
function validView(value: CoordinationView) { return value && typeof value.enabled === "boolean" && Array.isArray(value.nodes) && Array.isArray(value.events) && (!value.enabled || (typeof value.cluster_id === "string" && Number.isFinite(value.epoch) && Number.isFinite(value.revision))); }

export function CoordinationProvider({ children }: { children: ReactNode }) {
    const { t } = useI18n();
    const { live, events } = useFleet();
    const [view, setView] = useState<CoordinationView | null>(null);
    const currentView = useRef<CoordinationView | null>(null);
    const [error, setError] = useState("");
    const [notice, setNotice] = useState("");
    const [busy, setBusy] = useState(false);
    const acting = useRef(false);
    const [operations, setOperations] = useState(readOperations);
    const currentOperations = useRef(operations);
    function persist(value: OperationStore) {
        try { localStorage.setItem(storageKey(), JSON.stringify(value)); }
        catch { setError(t("coord.storageError")); return false; }
        currentOperations.current = value; setOperations(value); return true;
    }
    function accept(next: CoordinationView) {
        if (!validView(next)) throw new Error(t("coord.invalidResponse"));
        const previous = currentView.current;
        if (previous && previous.cluster_id === next.cluster_id && previous.revision > next.revision) next = { ...previous, authoritative: false, reason: t("coord.staleRead") };
        currentView.current = next; setView(next); setError("");
        const pending = currentOperations.current.pending;
        if (pending && pending.cluster_id === next.cluster_id && next.events.some((event) => event.id === pending.body.command_id)) {
            if (persist({ history: currentOperations.current.history })) setNotice(t("coord.confirmed"));
        }
        return next;
    }
    const load = useResourceRead("coordination", fetchCoordination, accept, (error) => setError(error instanceof Error ? error.message : String(error)));
    const changed = events.find((event) => event.kind.startsWith("coordination.") || event.kind === "node.updated")?.at;
    useEffect(() => { void load(); }, [live, changed, load]);
    useEffect(() => { if (!view?.enabled) return; const timer = window.setInterval(() => void load(), 5000); return () => window.clearInterval(timer); }, [view?.enabled, load]);

    async function execute(command?: CoordinationCommand) {
        if (acting.current) return false;
        let current = currentView.current;
        const stored = currentOperations.current.pending;
        if (command && stored) return false;
        if (!command && (!stored || stored.state === "rejected")) return false;
        if (!current?.enabled || !current.cluster_id) return false;
        const operation: CoordinationOperation = command ? { ...command, cluster_id: current.cluster_id, state: "pending", at: new Date().toISOString() } : stored!;
        if (operation.cluster_id !== current.cluster_id) { setError(t("coord.wrongCluster")); return false; }
        if (!current.authoritative && command) { setError(t("coord.noAuthority")); return false; }
        if (!persist({ ...currentOperations.current, pending: operation })) return false;
        acting.current = true; setBusy(true); setError(""); setNotice("");
        try {
            if (!command) {
                current = accept(await fetchCoordination());
                if (!currentOperations.current.pending) return true;
                if (current.cluster_id !== operation.cluster_id) throw new Error(t("coord.wrongCluster"));
                if (!current.authoritative) throw new Error(current.reason || t("coord.noAuthority"));
            }
            const next = accept(await executeCoordination(operation));
            if (!next.events.some((event) => event.id === operation.body.command_id)) throw new Error(t("coord.invalidResponse"));
            return !currentOperations.current.pending;
        } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            if (error instanceof HTTPError && [400, 403, 409].includes(error.status) && currentOperations.current.pending) persist({ ...currentOperations.current, pending: { ...operation, state: "rejected", error: message } });
            setError(message);
            await load();
            return !currentOperations.current.pending;
        } finally { acting.current = false; setBusy(false); }
    }
    function acknowledgeRejection() {
        const current = currentOperations.current;
        if (current.pending?.state !== "rejected" || acting.current) return;
        if (persist({ history: [...current.history, current.pending] })) { setError(""); setNotice(""); }
    }
    return <CoordinationContext.Provider value={{ view, error, busy, notice, pending: operations.pending, history: operations.history, refresh: load, run: execute, retry: () => execute(), acknowledgeRejection }}>{children}</CoordinationContext.Provider>;
}
export function useCoordination() { const value = useContext(CoordinationContext); if (!value) throw new Error("useCoordination outside CoordinationProvider"); return value; }
