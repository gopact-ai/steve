import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { cancelRestart, fetchRestart, fetchServices, fetchVersions, restartService, type ManagedService, type RestartMode, type RestartOperation, type Versions } from "@/lib/api/settings";
import { fetchConversations } from "@/lib/api/console";
import { useI18n } from "@/providers/locale-provider";
import { HTTPError } from "@/lib/http";

interface TrackedRestart { id: string; operation?: RestartOperation; error?: string }
const storageKey = "steve.settings.restarts";
const settled = (value?: TrackedRestart) => value?.operation?.state === "restarted" || value?.operation?.state === "failed" || value?.operation?.state === "cancelled";
// waitReasons translate what the service says it is finishing before a
// scheduled restart applies; anything else it reports reads as work in
// progress rather than as a word the reader has to decode.
const waitReasons = {
    preparing: "settingsPage.waitingOn.preparing", requests: "settingsPage.waitingOn.requests", conversations: "settingsPage.waitingOn.conversations",
    channel: "settingsPage.waitingOn.channel", question: "settingsPage.waitingOn.question",
    executions: "settingsPage.waitingOn.executions", copy: "settingsPage.waitingOn.copy",
    attempts: "settingsPage.waitingOn.attempts", agents: "settingsPage.waitingOn.agents", node: "settingsPage.waitingOn.node", offline: "settingsPage.waitingOn.offline",
} as const;
// useConversationNames turns the conversation ids a wait reports into the
// titles the reader gave them. An id is a last resort, not a label.
function useConversationNames(ids: string[]) {
    const [names, setNames] = useState<Record<string, string>>({});
    const wanted = ids.join("\u0000");
    useEffect(() => {
        if (!wanted) return;
        const controller = new AbortController();
        void fetchConversations(controller.signal)
            .then((view) => setNames(Object.fromEntries(view.conversations.map((c) => [c.id, c.title || c.id]))))
            .catch(() => undefined);
        return () => controller.abort();
    }, [wanted]);
    return (id: string) => names[id] || id;
}

function stored(): Record<string, TrackedRestart> {
    try {
        const value = JSON.parse(sessionStorage.getItem(storageKey) || "{}");
        return Object.fromEntries(Object.entries(value).filter(([, operation]) => operation && typeof (operation as TrackedRestart).id === "string")) as Record<string, TrackedRestart>;
    } catch { return {}; }
}
let restartState = stored();
const restartListeners = new Set<() => void>();
function subscribe(listener: () => void) { restartListeners.add(listener); return () => { restartListeners.delete(listener); }; }

