import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import { CheckCircle, Monitor01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { HTTPError } from "@/lib/http";
import { discoverDesktopAgents, enrollDesktopAgents, fetchDesktopStatus, type DesktopAgentCandidate, type DesktopStatus } from "@/lib/api/desktop";

// Resources can open enrollment explicitly with #/console?setup=agents.
export function DesktopOnboarding() {
    const { t } = useI18n();
    const { refresh, live } = useFleet();
    const location = useLocation();
    const navigate = useNavigate();
    const requested = new URLSearchParams(location.search).get("setup") === "agents";
    const [status, setStatus] = useState<DesktopStatus | null>(null);
    const [error, setError] = useState("");
    const [dismissed, setDismissed] = useState("");
    const load = useResourceRead("desktop-setup", fetchDesktopStatus, (value) => { setStatus(value); setError(""); }, (error) => setError(String(error).replace(/^Error: /, "")));
    useEffect(() => { void load(); }, [requested, live, load]);
    let deferred = false;
    try { deferred = !!status?.node_id && localStorage.getItem(`steve.desktop.setup-deferred:${status.node_id}`) === "1"; } catch { /* Deferring still works for the current visit. */ }
    const open = status?.enabled && (requested || (status.setup_required && !deferred && dismissed !== status.node_id));
    function close() {
        if (status?.node_id) {
            setDismissed(status.node_id);
            try { localStorage.setItem(`steve.desktop.setup-deferred:${status.node_id}`, "1"); } catch { /* The current visit remains dismissed. */ }
        }
        if (requested) { const search = new URLSearchParams(location.search); search.delete("setup"); navigate({ pathname: location.pathname, search: search.toString() }, { replace: true }); }
    }
    if (open && status) return <DesktopSetupDialog key={status.node_id} status={status} onClose={close} onRegistered={(next) => { setStatus(next); refresh(); }} />;
    if (requested && (error || status?.enabled === false)) return <div role="status" className="fixed right-4 bottom-4 z-50 flex max-w-md flex-wrap items-center gap-3 rounded-lg bg-primary p-4 text-sm text-secondary shadow-lg ring-1 ring-secondary">
        <p>{error ? `${t("desktop.statusError")} ${error}` : t("desktop.unavailable")}</p>
        {error && <Button size="sm" color="secondary" onClick={() => void load()}>{t("desktop.checkAgain")}</Button>}
        <Button size="sm" color="tertiary" onClick={close}>{t("desktop.close")}</Button>
    </div>;
    return null;
}

interface EnrollmentDraft { selected: string[]; pending?: string[] }
function readEnrollment(key: string): EnrollmentDraft {
    try {
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        const strings = (value: unknown): value is string[] => Array.isArray(value) && value.every((item) => typeof item === "string");
        return { selected: strings(saved?.selected) ? saved.selected : [], ...(strings(saved?.pending) ? { pending: saved.pending } : {}) };
    } catch { return { selected: [] }; }
}
function usable(candidate: DesktopAgentCandidate) { return candidate.installed && !candidate.requires?.length && !candidate.registered; }
function enrolled(status: DesktopStatus | undefined) { return !!status?.enabled && status.setup_required === false && status.agent_count > 0; }

function DesktopSetupDialog({ status, onClose, onRegistered }: { status: DesktopStatus; onClose: () => void; onRegistered: (status: DesktopStatus) => void }) {
    const { t } = useI18n();
    const key = `steve.desktop.enrollment:${status.node_id}`;
    const [draft, setDraft] = useState(() => readEnrollment(key));
    const [agents, setAgents] = useState<DesktopAgentCandidate[]>([]);
    const [loading, setLoading] = useState(true);
    const [discoveryError, setDiscoveryError] = useState("");
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState<DesktopStatus | null>(null);
    const acting = useRef(false);
    const agentList = useRef<HTMLFieldSetElement>(null);
    const load = useResourceRead(`desktop-agents:${status.node_id}`, discoverDesktopAgents, (value) => {
        const candidates = value.agents || [];
        setAgents(candidates); setDiscoveryError(""); setLoading(false);
        if (draft.pending?.length && enrolled(status) && draft.pending.every((id) => candidates.some((item) => item.id === id && item.registered))) finish(status);
    }, (error) => { setDiscoveryError(String(error).replace(/^Error: /, "")); setLoading(false); });
    useEffect(() => { void load(); }, [load]);
    function save(next: EnrollmentDraft) {
        setDraft(next);
        try { localStorage.setItem(key, JSON.stringify(next)); return true; }
        catch { setError(t("desktop.storageError")); return false; }
    }
    function choose(id: string, selected: boolean) {
        setError("");
        save({ selected: selected ? [...draft.selected.filter((item) => item !== id), id] : draft.selected.filter((item) => item !== id) });
    }
    function finish(next: DesktopStatus) {
        setResult(next);
        try { localStorage.removeItem(key); } catch { /* The server registration is authoritative. */ }
    }
    async function reconcile(ids: string[]) {
        const [current, discovery] = await Promise.all([fetchDesktopStatus(), discoverDesktopAgents()]);
        setAgents(discovery.agents || []);
        if (enrolled(current) && ids.every((id) => discovery.agents.some((item) => item.id === id && item.registered))) { finish(current); return true; }
        return false;
    }
    async function register() {
        if (acting.current || result) return;
        const selected = draft.pending || draft.selected.filter((id) => agents.some((item) => item.id === id && usable(item)));
        if (!selected.length) { setError(t("desktop.selectRequired")); agentList.current?.querySelector<HTMLInputElement>('input[type="checkbox"]:not(:disabled)')?.focus(); return; }
        if (!save({ selected: draft.selected, pending: selected })) return;
        acting.current = true; setBusy(true); setError("");
        try {
            if (draft.pending && await reconcile(selected)) return;
            const next = await enrollDesktopAgents(selected);
            if (!enrolled(next)) throw new Error(t("desktop.unconfirmed"));
            finish(next);
        } catch (error) {
            try { if (await reconcile(selected)) return; } catch { /* Keep the same selected IDs until a response is confirmed. */ }
            if (error instanceof HTTPError && error.status === 400) save({ selected: draft.selected });
            setError(error instanceof Error ? error.message : String(error));
        } finally { acting.current = false; setBusy(false); }
    }
    function done() { if (result) onRegistered(result); onClose(); }
    const unavailable = !loading && !discoveryError && !agents.some((item) => item.installed);
    const firstSelected = draft.selected.map((id) => agents.find((candidate) => candidate.id === id && usable(candidate))).find(Boolean);
    return <ModalOverlay className="motion-reduce:animate-none motion-reduce:duration-0" isOpen isDismissable={!busy && !draft.pending} isKeyboardDismissDisabled={busy || !!draft.pending} onOpenChange={(open) => { if (!open && !busy) done(); }}>
        <Modal className="max-w-xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("desktop.ready")} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary">
            <div className="max-h-[min(760px,85dvh)] overflow-y-auto overscroll-contain p-5 sm:p-6">
                <header className="mb-6 space-y-3">
                    <Monitor01 className="size-8 text-tertiary" aria-hidden="true" />
                    <h1 className="text-xl font-semibold text-primary">{t("desktop.ready")}</h1>
                    <p className="text-sm leading-6 text-secondary">{t(status.setup_required ? "desktop.explanation" : "desktop.registerExplanation")}</p>
                    <p className="flex min-w-0 items-center gap-2 text-xs text-tertiary"><span>{t("desktop.node")}</span><span className="min-w-0 truncate font-medium text-primary" title={status.node_id}>{status.node_id}</span></p>
                </header>
                {result ? <section className="space-y-3" aria-live="polite">
                    <CheckCircle className="size-6 text-fg-success-primary" aria-hidden="true" />
                    <h2 className="text-lg font-semibold text-primary">{t("desktop.success")}</h2>
                    <p className="text-sm leading-6 text-secondary">{t("desktop.successHint")}</p>
                    {result.default_agent && <p className="text-sm text-tertiary">{t("desktop.defaultAgent", { agent: result.default_agent })}</p>}
                    <Button size="md" onClick={done}>{t("desktop.openWorkbench")}</Button>
                </section> : <>
                    <fieldset ref={agentList} className="min-w-0 space-y-3" disabled={busy || !!draft.pending}>
                        <legend className="mb-2 text-sm font-semibold text-primary">{t("desktop.chooseAgents")}</legend>
                        {!loading && agents.some((candidate) => candidate.installed) && <p className="text-xs leading-5 text-tertiary">{t("desktop.detectedHint")}</p>}
                        {loading && <p role="status" className="py-3 text-sm text-tertiary">{t("desktop.loading")}</p>}
                        {unavailable && <div className="space-y-1 rounded-lg bg-secondary p-3"><p className="text-sm font-medium text-primary">{t("desktop.empty")}</p><p className="text-xs leading-5 text-tertiary">{t("desktop.emptyHint")}</p></div>}
                        {agents.map((candidate) => <Checkbox key={candidate.id} aria-label={candidate.name} name="desktop-agent" value={candidate.id} isSelected={candidate.registered || draft.selected.includes(candidate.id)} isDisabled={busy || !!draft.pending || !usable(candidate)} onChange={(selected) => choose(candidate.id, selected)}
                            className="min-h-12 min-w-0 rounded-lg border border-secondary p-3 data-selected:bg-secondary [&>div:last-child]:min-w-0" label={<span className="flex min-w-0 flex-col gap-1"><span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1"><span>{candidate.name}</span><span className="text-xs font-normal text-tertiary">{t(candidate.registered ? "desktop.registered" : candidate.installed ? "desktop.detected" : "desktop.notInstalled")}</span></span>
                                {candidate.requires?.length ? <span className="text-xs font-normal text-error-primary">{t("desktop.requires", { tools: candidate.requires.join(", ") })}</span> : candidate.installed && candidate.executable ? <span className="max-w-full break-all font-mono text-xs font-normal text-tertiary">{candidate.executable}</span> : null}
                            </span>} />)}
                    </fieldset>
                    {discoveryError && <p role="alert" className="mt-3 break-words text-sm text-error-primary">{discoveryError}</p>}
                    {!loading && <Button size="sm" color="tertiary" className="mt-3" isDisabled={busy || !!draft.pending} onClick={() => { setLoading(true); void load(); }}>{t("desktop.checkAgain")}</Button>}
                    {status.agent_count === 0 && firstSelected && <p className="mt-3 text-xs leading-5 text-secondary">{t("desktop.defaultChoice", { agent: firstSelected.name })}</p>}
                    {error && <p role="alert" className="mt-3 break-words text-sm text-error-primary">{error}</p>}
                    {draft.pending && !busy && <p role="status" className="mt-3 text-xs leading-5 text-tertiary">{t("desktop.unconfirmed")}</p>}
                    {busy && <p role="status" className="mt-3 text-sm text-tertiary">{t("desktop.registering")}</p>}
                    <div className="mt-5 flex flex-wrap gap-2"><Button size="md" isLoading={busy} isDisabled={loading || !!discoveryError} onClick={() => void register()}>{t(error || draft.pending ? "desktop.retryRegister" : "desktop.register")}</Button><Button size="md" color="secondary" isDisabled={busy} onClick={done}>{t("desktop.registerLater")}</Button></div>
                    <p className="mt-3 text-xs leading-5 text-quaternary">{t("desktop.selectionSaved")}</p>
                </>}
            </div>
        </Dialog></Modal>
    </ModalOverlay>;
}
