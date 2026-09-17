import { useEffect, useRef, useState, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router";
import { Radio, RadioGroup } from "react-aria-components";
import { CheckCircle, Monitor01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Input } from "@/components/base/input/input";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { SSHConnect } from "@/components/steve/ssh-connect";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import type { Translator } from "@/lib/i18n";
import { message } from "@/lib/http";
import { useTheme } from "@/providers/theme-provider";
import { HTTPError } from "@/lib/http";
import { fetchCoordination } from "@/lib/api/coordination";
import { fetchNodeSettings, removeNode, saveNodeSettings } from "@/lib/api/fleet";
import { renameMachine } from "@/lib/machines";
import { nodeLabel } from "@/lib/node-name";
import { Mono, StateBadge } from "@/components/steve/ui";
import type { Node } from "@/lib/types";
import { fetchHubSettings, saveHubSettings } from "@/lib/api/settings";
import { canPickDirectory, discoverDesktopAgents, enrollDesktopAgents, fetchDesktopStatus, pickDirectory, saveDesktopSetup, saveDesktopWorkspace, setupSteps, type DesktopAgentCandidate, type DesktopStatus, type SetupStep } from "@/lib/api/desktop";


// The first-run guide opens on its own until the owner has been through it
// once; where it stands is kept by the desktop backend, so closing the App
// and coming back resumes at the same page. Resources can still open the
// agents page directly with #/console?setup=agents.
export function DesktopOnboarding() {
    const { t } = useI18n();
    const { refresh, live } = useFleet();
    const location = useLocation();
    const navigate = useNavigate();
    const requested = new URLSearchParams(location.search).get("setup");
    const entry: SetupStep | null = requested === "agents" ? "agents" : null;
    const [status, setStatus] = useState<DesktopStatus | null>(null);
    const [error, setError] = useState("");
    const [dismissed, setDismissed] = useState("");
    const load = useResourceRead("desktop-setup", fetchDesktopStatus, (value) => { setStatus(value); setError(""); }, (error) => setError(message(error)));
    useEffect(() => { void load(); }, [requested, live, load]);
    // "Finish later" keeps the guide away for this window session; the next
    // launch of the App resumes it at the recorded page.
    let deferred = false;
    try { deferred = !!status?.node_id && sessionStorage.getItem(`steve.desktop.setup-deferred:${status.node_id}`) === "1"; } catch { /* Deferring still works for the current visit. */ }
    const open = status?.enabled && (entry || (status.setup_required && !deferred && dismissed !== status.node_id));
    function close() {
        if (status?.node_id) {
            setDismissed(status.node_id);
            try { sessionStorage.setItem(`steve.desktop.setup-deferred:${status.node_id}`, "1"); } catch { /* The current visit remains dismissed. */ }
        }
        if (requested) { const search = new URLSearchParams(location.search); search.delete("setup"); navigate({ pathname: location.pathname, search: search.toString() }, { replace: true }); }
    }
    if (open && status) return <DesktopSetupDialog key={status.node_id} status={status} entry={entry} onClose={close} onStatus={(next) => { setStatus(next); refresh(); }} />;
    if (requested && (error || status?.enabled === false)) return <div role="status" className="fixed right-4 bottom-4 z-50 flex max-w-md flex-wrap items-center gap-3 rounded-lg bg-primary p-4 text-sm text-secondary shadow-lg ring-1 ring-secondary">
        <p>{error ? `${t("desktop.statusError")} ${error}` : t("desktop.unavailable")}</p>
        {error && <Button size="sm" color="secondary" onClick={() => void load()}>{t("desktop.checkAgain")}</Button>}
        <Button size="sm" color="tertiary" onClick={close}>{t("desktop.close")}</Button>
    </div>;
    return null;
}

type Page = Exclude<SetupStep, "finished">;
const pages = setupSteps.filter((step): step is Page => step !== "finished");

function DesktopSetupDialog({ status, entry, onClose, onStatus }: { status: DesktopStatus; entry: SetupStep | null; onClose: () => void; onStatus: (status: DesktopStatus) => void }) {
    const { t } = useI18n();
    // Opened for one page after the guide is done (Resources → register
    // agents), the dialog shows just that page and closes when it is done.
    const single = !!entry && !!status.setup?.done;
    const [step, setStep] = useState<SetupStep>(entry ?? status.setup?.step ?? "identity");
    const [busy, setBusy] = useState(false);
    const [nested, setNested] = useState(false);
    const [finishError, setFinishError] = useState("");
    const heading = useRef<HTMLHeadingElement>(null);
    useEffect(() => { heading.current?.focus({ preventScroll: true }); }, [step]);
    // Progress is recorded as the owner moves, so a closed App reopens here.
    // Writes are serialized so a fast "Finish" cannot be overtaken by the
    // move onto the last page; once the guide is done nothing is written.
    const writes = useRef(Promise.resolve());
    function record(next: SetupStep, done = false) {
        const write = writes.current.then(() => saveDesktopSetup(next, done));
        writes.current = write.then(() => undefined, () => undefined);
        return write;
    }
    function go(next: SetupStep) {
        setStep(next);
        if (!status.setup?.done) record(next).then(onStatus).catch(() => { /* The page still moves; progress is retried on the next move. */ });
    }
    const index = pages.indexOf(step as Page);
    const back = !single && index > 0 ? pages[index - 1] : null;
    const forward = !single && index >= 0 ? setupSteps[setupSteps.indexOf(step) + 1] : null;
    async function finish() {
        setBusy(true); setFinishError("");
        try { onStatus(await record("finished", true)); onClose(); }
        catch (error) { setFinishError(`${t("desktop.finishError")} ${message(error)}`); }
        finally { setBusy(false); }
    }
    const shared = { status, onStatus, busy, setBusy, onNext: () => (forward ? go(forward) : onClose()), onBack: back ? () => go(back) : undefined };
    return <ModalOverlay className="motion-reduce:animate-none motion-reduce:duration-0" isOpen isDismissable={!busy && !nested} isKeyboardDismissDisabled={busy || nested} onOpenChange={(open) => { if (!open && !busy && !nested) onClose(); }}>
        <Modal className="max-w-xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("desktop.guide")} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary">
            <div className="max-h-[min(760px,85dvh)] overflow-y-auto overscroll-contain p-5 sm:p-6">
                <header className="mb-5 space-y-3">
                    <div className="flex items-start justify-between gap-3">
                        <Monitor01 className="size-8 text-tertiary" aria-hidden="true" />
                        {step !== "finished" && <Button size="sm" color="tertiary" isDisabled={busy} onClick={onClose}>{t("desktop.later")}</Button>}
                    </div>
                    <h1 ref={heading} tabIndex={-1} className="text-xl font-semibold text-primary focus-visible:outline-2 focus-visible:outline-focus-ring">{step === "finished" ? t("desktop.finishedTitle") : t(`desktop.step.${step}`)}</h1>
                    {step !== "finished" && !single && <SetupProgress step={step} />}
                    {step === "identity" && <p className="text-sm leading-6 text-secondary">{t("desktop.guideIntro")}</p>}
                </header>
                {step === "identity" && <IdentityStep {...shared} />}
                {step === "workspace" && <WorkspaceStep {...shared} />}
                {step === "agents" && <AgentsStep {...shared} />}
                {step === "machines" && <MachinesStep {...shared} nested={nested} setNested={setNested} />}
                {step === "preferences" && <PreferencesStep {...shared} />}
                {step === "finished" && <FinishedStep status={status} busy={busy} error={finishError} onBack={() => go("preferences")} onFinish={() => void finish()} />}
            </div>
        </Dialog></Modal>
    </ModalOverlay>;
}

function SetupProgress({ step }: { step: Page }) {
    const { t } = useI18n();
    const index = pages.indexOf(step);
    const text = t("desktop.stepOf", { current: index + 1, total: pages.length, step: t(`desktop.step.${step}`) });
    return <div className="space-y-2">
        <p id="desktop-setup-progress" className="text-xs font-medium text-tertiary">{text}</p>
        <div role="progressbar" aria-labelledby="desktop-setup-progress" aria-valuenow={index} aria-valuemin={0} aria-valuemax={pages.length} aria-valuetext={text} className="h-1.5 w-full overflow-hidden rounded-md bg-quaternary">
            <div style={{ transform: `translateX(-${100 - (index * 100) / pages.length}%)` }} className="size-full rounded-md bg-fg-brand-primary transition duration-300 ease-out motion-reduce:transition-none" />
        </div>
        <ol className="flex flex-wrap gap-x-3 gap-y-1 text-xs">{pages.map((page, i) => <li key={page} aria-current={i === index ? "step" : undefined} className={i < index ? "text-secondary" : i === index ? "font-medium text-primary" : "text-quaternary"}>{t(`desktop.step.${page}`)}</li>)}</ol>
    </div>;
}

interface StepProps { status: DesktopStatus; onStatus: (status: DesktopStatus) => void; busy: boolean; setBusy: (busy: boolean) => void; onNext: () => void; onBack?: () => void }

function StepFooter({ busy, onBack, onNext, nextLabel, onSkip, disabled, children }: { busy: boolean; onBack?: () => void; onNext?: () => void; nextLabel?: string; onSkip?: () => void; disabled?: boolean; children?: ReactNode }) {
    const { t } = useI18n();
    return <div className="mt-6 flex flex-wrap items-center gap-2">
        {onNext && <Button size="md" isLoading={busy} isDisabled={disabled} onClick={onNext}>{nextLabel ?? t("desktop.next")}</Button>}
        {onSkip && <Button size="md" color="secondary" isDisabled={busy} onClick={onSkip}>{t("desktop.skip")}</Button>}
        {onBack && <Button size="md" color="tertiary" isDisabled={busy} onClick={onBack}>{t("desktop.back")}</Button>}
        {children}
    </div>;
}

// Identity: the display name lives in coordination (renaming keeps the node
// ID), labels are the worker's capabilities. Both are read fresh here so the
// page shows what is in force, and only what changed is written.
function IdentityStep({ status, busy, setBusy, onNext, onBack }: StepProps) {
    const { t } = useI18n();
    const nodeID = status.node_id || "";
    const [name, setName] = useState("");
    const [labels, setLabels] = useState("");
    const [current, setCurrent] = useState<{ name: string; enabled: boolean; labels: string[] } | null>(null);
    const [settings, setSettings] = useState<Awaited<ReturnType<typeof fetchNodeSettings>>["settings"] | null>(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(true);
    useEffect(() => {
        let alive = true;
        (async () => {
            try {
                const [view, node] = await Promise.all([fetchCoordination(), fetchNodeSettings(nodeID).catch(() => null)]);
                if (!alive) return;
                const local = view.nodes.find((item) => item.local || item.id === nodeID);
                const existing = node?.settings.capabilities || [];
                setCurrent({ name: local?.name || "", enabled: view.enabled && !!local, labels: existing });
                setSettings(node?.settings || null);
                setName((value) => value || local?.name || "");
                setLabels((value) => value || existing.join(", "));
            } catch (error) { if (alive) setError(message(error)); }
            finally { if (alive) setLoading(false); }
        })();
        return () => { alive = false; };
    }, [nodeID]);
    const parsed = labels.split(/[,，]/).map((item) => item.trim()).filter(Boolean).filter((item, i, all) => all.indexOf(item) === i);
    async function save() {
        const trimmed = name.trim();
        if (current?.enabled && !trimmed) { setError(t("desktop.displayNameRequired")); return; }
        setBusy(true); setError("");
        try {
            if (current?.enabled && trimmed !== current.name) {
                await renameMachine(nodeID, trimmed, t);
                setCurrent({ ...current, name: trimmed });
            }
            if (settings && (parsed.length !== settings.capabilities.length || parsed.some((item, i) => item !== settings.capabilities[i]))) {
                const saved = await saveNodeSettings(nodeID, { ...settings, capabilities: parsed });
                setSettings(saved.settings);
            }
            onNext();
        } catch (error) { setError(message(error)); }
        finally { setBusy(false); }
    }
    return <section className="space-y-4">
        <p className="text-sm leading-6 text-secondary">{t("desktop.identityIntro")}</p>
        {loading ? <p role="status" className="text-sm text-tertiary">{t("desktop.loading")}</p> : <>
            <Input size="sm" label={t("desktop.displayName")} name="desktop-name" autoComplete="off" hint={current?.enabled ? t("desktop.displayNameHint") : t("desktop.renameUnavailable")} value={name} onChange={setName} isDisabled={busy || !current?.enabled} maxLength={64} />
            <Input size="sm" label={t("desktop.labels")} name="desktop-labels" autoComplete="off" spellCheck="false" placeholder="gpu, intranet" hint={t("desktop.labelsHint")} value={labels} onChange={setLabels} isDisabled={busy || !settings} />
            {parsed.length > 0 && <ul aria-label={t("desktop.labels")} className="flex flex-wrap gap-1.5">{parsed.map((item) => <li key={item} className="rounded-md bg-secondary px-2 py-0.5 text-xs text-secondary">{item}</li>)}</ul>}
            <p className="flex min-w-0 items-center gap-2 text-xs text-tertiary"><span>{t("desktop.nodeID")}</span><span className="min-w-0 truncate font-mono text-secondary" title={nodeID}>{nodeID}</span></p>
        </>}
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        <StepFooter busy={busy} onBack={onBack} onNext={() => void save()} disabled={loading} />
    </section>;
}

// Workspace: the default project's directory. The macOS shell offers the
// native chooser; elsewhere the path is typed and the backend checks it.
function WorkspaceStep({ status, onStatus, busy, setBusy, onNext, onBack }: StepProps) {
    const { t } = useI18n();
    const managed = !!status.workspace_managed;
    const [path, setPath] = useState(managed || !status.workspace_path ? "~/Steve" : status.workspace_path);
    const [error, setError] = useState("");
    const [picking, setPicking] = useState(false);
    async function choose() {
        if (picking) return;
        setPicking(true); setError("");
        try { const picked = await pickDirectory(path); if (picked) setPath(picked); }
        catch (error) { setError(message(error)); }
        finally { setPicking(false); }
    }
    async function save() {
        setBusy(true); setError("");
        try { onStatus(await saveDesktopWorkspace(path)); onNext(); }
        catch (error) { setError(message(error)); }
        finally { setBusy(false); }
    }
    return <section className="space-y-4">
        <p className="text-sm leading-6 text-secondary">{t("desktop.workspaceIntro")}</p>
        <div className="flex flex-wrap items-end gap-2">
            <Input size="sm" wrapperClassName="min-w-0 flex-1" label={t("desktop.workspacePath")} name="desktop-workspace" autoComplete="off" spellCheck="false" placeholder="~/Steve" hint={t("desktop.workspaceHint")} value={path} onChange={setPath} isDisabled={busy} />
            {canPickDirectory() && <Button size="sm" color="secondary" className="mb-6" isDisabled={busy || picking} onClick={() => void choose()}>{t("desktop.chooseFolder")}</Button>}
        </div>
        {status.workspace_path && !managed && status.workspace_path !== path && <p className="text-xs text-tertiary">{t("desktop.workspaceCurrent", { path: status.workspace_path })}</p>}
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        <StepFooter busy={busy} onBack={onBack} onNext={() => void save()} disabled={!path.trim()} />
    </section>;
}

const agentName = /^[a-z0-9][a-z0-9._-]{0,63}$/;
interface EnrollAgent { candidate_id: string; agent_id: string; about?: string; default?: boolean }
interface EnrollmentDraft { selected: string[]; names?: Record<string, string>; about?: Record<string, string>; primary?: string; pending?: EnrollAgent[] }
function readEnrollment(key: string): EnrollmentDraft {
    try {
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        const strings = (value: unknown): value is string[] => Array.isArray(value) && value.every((item) => typeof item === "string");
        const texts = (value: unknown): value is Record<string, string> => !!value && typeof value === "object" && !Array.isArray(value) && Object.values(value).every((item) => typeof item === "string");
        const chosen = (value: unknown): value is EnrollAgent[] => Array.isArray(value) && value.every((item) => item && typeof item.candidate_id === "string" && typeof item.agent_id === "string");
        return {
            selected: strings(saved?.selected) ? saved.selected : [],
            ...(texts(saved?.names) ? { names: saved.names } : {}),
            ...(texts(saved?.about) ? { about: saved.about } : {}),
            ...(typeof saved?.primary === "string" ? { primary: saved.primary } : {}),
            ...(chosen(saved?.pending) ? { pending: saved.pending } : {}),
        };
    } catch { return { selected: [] }; }
}
function usable(candidate: DesktopAgentCandidate) { return candidate.installed && !candidate.requires?.length && !candidate.registered; }
// localAgents is how many agents run on this computer. A backend that does
// not report it yet only ever registered local agents, so the total stands
// in for it.
function localAgents(status: DesktopStatus) { return status.local_agent_count ?? status.agent_count; }

// Agents: each chosen tool becomes an agent with the name it answers to in
// chat and, optionally, what it is good for. One of them is the default.
// Registration is confirmed by the server's count and the discovery marking
// the chosen tools registered; a selection that was sent but not confirmed
// is retried against that rather than registered twice.
function AgentsStep({ status, onStatus, busy, setBusy, onNext, onBack }: StepProps) {
    const { t } = useI18n();
    const key = `steve.desktop.enrollment:${status.node_id}`;
    const [draft, setDraft] = useState(() => readEnrollment(key));
    const [agents, setAgents] = useState<DesktopAgentCandidate[]>([]);
    const [loading, setLoading] = useState(true);
    const [discoveryError, setDiscoveryError] = useState("");
    const [error, setError] = useState("");
    const [attempted, setAttempted] = useState(false);
    const [mode, setMode] = useState<"local" | "remote">("local");
    const acting = useRef(false);
    const agentList = useRef<HTMLFieldSetElement>(null);
    const form = useRef<HTMLDivElement>(null);
    const confirmed = (current: DesktopStatus, ids: string[], candidates: DesktopAgentCandidate[]) => localAgents(current) > 0 && ids.every((id) => candidates.some((item) => item.id === id && item.registered));
    const load = useResourceRead(`desktop-agents:${status.node_id}`, discoverDesktopAgents, (value) => {
        const candidates = value.agents || [];
        setAgents(candidates); setDiscoveryError(""); setLoading(false);
        if (draft.pending?.length && confirmed(status, draft.pending.map((item) => item.candidate_id), candidates)) finish();
    }, (error) => { setDiscoveryError(message(error)); setLoading(false); });
    useEffect(() => { void load(); }, [load]);
    function save(next: EnrollmentDraft) {
        setDraft(next);
        try { localStorage.setItem(key, JSON.stringify(next)); return true; }
        catch { setError(t("desktop.storageError")); return false; }
    }
    function choose(id: string, selected: boolean) {
        setError(""); setAttempted(false);
        const chosen = selected ? [...draft.selected.filter((item) => item !== id), id] : draft.selected.filter((item) => item !== id);
        save({ ...draft, selected: chosen, names: { ...draft.names, [id]: draft.names?.[id] ?? id }, ...(draft.primary === id && !selected ? { primary: undefined } : {}) });
    }
    function edit(next: Partial<EnrollmentDraft>) { setError(""); setAttempted(false); save({ ...draft, ...next }); }
    function finish() {
        try { localStorage.removeItem(key); } catch { /* The server registration is authoritative. */ }
        setDraft({ selected: [] });
    }
    async function reconcile(ids: string[]) {
        const [current, discovery] = await Promise.all([fetchDesktopStatus(), discoverDesktopAgents()]);
        setAgents(discovery.agents || []);
        if (confirmed(current, ids, discovery.agents || [])) { onStatus(current); finish(); return true; }
        return false;
    }
    const chosen = draft.selected.filter((id) => agents.some((item) => item.id === id && usable(item)));
    const nameOf = (id: string) => draft.names?.[id] ?? id;
    // shown is the name as it will be registered, so the default chooser and
    // the summary line never promise a name the server would not accept.
    const shown = (id: string) => nameOf(id).trim().toLowerCase() || id;
    const primary = chosen.includes(draft.primary || "") ? draft.primary! : chosen[0] || "";
    function focusField(id: string, field: "name" | "about") { form.current?.querySelector<HTMLInputElement>(`input[name="desktop-agent-${field}-${id}"]`)?.focus(); }
    // build turns the selection into what the server registers, refusing a
    // name it could not answer to and a name claimed twice in one go.
    function build(): EnrollAgent[] | null {
        const taken = new Set<string>();
        const out: EnrollAgent[] = [];
        for (const id of chosen) {
            const name = nameOf(id).trim().toLowerCase();
            if (!agentName.test(name)) { setError(t("desktop.agentNameInvalid", { name: nameOf(id).trim() || id })); focusField(id, "name"); return null; }
            if (taken.has(name)) { setError(t("desktop.agentNameDuplicate", { name })); focusField(id, "name"); return null; }
            taken.add(name);
            const about = (draft.about?.[id] || "").trim();
            out.push({ candidate_id: id, agent_id: name, ...(about ? { about } : {}), ...(id === primary && (draft.primary === id || !status.default_agent) ? { default: true } : {}) });
        }
        return out;
    }
    async function register() {
        if (acting.current) return;
        const selected = draft.pending || build();
        if (!selected) return;
        if (!selected.length) {
            if (registeredCount > 0) { onNext(); return; }
            setError(t("desktop.selectRequired")); agentList.current?.querySelector<HTMLInputElement>('input[type="checkbox"]:not(:disabled)')?.focus(); return;
        }
        if (!save({ ...draft, pending: selected })) return;
        const ids = selected.map((item) => item.candidate_id);
        acting.current = true; setBusy(true); setError("");
        try {
            if (draft.pending && await reconcile(ids)) { onNext(); return; }
            setAttempted(true);
            const next = await enrollDesktopAgents(selected);
            if (!(localAgents(next) > 0)) throw new Error(t("desktop.unconfirmed"));
            onStatus(next); finish(); onNext();
        } catch (error) {
            try { if (await reconcile(ids)) { onNext(); return; } } catch { /* Keep the same selection until a response is confirmed. */ }
            if (error instanceof HTTPError && error.status === 400) save({ ...draft, pending: undefined });
            setError(message(error));
        } finally { acting.current = false; setBusy(false); }
    }
    const registeredCount = agents.filter((item) => item.registered).length;
    const unavailable = !loading && !discoveryError && !agents.some((item) => item.installed);
    const locked = busy || !!draft.pending;
    const radio = "group flex min-h-8 cursor-pointer items-center gap-2 rounded-md border border-secondary px-3 py-1.5 text-sm text-primary data-selected:border-brand data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring";
    const choice = "group flex cursor-pointer items-start gap-2 rounded-lg border border-secondary px-3 py-2.5 text-sm text-primary data-selected:border-brand data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring";
    return <section className="space-y-3" ref={form}>
        <p className="text-sm leading-6 text-secondary">{t("desktop.explanation")}</p>
        {registeredCount === 0 && <RadioGroup aria-label={t("desktop.modeLabel")} value={mode} isDisabled={locked} onChange={(value) => { setError(""); setAttempted(false); setMode(value as "local" | "remote"); }} className="flex flex-col gap-2">
            {([["local", "desktop.modeLocal", "desktop.modeLocalHint"], ["remote", "desktop.modeRemote", "desktop.modeRemoteHint"]] as const).map(([value, label, hint]) => <Radio key={value} value={value} className={choice}>
                <span className="flex min-w-0 flex-col gap-0.5"><span className="font-medium">{t(label)}</span><span className="text-xs leading-5 text-tertiary">{t(hint)}</span></span>
            </Radio>)}
        </RadioGroup>}
        {mode === "remote" ? <StepFooter busy={busy} onBack={onBack} onNext={onNext} nextLabel={t("desktop.modeRemoteNext")} /> : <>
        <fieldset ref={agentList} className="min-w-0 space-y-3" disabled={locked}>
            <legend className="mb-2 text-sm font-semibold text-primary">{t("desktop.chooseAgents")}</legend>
            {!loading && agents.some((candidate) => candidate.installed) && <p className="text-xs leading-5 text-tertiary">{t("desktop.detectedHint")}</p>}
            {loading && <p role="status" className="py-3 text-sm text-tertiary">{t("desktop.loading")}</p>}
            {unavailable && <div className="space-y-1 rounded-lg bg-secondary p-3"><p className="text-sm font-medium text-primary">{t("desktop.empty")}</p><p className="text-xs leading-5 text-tertiary">{t("desktop.emptyHint")}</p></div>}
            {agents.map((candidate) => <Checkbox key={candidate.id} aria-label={candidate.name} name="desktop-agent" value={candidate.id} isSelected={candidate.registered || draft.selected.includes(candidate.id)} isDisabled={locked || !usable(candidate)} onChange={(selected) => choose(candidate.id, selected)}
                className="min-h-10 min-w-0 rounded-md border border-secondary px-3 py-2 data-selected:bg-secondary [&>div:last-child]:min-w-0" label={<span className="flex min-w-0 flex-col gap-1"><span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1"><span>{candidate.name}</span><span className="text-xs font-normal text-tertiary">{t(candidate.registered ? "desktop.registered" : candidate.installed ? "desktop.detected" : "desktop.notInstalled")}</span></span>
                    {candidate.requires?.length ? <span className="text-xs font-normal text-error-primary">{t("desktop.requires", { tools: candidate.requires.join(", ") })}</span> : candidate.installed && candidate.executable ? <span className="max-w-full break-all font-mono text-xs font-normal text-tertiary">{candidate.executable}</span> : null}
                </span>} />)}
        </fieldset>
        {chosen.length > 0 && <div className="space-y-3">
            {chosen.map((id) => {
                const candidate = agents.find((item) => item.id === id);
                return <div key={id} className="space-y-3 rounded-lg border border-secondary p-3">
                    <p className="text-sm font-medium text-primary">{candidate?.name || id}</p>
                    <Input size="sm" label={t("desktop.agentName")} name={`desktop-agent-name-${id}`} autoComplete="off" spellCheck="false" hint={t("desktop.agentNameHint")} maxLength={64} value={nameOf(id)} isDisabled={locked} onChange={(value) => edit({ names: { ...draft.names, [id]: value } })} />
                    <Input size="sm" label={t("desktop.agentAbout")} name={`desktop-agent-about-${id}`} autoComplete="off" placeholder={t("desktop.agentAboutPlaceholder")} hint={t("desktop.agentAboutHint")} maxLength={120} value={draft.about?.[id] || ""} isDisabled={locked} onChange={(value) => edit({ about: { ...draft.about, [id]: value } })} />
                </div>;
            })}
            {chosen.length > 1 ? <RadioGroup aria-label={t("desktop.defaultAgentLabel")} value={primary} isDisabled={locked} onChange={(value) => edit({ primary: value })} className="space-y-2">
                <span className="text-sm font-medium text-primary">{t("desktop.defaultAgentLabel")}</span>
                <div className="flex flex-wrap gap-2">{chosen.map((id) => <Radio key={id} value={id} className={radio}>{shown(id)}</Radio>)}</div>
                <span className="block text-xs leading-5 text-tertiary">{t("desktop.defaultAgentHint")}</span>
            </RadioGroup> : !status.default_agent && primary && <p className="text-xs leading-5 text-secondary">{t("desktop.defaultChoice", { agent: shown(primary) })}</p>}
        </div>}
        {discoveryError && <p role="alert" className="break-words text-sm text-error-primary">{discoveryError}</p>}
        {!loading && <Button size="sm" color="tertiary" isDisabled={locked} onClick={() => { setLoading(true); void load(); }}>{t("desktop.checkAgain")}</Button>}
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        {draft.pending && !busy && error !== t("desktop.unconfirmed") && <p role="status" className="text-xs leading-5 text-tertiary">{t("desktop.unconfirmed")}</p>}
        {busy && <p role="status" className="text-sm text-tertiary">{t("desktop.registering")}</p>}
        <StepFooter busy={busy} onBack={onBack} onNext={() => void register()} nextLabel={chosen.length === 0 && registeredCount > 0 ? t("desktop.next") : t(attempted || draft.pending ? "desktop.retryRegister" : "desktop.register")} disabled={loading || !!discoveryError} />
        </>}
    </section>;
}

// Machines: the SSH dialog does the connecting; this page lists what is
// connected so a machine can be renamed or taken out again without leaving
// the guide.
function MachinesStep({ status, busy, onNext, onBack, nested, setNested }: StepProps & { nested: boolean; setNested: (open: boolean) => void }) {
    const { t } = useI18n();
    const { snap, refresh } = useFleet();
    const navigate = useNavigate();
    const others = snap.nodes.filter((node) => node.role !== "hub");
    return <section className="space-y-4">
        <p className="text-sm leading-6 text-secondary">{t("desktop.machinesIntro")}</p>
        {status.agent_count === 0 && <p className="rounded-lg bg-secondary p-3 text-sm leading-6 text-primary">{t("desktop.machinesNeeded")}</p>}
        {others.length === 0 ? <p className="text-sm text-primary">{t("desktop.machinesNone")}</p>
            : <ul className="divide-y divide-secondary rounded-lg border border-secondary">{others.map((node) => <MachineRow key={node.name} node={node} busy={busy} onChanged={refresh} />)}</ul>}
        <Button size="sm" color="secondary" isDisabled={busy} onClick={() => setNested(true)}>{t("desktop.connectMachine")}</Button>
        {nested && <SSHConnect onClose={() => setNested(false)} onChanged={refresh} onViewMachines={() => { setNested(false); navigate("/fleet"); }} onAddExecutor={() => { setNested(false); navigate("/fleet"); }} />}
        <StepFooter busy={busy} onBack={onBack} onNext={others.length > 0 ? onNext : undefined} onSkip={others.length > 0 ? undefined : onNext} />
    </section>;
}

function MachineRow({ node, busy, onChanged }: { node: Node; busy: boolean; onChanged: () => void }) {
    const { t } = useI18n();
    const [renaming, setRenaming] = useState(false);
    const [draft, setDraft] = useState("");
    const [removing, setRemoving] = useState(false);
    const [pending, setPending] = useState(false);
    const [error, setError] = useState("");
    async function rename() {
        setPending(true); setError("");
        try { await renameMachine(node.name, draft, t); setRenaming(false); onChanged(); }
        catch (e) { setError(message(e)); }
        finally { setPending(false); }
    }
    async function remove() {
        setPending(true); setError("");
        try { await removeNode(node.name); onChanged(); }
        catch (e) { setError(message(e)); }
        finally { setPending(false); setRemoving(false); }
    }
    const disabled = busy || pending;
    return <li className="space-y-2 px-3 py-2.5">
        <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <div className="flex min-w-0 flex-1 flex-col">
                <span className="truncate text-sm font-medium text-primary" title={nodeLabel(node)}>{nodeLabel(node)}</span>
                {node.display_name && <Mono className="text-tertiary">{node.name}</Mono>}
            </div>
            <StateBadge state={node.up ? "up" : "down"} />
            {!renaming && !removing && <div className="flex items-center gap-1">
                <Button size="sm" color="link-color" isDisabled={disabled} onClick={() => { setDraft(node.display_name || ""); setError(""); setRenaming(true); }}>{t("fleet.rename")}</Button>
                <Button size="sm" color="link-destructive" isDisabled={disabled} onClick={() => { setError(""); setRemoving(true); }}>{t("common.remove")}</Button>
            </div>}
        </div>
        {!node.up && node.last_error && <p className="break-words text-xs text-error-primary">{node.last_error}</p>}
        {renaming && <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => { e.preventDefault(); void rename(); }}>
            <Input aria-label={t("fleet.displayName")} value={draft} onChange={setDraft} maxLength={64} isDisabled={disabled} autoFocus wrapperClassName="min-w-48 flex-1" />
            <Button size="sm" color="primary" type="submit" isDisabled={disabled} isLoading={pending}>{t("fleet.saveName")}</Button>
            <Button size="sm" color="secondary" isDisabled={disabled} onClick={() => { setRenaming(false); setError(""); }}>{t("common.cancel")}</Button>
        </form>}
        {removing && <div className="flex flex-wrap items-center gap-2">
            <span className="flex-1 text-xs text-tertiary">{t("desktop.machineRemoveHint")}</span>
            <Button size="sm" color="secondary" isDisabled={disabled} onClick={() => setRemoving(false)}>{t("common.cancel")}</Button>
            <Button size="sm" color="primary-destructive" isDisabled={disabled} isLoading={pending} onClick={() => void remove()}>{t("fleet.confirmRemove")}</Button>
        </div>}
        {error && <p role="alert" className="break-words text-xs text-error-primary">{error}</p>}
    </li>;
}

// Preferences: language is applied to this window at once and recorded for
// the backend's messages; appearance is a window preference.
function PreferencesStep({ busy, setBusy, onNext, onBack }: StepProps) {
    const { t, preference, setLocale } = useI18n();
    const { theme, setTheme } = useTheme();
    const [error, setError] = useState("");
    async function save() {
        setBusy(true); setError("");
        try {
            const hub = await fetchHubSettings();
            const desired = (hub.desired.gateway as Record<string, unknown> | undefined)?.locale ?? "";
            const wanted = preference === "system" ? "" : preference;
            if (desired !== wanted) await saveHubSettings(hub.revision, { gateway: { locale: wanted } });
            onNext();
        } catch (error) { setError(message(error)); }
        finally { setBusy(false); }
    }
    const option = "group flex min-h-8 cursor-pointer items-center gap-2 rounded-md border border-secondary px-3 py-1.5 text-sm text-primary data-selected:border-brand data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring";
    return <section className="space-y-5">
        <p className="text-sm leading-6 text-secondary">{t("desktop.preferencesIntro")}</p>
        <RadioGroup aria-label={t("desktop.language")} value={preference} isDisabled={busy} onChange={(value) => setLocale(value as "system" | "zh" | "en")} className="space-y-2">
            <span className="text-sm font-medium text-primary">{t("desktop.language")}</span>
            <div className="flex flex-wrap gap-2">{([["system", "desktop.languageSystem"], ["zh", "desktop.languageZh"], ["en", "desktop.languageEn"]] as const).map(([value, label]) => <Radio key={value} value={value} className={option}>{t(label)}</Radio>)}</div>
        </RadioGroup>
        <RadioGroup aria-label={t("desktop.appearance")} value={theme} isDisabled={busy} onChange={(value) => setTheme(value as "system" | "light" | "dark")} className="space-y-2">
            <span className="text-sm font-medium text-primary">{t("desktop.appearance")}</span>
            <div className="flex flex-wrap gap-2">{([["system", "desktop.appearanceSystem"], ["light", "desktop.appearanceLight"], ["dark", "desktop.appearanceDark"]] as const).map(([value, label]) => <Radio key={value} value={value} className={option}>{t(label)}</Radio>)}</div>
        </RadioGroup>
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        <StepFooter busy={busy} onBack={onBack} onNext={() => void save()} />
    </section>;
}

function agentSummary(status: DesktopStatus, t: Translator) {
    const local = localAgents(status), remote = status.agent_count - local;
    if (status.agent_count === 0) return t("desktop.summaryNoAgents");
    if (remote <= 0) return local === 1 ? t("desktop.summaryAgentsOne") : t("desktop.summaryAgents", { count: local });
    if (local === 0) return remote === 1 ? t("desktop.summaryAgentsRemoteOne") : t("desktop.summaryAgentsRemote", { count: remote });
    return t("desktop.summaryAgentsSplit", { local, remote });
}

function FinishedStep({ status, busy, error, onBack, onFinish }: { status: DesktopStatus; busy: boolean; error: string; onBack: () => void; onFinish: () => void }) {
    const { t } = useI18n();
    const { snap } = useFleet();
    const others = snap.nodes.filter((node) => node.role !== "hub").length;
    return <section className="space-y-4">
        <CheckCircle className="size-6 text-fg-success-primary" aria-hidden="true" />
        <p className="text-sm leading-6 text-secondary">{t("desktop.finishedIntro")}</p>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            {status.workspace_path && <><dt className="text-tertiary">{t("desktop.workspacePath")}</dt><dd className="min-w-0 break-all font-mono text-xs text-secondary">{status.workspace_path}</dd></>}
            <dt className="text-tertiary">{t("desktop.step.agents")}</dt><dd className="text-secondary">{agentSummary(status, t)}{status.default_agent ? ` · ${t("desktop.defaultAgent", { agent: status.default_agent })}` : ""}</dd>
            <dt className="text-tertiary">{t("desktop.step.machines")}</dt><dd className="text-secondary">{others === 1 ? t("desktop.summaryMachinesOne") : t("desktop.summaryMachines", { count: others })}</dd>
        </dl>
        {status.agent_count === 0 && <p className="rounded-lg bg-secondary p-3 text-sm leading-6 text-primary">{t("desktop.summaryNoAgentsHint")}</p>}
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        <StepFooter busy={busy} onBack={onBack} onNext={onFinish} nextLabel={t("desktop.finish")} />
    </section>;
}