export function SettingsServices({ onRestarted }: { onRestarted?: (service: string) => void }) {
    const { t } = useI18n();
    const [services, setServices] = useState<ManagedService[] | null>(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(true);
    const [confirm, setConfirm] = useState<ManagedService | null>(null);
    const tracked = useSyncExternalStore(subscribe, () => restartState);
    const [busy, setBusy] = useState<Record<string, boolean>>({});
    const [storageError, setStorageError] = useState(false);
    const [advanced, setAdvanced] = useState(false);
    const waitingIds = [...new Set((services ?? []).flatMap((service) => (tracked[service.name]?.operation ?? service.operation)?.waiting_conversations ?? []))].sort();
    const nameOf = useConversationNames(waitingIds);
    const active = useRef(new Set<string>()), alive = useRef(true), reads = useRef(0);
    const finished = useRef(onRestarted);
    finished.current = onRestarted;
    const load = async () => {
        const revision = ++reads.current;
        setLoading(true);
        try {
            const next = await fetchServices();
            if (alive.current && revision === reads.current) {
                setServices(next.services || []); setError("");
                for (const service of next.services || []) if (service.operation?.state === "accepted" && !restartState[service.name]) remember(service.name, { id: service.operation.command_id, operation: service.operation });
            }
        }
        catch (failure) { if (alive.current && revision === reads.current) setError(failure instanceof Error ? failure.message : String(failure)); }
        finally { if (alive.current && revision === reads.current) setLoading(false); }
    };
    useEffect(() => { alive.current = true; void load(); return () => { alive.current = false; reads.current++; }; }, []);
    function remember(name: string, value: TrackedRestart, requireStorage = false): boolean {
        const previous = restartState[name];
        if (!requireStorage && previous && (previous.id !== value.id || settled(previous))) return false;
        const next = { ...restartState, [name]: value };
        try { sessionStorage.setItem(storageKey, JSON.stringify(next)); }
        catch { if (requireStorage) { setStorageError(true); return false; } }
        restartState = next;
        for (const listener of restartListeners) listener();
        return true;
    }
    async function run(name: string, id: string, retry = false, mode: RestartMode = "now") {
        if (active.current.has(name)) return;
        active.current.add(name); setBusy((all) => ({ ...all, [name]: true }));
        try {
            const operation = retry ? await restartService(name, id, mode) : await fetchRestart(name, id);
            if (operation.command_id !== id) throw new Error("Restart acknowledgement identity mismatch");
            const previous = restartState[name];
            if (previous?.id !== id || settled(previous)) return;
            remember(name, { id, operation });
            if (alive.current && operation.state === "restarted") finished.current?.(name);
            if (alive.current && (operation.state === "restarted" || operation.state === "failed")) void load();
        } catch (failure) {
            const previous = restartState[name];
            if (previous?.id === id && !settled(previous)) {
                const error = failure instanceof Error ? failure.message : String(failure);
                if (retry && !previous.error && !previous.operation && failure instanceof HTTPError && failure.status === 409) remember(name, { id, operation: { command_id: id, state: "failed", incarnation: 0, error } });
                else remember(name, { ...previous, error });
            }
        } finally { active.current.delete(name); if (alive.current) setBusy((all) => ({ ...all, [name]: false })); }
    }
    useEffect(() => {
        const pending = Object.entries(tracked).filter(([, value]) => !settled(value));
        if (!pending.length) return;
        const timer = window.setInterval(() => { for (const [name, value] of pending) void run(name, value.id); }, 1800);
        return () => window.clearInterval(timer);
    }, [tracked]);
    function start(service: ManagedService, mode: RestartMode) {
        const previous = restartState[service.name];
        const id = previous && !settled(previous) ? previous.id : globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`;
        if (!remember(service.name, { id }, true)) return;
        setStorageError(false); setConfirm(null); void run(service.name, id, true, mode);
    }
    // withdraw returns the service to where it was before the request: the
    // scheduled restart is dropped and nothing is left waiting.
    async function withdraw(name: string, id: string) {
        if (active.current.has(name)) return;
        active.current.add(name); setBusy((all) => ({ ...all, [name]: true }));
        try { remember(name, { id, operation: await cancelRestart(name, id) }); }
        catch (failure) { remember(name, { id, error: failure instanceof Error ? failure.message : String(failure) }); }
        finally { active.current.delete(name); if (alive.current) { setBusy((all) => ({ ...all, [name]: false })); void load(); } }
    }
    const shown = services ?? Object.entries(tracked).filter(([, value]) => !settled(value)).map(([name]): ManagedService => ({ name, kind: name === "hub" ? "hub" : "node", label: name, online: false, version: "", supported: false }));
    return <section>
        <div className="settings-section-heading"><div><h2>{t("settingsPage.servicesSection")}</h2><p>{t("settingsPage.servicesHint")}</p></div><Button size="sm" color="secondary" isLoading={loading} onClick={() => void load()}>{t("settingsPage.reload")}</Button></div>
        {error && <p role="alert" className="settings-alert">{error}</p>}
        {storageError && <p role="alert" className="settings-alert">{t("settingsPage.restartStorageError")}</p>}
        {!shown.length ? <p role="status" className="settings-note">{loading ? t("common.loading") : services ? t("settingsPage.noServices") : t("settingsPage.servicesUnavailable")}</p> : <ul className="settings-service-list">{shown.map((service) => {
            const mine = tracked[service.name], operation = mine?.operation ?? service.operation;
            const unresolved = !!mine && !settled(mine);
            const waiting = operation?.state === "draining";
            const scheduled = waiting || operation?.state === "accepted";
            const reason = waiting ? waitReasons[(operation?.waiting_on ?? "") as keyof typeof waitReasons] ?? "settingsPage.waitingOn.unknown" : null;
            const headline = operation?.state === "restarted" ? "settingsPage.restartConfirmed" : operation?.state === "failed" ? "settingsPage.restartFailed" : operation?.state === "cancelled" ? "settingsPage.restartCancelled" : waiting ? "settingsPage.restartWaiting" : operation?.state === "accepted" ? "settingsPage.restartAccepted" : "settingsPage.restartUnknown";
            return <li key={service.name}>
                <div className="settings-service-heading"><div><h3>{service.label || service.name}</h3><p>{service.kind === "hub" ? "Hub" : t("settingsPage.node")} · {t(service.online ? "settingsPage.online" : "settingsPage.offline")} {service.version ? `· ${service.version}` : ""}</p></div><Button size="sm" color="secondary" isDisabled={!service.supported || !service.online || unresolved || scheduled || !!busy[service.name]} onClick={() => setConfirm(service)}>{t("settingsPage.restartService")}</Button></div>
                {!service.supported && <p className="settings-note">{t("settingsPage.restartUnsupported")}</p>}
                {(operation && operation.state !== "idle" || unresolved) && <div className="settings-operation" role="status"><strong>{t(headline)}</strong>{reason && <p>{t(reason)}</p>}{waiting && !!operation?.waiting_conversations?.length && <p className="settings-note">{operation.waiting_conversations.map(nameOf).join(" · ")}</p>}{(mine?.error || operation?.error) && <p>{mine?.error || operation?.error}</p>}{operation?.state === "restarted" && <p>{t("settingsPage.newIncarnation")}: <code>{operation.incarnation}</code></p>}{unresolved && (waiting
                    ? <div className="settings-actions"><Button size="sm" color="link-gray" isDisabled={!!busy[service.name]} onClick={() => void withdraw(service.name, mine.id)}>{t("settingsPage.cancelRestart")}</Button><Button size="sm" color="link-color" isDisabled={!!busy[service.name]} onClick={() => void run(service.name, mine.id, true)}>{t("settingsPage.restartNowInstead")}</Button></div>
                    : <><p>{t("settingsPage.restartPendingHint")}</p><div className="settings-actions"><Button size="sm" color="link-gray" isDisabled={!!busy[service.name]} onClick={() => void run(service.name, mine.id)}>{t("settingsPage.checkRestart")}</Button><Button size="sm" color="link-color" isDisabled={!!busy[service.name]} onClick={() => void run(service.name, mine.id, true)}>{t("settingsPage.retryRestart")}</Button></div></>)}</div>}
            </li>;
        })}</ul>}
        <details className="settings-advanced" onToggle={(event) => setAdvanced(event.currentTarget.open)}><summary>{t("settingsPage.identityDetails")} · {t("settingsPage.protocol")}</summary>{advanced && <ServiceDetails />}</details>
        {confirm && <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open) setConfirm(null); }}><Modal className="max-w-md"><Dialog aria-label={t("settingsPage.restartTitle", { service: confirm.label || confirm.name })}><div className="settings-dialog"><h2>{t("settingsPage.restartTitle", { service: confirm.label || confirm.name })}</h2><p>{t("settingsPage.restartConfirmHint")}</p><code>{confirm.name}</code><div className="settings-actions"><Button size="sm" color="secondary" onClick={() => setConfirm(null)}>{t("common.cancel")}</Button><Button size="sm" color="tertiary" onClick={() => start(confirm, "now")}>{t("settingsPage.confirmRestart")}</Button><Button size="sm" color="primary" onClick={() => start(confirm, "when-idle")}>{t("settingsPage.restartWhenIdle")}</Button></div></div></Dialog></Modal></ModalOverlay>}
    </section>;
}

function ServiceDetails() {
    const { t } = useI18n();
    const [view, setView] = useState<Versions | null>(null), [error, setError] = useState("");
    useEffect(() => { const controller = new AbortController(); void fetchVersions(controller.signal).then(setView).catch((failure) => { if (!controller.signal.aborted) setError(failure instanceof Error ? failure.message : String(failure)); }); return () => controller.abort(); }, []);
    if (error) return <p role="alert" className="settings-alert">{error}</p>;
    if (!view) return <p role="status" className="settings-note">{t("common.loading")}</p>;
    return <>
        <dl><dt>{t("settingsPage.hubId")}</dt><dd><code>{view.hub_id}</code></dd><dt>{t("settingsPage.protocol")}</dt><dd>{view.protocol_min}–{view.protocol_max}</dd></dl>
        <details className="settings-advanced"><summary>{t("settingsPage.ownership")}</summary><p>{t("settingsPage.ownershipHint")}</p>{view.projects?.length ? <ul className="settings-service-list">{view.projects.map((owner) => <li key={owner.project}><strong>{owner.project}</strong><p><code>{owner.hub_id}</code> · {t("settingsPage.epoch")} {owner.epoch} · {owner.state === "active" || owner.state === "released" || owner.state === "importing" ? t(`settingsPage.owner.${owner.state}`) : owner.state}{owner.target_hub ? ` → ${owner.target_hub}` : ""}</p></li>)}</ul> : <p>{t(view.projects == null ? "settingsPage.ownershipUnavailable" : "settingsPage.noOwnership")}</p>}</details>
        <details className="settings-advanced"><summary>{t("settingsPage.peers")}</summary><p>{t("settingsPage.peersHint")}</p>{view.peers?.length ? <ul className="settings-service-list">{view.peers.map((peer) => <li key={peer.id}><strong>{peer.name || peer.id}</strong><p><code>{peer.id} · {peer.url}</code></p></li>)}</ul> : <p>{t(view.peers == null ? "settingsPage.peersUnavailable" : "settingsPage.noPeers")}</p>}</details>
    </>;
}
