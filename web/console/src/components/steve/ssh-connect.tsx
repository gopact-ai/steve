import { DialogSurface, DialogBody } from "@/components/steve/dialog-surface";
import { NodeAgentEnrollment } from "@/components/steve/node-agent-enrollment";
import { RemoteDirectoryPicker } from "@/components/steve/remote-directory-picker";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { CheckCircle, FolderSearch, XCircle, X } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { IconButton } from "@/components/steve/icon-button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useI18n } from "@/providers/locale-provider";
import { dateTime } from "@/lib/format";
import { levelName } from "@/lib/workspaces";
import { abandonSSH, checkSSH, discoverSSH, installSSH, planSSH, statusSSH, type SSHCheck, type SSHDiscovery, type SSHInstallRequest, type SSHInstallResult, type SSHPlan, type SSHStep } from "@/lib/api/ssh";

interface SSHRecord { plan: SSHPlan; result: SSHInstallResult }
interface SSHDraft { request: SSHInstallRequest; check?: SSHCheck; plan?: SSHPlan; attempted?: boolean; result?: SSHInstallResult; history?: SSHRecord[] }
const initialRequest: SSHInstallRequest = { alias: "", name: "", addr: "", level: "restricted" };
function draftKey() { return `steve.ssh.connect:${new URL(".", window.location.href).href}`; }
function usableCheck(check?: SSHCheck) { return check && (check.existing_installation || ["peer", "executor"].includes(check.installation_mode || "")) ? check : undefined; }
function readDraft(): SSHDraft {
    try {
        const saved = JSON.parse(localStorage.getItem(draftKey()) || "null");
        if (!saved || !saved.request || typeof saved.request.alias !== "string") return { request: initialRequest };
        if (saved.plan && (!saved.plan.id || !Array.isArray(saved.plan.steps) || !Array.isArray(saved.plan.effects))) return { request: initialRequest };
        if (!saved.plan) return { ...saved, check: usableCheck(saved.check) };
        return saved;
    } catch { return { request: initialRequest }; }
}
function addressFor(host?: string) { return host ? `${host.includes(":") ? `[${host}]` : host}:7701` : ""; }
// A machine's name is what people see for it; the SSH alias is the natural
// first guess. An execution-only node's name is a configuration key.
const defaultWorkspace = "~/steve-workspace";
const executorNameShape = /^[a-z0-9][a-z0-9._-]{0,63}$/;
function suggestedName(alias: string) { return alias.trim().slice(0, 64); }
function executorName(name: string) { return name.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "").slice(0, 64); }
function validWorkspace(dir: string) { return dir.length <= 512 && !/[\r\n\t\0]/.test(dir) && (dir.startsWith("/") || dir === "~" || dir.startsWith("~/")) && !dir.split("/").includes(".."); }
function sameRequest(a: SSHInstallRequest, b: SSHInstallRequest) { return a.alias === b.alias && a.name === b.name && a.addr === b.addr && a.level === b.level && (a.raft_addr || "") === (b.raft_addr || "") && (a.workspace_dir || "") === (b.workspace_dir || ""); }

export function SSHConnect({ onClose, onChanged, onViewMachines, onAddExecutor }: { onClose: () => void; onChanged: () => void; onViewMachines: () => void; onAddExecutor: (request: SSHInstallRequest) => void }) {
    const { t, locale } = useI18n();
    const [draft, setDraft] = useState(readDraft);
    const [enrolling, setEnrolling] = useState(false);
    const [discovery, setDiscovery] = useState<SSHDiscovery>({ candidates: [], warnings: [], revision: "" });
    const [loading, setLoading] = useState(true);
    const [readError, setReadError] = useState("");
    const [error, setError] = useState("");
    const [busy, setBusy] = useState<"check" | "plan" | "install" | "abandon" | null>(null);
    const [browsing, setBrowsing] = useState(false);
    const acting = useRef(false);
    const scrollArea = useRef<HTMLDivElement>(null);
    const stageHeading = useRef<HTMLHeadingElement>(null);
    const fields = useRef<HTMLDivElement>(null);
    const candidates = useRef<HTMLDivElement>(null);
    const [now, setNow] = useState(Date.now);
    const { request, check, plan, attempted, result } = draft;
    const peerInstallation = check?.installation_mode === "peer";
    const needsPeerConsent = peerInstallation && !["restricted", "sealed"].includes(request.level);
    const existingInstallation = check?.existing_installation === true;
    const relatedHistory = (draft.history || []).filter((record) => record.plan.request.alias === request.alias);
    const stage = result?.status || (attempted ? "installing" : plan ? "plan" : check ? existingInstallation ? "existing" : "form" : "discovery");
    useLayoutEffect(() => {
        stageHeading.current?.focus({ preventScroll: true });
        if (scrollArea.current) scrollArea.current.scrollTop = 0;
    }, [stage]);
    const load = useResourceRead("ssh-candidates", discoverSSH, (value) => {
        const available = value.candidates || [];
        setDiscovery({ ...value, candidates: available, warnings: value.warnings || [] });
        setReadError(""); setLoading(false);
        if (!check && !plan && !acting.current) {
            setError("");
            if (request.alias && !available.some((candidate) => candidate.alias === request.alias)) save({ request: initialRequest });
        }
    }, (error) => { setReadError(String(error).replace(/^Error: /, "")); setLoading(false); });
    useEffect(() => { if (!plan) void load(); }, [load, plan]);
    useEffect(() => { if (!plan) return; const ms = Date.parse(plan.expires_at) - Date.now(); if (ms <= 0) return; const timer = window.setTimeout(() => setNow(Date.now()), Math.min(ms + 1, 2_147_483_647)); return () => window.clearTimeout(timer); }, [plan]);
    const expired = !!plan && Date.parse(plan.expires_at) <= now;
    const connected = result?.registered === true && result.connected === true && result.status === "connected";
    // Connecting a machine is only half of it: until an agent is registered
    // there, nothing can be assigned to it. The registration opens by itself
    // the first time a machine reports connected, and stays closed once it
    // has been dealt with.
    const offered = useRef("");
    useEffect(() => {
        if (!connected || !plan || offered.current === plan.id) return;
        offered.current = plan.id;
        setEnrolling(true);
    }, [connected, plan]);
    const canResumeRegistration = plan?.check.installation_mode === "peer" && attempted && result?.registered === true && !result.connected && result.status === "needs_attention";
    const terminal = connected || result?.status === "needs_attention";
    // Only a cluster enrollment can be withdrawn from here; an execution-only
    // node's registration is removed on the resources page.
    const canAbandon = plan?.check.installation_mode === "peer" && result?.status === "needs_attention";
    function save(next: SSHDraft) {
        next = { ...next, history: next.history || draft.history || [] };
        setDraft(next);
        try { localStorage.setItem(draftKey(), JSON.stringify(next)); return true; }
        catch { setError(t("ssh.storageError")); return false; }
    }
    function edit(patch: Partial<SSHInstallRequest>) { if (attempted || acting.current) return; setError(""); save({ ...draft, request: { ...request, ...patch }, plan: undefined }); }
    async function testConnection() {
        if (acting.current || attempted || (!check && (loading || readError))) return;
        if (!check && !discovery.candidates.some((candidate) => candidate.alias === request.alias)) { setError(t("ssh.selectRequired")); candidates.current?.querySelector<HTMLInputElement>('input[type="radio"]')?.focus(); return; }
        acting.current = true; setBusy("check"); setError("");
        try {
            const checked = await checkSSH(request.alias);
            if (!checked?.candidate || checked.candidate.alias !== request.alias || !Array.isArray(checked.steps) || (checked.reachable && !usableCheck(checked))) throw new Error(t("ssh.responseInvalid"));
            const peer = checked.installation_mode === "peer";
            const name = request.name || suggestedName(request.alias);
            save({ request: { ...request, name: peer ? name : executorName(name), addr: request.addr || addressFor(checked.address), workspace_dir: peer ? request.workspace_dir || defaultWorkspace : undefined }, check: checked });
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    async function preparePlan(allowPeerData = false) {
        if (acting.current || attempted || !check?.reachable || existingInstallation || (needsPeerConsent && !allowPeerData)) return;
        const body = { ...request, ...(needsPeerConsent && allowPeerData ? { level: "restricted" } : {}), name: request.name.trim(), addr: request.addr.trim(), raft_addr: request.raft_addr?.trim() || undefined, workspace_dir: peerInstallation ? request.workspace_dir?.trim() || defaultWorkspace : undefined };
        if (peerInstallation ? body.name.length === 0 || [...body.name].length > 64 : !executorNameShape.test(body.name)) { setError(t(peerInstallation ? "ssh.machineNameInvalid" : "ssh.nameInvalid")); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-node-name"]')?.focus(); return; }
        if (!body.addr) { setError(t("ssh.addressRequired")); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-node-address"]')?.focus(); return; }
        if (body.workspace_dir && !validWorkspace(body.workspace_dir)) { setError(t("ssh.workspaceInvalid")); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-workspace"]')?.focus(); return; }
        acting.current = true; setBusy("plan"); setError("");
        try {
            const next = await planSSH(body);
            if (!next?.id || !next.request || !sameRequest(next.request, body) || !Array.isArray(next.steps) || !Array.isArray(next.effects) || !Number.isFinite(Date.parse(next.expires_at))) throw new Error(t("ssh.responseInvalid"));
            setNow(Date.now()); save({ request: body, check: next.check, plan: next });
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    async function install() {
        if (acting.current || !plan || (terminal && !canResumeRegistration)) return;
        if (!attempted && (!plan.ready || Date.parse(plan.expires_at) <= Date.now())) { setNow(Date.now()); setError(t(plan.ready ? "ssh.expired" : "ssh.blocked")); return; }
        if (!save({ ...draft, attempted: true })) return;
        acting.current = true; setBusy("install"); setError("");
        // While the installer runs, follow it: the phase it is in and what
        // the machine has said. Only a running installation is shown this
        // way; the outcome comes from the install call itself. Resuming a
        // registration runs the same way over a needs-attention result.
        const following = new AbortController();
        const follow = () => statusSSH(plan.id, following.signal).then((live) => {
            if (following.signal.aborted || live?.plan_id !== plan.id || live.status !== "installing") return;
            setDraft((current) => current.plan?.id === plan.id && current.attempted && current.result?.status !== "connected" ? { ...current, result: live } : current);
        }).catch(() => undefined);
        const followTimer = window.setInterval(() => void follow(), 1000);
        void follow();
        try {
            const next = await installSSH(plan.id);
            if (next?.plan_id !== plan.id) throw new Error(t("ssh.otherInstallation"));
            if (typeof next.registered !== "boolean" || typeof next.connected !== "boolean" || !["connected", "needs_attention", "installing"].includes(next.status) || !Array.isArray(next.steps) || (next.status === "connected" && (!next.registered || !next.connected))) throw new Error(t("ssh.responseInvalid"));
            // History keeps outcomes, not transcripts: the log stays with the
            // current result and would otherwise outgrow local storage.
            const history = next.status === "connected" || next.status === "needs_attention" ? [...(draft.history || []).filter((record) => record.plan.id !== plan.id), { plan, result: { ...next, log: undefined } }] : draft.history;
            save({ ...draft, attempted: true, result: next, history });
            if (next.registered) onChanged();
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { window.clearInterval(followTimer); following.abort(); acting.current = false; setBusy(null); }
    }
    // abandon gives up on an installation that will not finish: the plan
    // or operation is withdrawn on this side and the form comes back with
    // the same answers, so the machine can be enrolled again. Nothing on
    // the remote machine is changed.
    async function abandon() {
        if (acting.current || !plan) return;
        acting.current = true; setBusy("abandon"); setError("");
        try {
            await abandonSSH(plan.id);
            save({ request: plan.request, check: usableCheck(plan.check), history: (draft.history || []).filter((record) => record.plan.id !== plan.id) });
            onChanged();
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    function close() {
        if (acting.current) return;
        if (connected) save({ request: initialRequest });
        onClose();
    }
    return <ModalOverlay isOpen isDismissable={!busy} isKeyboardDismissDisabled={!!busy} onOpenChange={(open) => { if (!open) close(); }} className="motion-reduce:animate-none motion-reduce:duration-0">
        <Modal className="max-w-2xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("ssh.title")} className="block"><DialogSurface className="overflow-hidden">
            <DialogBody ref={scrollArea} className="max-h-[min(840px,85dvh)] overflow-y-auto overscroll-contain block">
                <header className="mb-5 flex items-start gap-3"><div className="min-w-0 flex-1"><h1 ref={!check && !plan ? stageHeading : undefined} tabIndex={-1} className="text-md font-semibold text-primary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("ssh.title")}</h1><p className="mt-2 text-sm leading-6 text-tertiary">{t("ssh.description")}</p></div><IconButton size="sm" color="tertiary" icon={X} label={t("ssh.close")} isDisabled={!!busy} onClick={close} /></header>
                {!check && !plan && <section className="space-y-4">
                    {loading && <p role="status" className="text-sm text-tertiary">{t("ssh.loading")}</p>}
                    {!loading && !readError && discovery.candidates.length === 0 && <div className="space-y-2 rounded-lg bg-secondary p-4"><h2 className="text-sm font-semibold text-primary">{t("ssh.empty")}</h2><p className="text-sm leading-6 text-tertiary">{t("ssh.emptyHint")}</p></div>}
                    <RadioGroup ref={candidates} aria-label={t("ssh.candidates")} value={request.alias} isDisabled={!!busy} onChange={(alias) => { setError(""); save({ request: { ...initialRequest, alias, name: suggestedName(alias) } }); }} className="space-y-1.5">
                        {discovery.candidates.map((candidate) => <Radio key={candidate.alias} value={candidate.alias} className="group flex min-h-10 cursor-pointer gap-2.5 rounded-md border border-secondary px-3 py-2 data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring">
                            <span className="mt-0.75 size-3.5 shrink-0 rounded-full border border-primary group-data-selected:border-4 group-data-selected:border-brand" aria-hidden="true" />
                            <span className="flex min-w-0 flex-col gap-0.5"><span className="text-sm font-medium text-primary">{candidate.alias}</span><span className="break-all font-mono text-xs text-secondary">{candidate.user ? candidate.user + "@" : ""}{candidate.host_name}:{candidate.port}</span>{candidate.proxy_jump && <span className="break-words text-xs text-tertiary">{t("ssh.via", { proxy: candidate.proxy_jump })}</span>}{candidate.has_proxy_command && <span className="text-xs text-tertiary">{t("ssh.proxyCommand")}</span>}{candidate.conditional && <span className="text-xs text-tertiary">{t("ssh.conditional")}</span>}</span>
                        </Radio>)}
                    </RadioGroup>
                    {discovery.warnings.length > 0 && <details className="rounded-lg bg-secondary p-3" open><summary className="text-xs font-medium text-secondary">{t("ssh.warnings")}</summary><ul className="mt-2 space-y-2 text-xs leading-5 text-tertiary">{discovery.warnings.map((warning, i) => <li key={i}><p>{warning.message}</p><span className="break-all font-mono">{warning.source}{warning.line ? `:${warning.line}` : ""}</span></li>)}</ul></details>}
                    {!!draft.history?.length && <details className="rounded-lg border border-secondary p-3"><summary className="text-xs font-medium text-tertiary">{t("ssh.history", { count: draft.history.length })}</summary><ul className="mt-3 space-y-3">{draft.history.toReversed().map((record) => <li key={record.plan.id} className="flex min-w-0 flex-wrap items-center justify-between gap-2"><div className="min-w-0"><p className="break-all text-sm font-medium text-primary">{record.plan.request.name}</p><p className="text-xs text-tertiary">{t(record.result.connected ? "ssh.historyConnected" : "ssh.attention")}</p></div><Button size="sm" color="secondary" onClick={() => { setError(""); save({ request: record.plan.request, check: record.plan.check, plan: record.plan, attempted: true, result: record.result }); }}>{t("ssh.viewRecord")}</Button></li>)}</ul></details>}
                    {readError && <p role="alert" className="break-words text-sm text-error-primary">{readError}</p>}
                    <div className="flex flex-wrap gap-2"><Button size="md" isLoading={busy === "check"} isDisabled={loading || !!readError || discovery.candidates.length === 0} onClick={() => void testConnection()}>{t("ssh.check")}</Button><Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setLoading(true); void load(); }}>{t("ssh.refresh")}</Button></div>
                </section>}
                {check && !plan && <section className="space-y-4" ref={fields}>
                    <h2 ref={stageHeading} tabIndex={-1} className="text-base font-semibold text-primary focus-visible:outline-2 focus-visible:outline-focus-ring">{t(existingInstallation ? "ssh.existing" : check.reachable ? "ssh.reachable" : "ssh.notReachable")}</h2>
                    {existingInstallation ? <>
                        <dl className="grid min-w-0 grid-cols-[minmax(0,auto)_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm"><dt className="text-tertiary">{t("ssh.targetAlias")}</dt><dd className="break-all font-medium text-primary">{check.candidate.alias}</dd>{check.address && <><dt className="text-tertiary">{t("ssh.checkedAddress")}</dt><dd className="break-all font-mono text-secondary">{check.address}</dd></>}</dl>
                        <p className="text-sm leading-6 text-secondary">{t("ssh.existingHint")}</p>
                        <div className="space-y-2 rounded-lg bg-secondary p-3"><h3 className="text-xs font-medium text-tertiary">{t("ssh.existingPaths")}</h3>{check.existing_paths?.length ? <ul className="space-y-1">{check.existing_paths.map((path) => <li key={path} className="break-all font-mono text-sm text-secondary">{path}</li>)}</ul> : <p className="text-sm text-secondary">{t("ssh.existingUnknownPaths")}</p>}</div>
                        {(check.existing_node?.name || check.existing_node?.owner) && <dl className="grid min-w-0 grid-cols-[minmax(0,auto)_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">{check.existing_node.name && <><dt className="text-tertiary">{t("ssh.recordedNodeName")}</dt><dd className="break-all text-secondary">{check.existing_node.name}</dd></>}{check.existing_node.owner && <><dt className="text-tertiary">{t("ssh.recordedOwner")}</dt><dd className="break-all text-secondary">{check.existing_node.owner}</dd></>}</dl>}
                        <p className="text-sm leading-6 text-secondary">{t("ssh.existingNext")}</p>
                        {relatedHistory.length > 0 && <div className="space-y-3 rounded-lg border border-secondary p-3"><p className="text-xs leading-5 text-tertiary">{t("ssh.relatedHistory")}</p>{relatedHistory.toReversed().map((record) => <div key={record.plan.id} className="flex min-w-0 flex-wrap items-center justify-between gap-2"><span className="break-all text-sm text-secondary">{record.plan.request.name}</span><Button size="sm" color="secondary" isDisabled={!!busy} onClick={() => { setError(""); save({ request: record.plan.request, check: record.plan.check, plan: record.plan, attempted: true, result: record.result }); }}>{t("ssh.viewRecord")}</Button></div>)}</div>}
                        <div className="flex flex-wrap gap-2"><Button size="md" isLoading={busy === "check"} onClick={() => void testConnection()}>{t("ssh.recheck")}</Button><Button size="md" color="secondary" isDisabled={!!busy} onClick={onViewMachines}>{t("ssh.viewMachines")}</Button></div>
                    </> : <SSHSteps steps={check.steps} />}
                    {check.reachable && !existingInstallation && <><Input size="sm" label={t(peerInstallation ? "ssh.machineName" : "ssh.name")} name="ssh-node-name" autoComplete="off" spellCheck="false" placeholder={peerInstallation ? t("ssh.machineNamePlaceholder") : "worker-west…"} hint={t(peerInstallation ? "ssh.machineNameHint" : "ssh.nameHint")} value={request.name} onChange={(name) => edit({ name })} isDisabled={!!busy} />
                        <Input size="sm" label={t("ssh.address")} name="ssh-node-address" autoComplete="off" spellCheck="false" placeholder="192.0.2.7:7701…" hint={t("ssh.addressHint")} value={request.addr} onChange={(addr) => edit({ addr })} isDisabled={!!busy} />
                        {peerInstallation && <div className="space-y-3">
                            <div className="space-y-1.5"><div className="flex items-end gap-2"><div className="min-w-0 flex-1"><Input size="sm" label={t("ssh.workspace")} name="ssh-workspace" aria-describedby="ssh-workspace-hint" autoComplete="off" spellCheck="false" placeholder={defaultWorkspace} value={request.workspace_dir ?? defaultWorkspace} onChange={(workspace_dir) => edit({ workspace_dir })} isDisabled={!!busy} /></div><Button size="sm" color="secondary" iconLeading={FolderSearch} aria-expanded={browsing} aria-controls="ssh-workspace-browser" isDisabled={!!busy} onClick={() => setBrowsing((open) => !open)}>{t("ssh.browse")}</Button></div><p id="ssh-workspace-hint" className="text-xs text-tertiary">{t("ssh.workspaceHint")}</p></div>
                            {browsing && <RemoteDirectoryPicker id="ssh-workspace-browser" target={{ alias: request.alias }} newFolder initialPath={(request.workspace_dir ?? defaultWorkspace).trim() || defaultWorkspace} isDisabled={!!busy} onPick={(workspace_dir) => { edit({ workspace_dir }); setBrowsing(false); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-workspace"]')?.focus(); }} onClose={() => setBrowsing(false)} />}
                        </div>}
                        <details className="rounded-lg border border-secondary p-3">
                            <summary className="cursor-pointer text-sm font-medium text-secondary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("ssh.networkSettings")}</summary>
                            <div className="mt-4 space-y-4">
                                <p className="text-xs leading-5 text-tertiary">{t("ssh.networkHint")}</p>
                                <Input size="sm" label={t("ssh.raftAddress")} name="ssh-raft-address" autoComplete="off" spellCheck="false" placeholder="192.0.2.7:7702…" hint={t("ssh.raftAddressHint")} value={request.raft_addr || ""} onChange={(raft_addr) => edit({ raft_addr })} isDisabled={!!busy} />
                            </div>
                        </details>
                        {peerInstallation ? <div className="space-y-3 rounded-lg bg-secondary p-4"><h3 className="text-sm font-semibold text-primary">{t("ssh.peerScope")}</h3><p className="text-sm leading-6 text-secondary">{t("ssh.peerScopeHint")}</p><p className="text-xs leading-5 text-tertiary">{t("ssh.peerScopeLogin")}</p><p className="text-xs leading-5 text-tertiary">{t("ssh.peerScopeAuto")}</p>{needsPeerConsent && <p className="text-sm leading-6 text-secondary">{t("ssh.peerConsent", { level: levelName(request.level, locale) })}</p>}</div> : <ExecutionDataLevel value={request.level} isDisabled={!!busy} onChange={(level) => edit({ level })} />}</>}
                    <div className="flex flex-wrap gap-2">{check.reachable && !existingInstallation && <Button size="md" isLoading={busy === "plan"} onClick={() => void preparePlan(needsPeerConsent)}>{t(needsPeerConsent ? "ssh.allowPeerReview" : "ssh.review")}</Button>}<Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setError(""); save({ request }); }}>{t("ssh.changeMachine")}</Button></div>
                    {check.reachable && !existingInstallation && peerInstallation && <div className="space-y-2 border-t border-secondary pt-4"><p className="text-xs leading-5 text-tertiary">{t("ssh.executionOnlyHint")}</p><Button size="md" color="secondary" isDisabled={!!busy} onClick={() => onAddExecutor(request)}>{t("ssh.executionOnly")}</Button></div>}
                </section>}
                {plan && <section className="space-y-4">
                    <h2 ref={stageHeading} tabIndex={-1} className="text-base font-semibold text-primary focus-visible:outline-2 focus-visible:outline-focus-ring">{t(connected ? "ssh.connected" : result?.status === "needs_attention" ? "ssh.attention" : "ssh.plan")}</h2>
                    <dl className="grid min-w-0 grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-xs"><dt className="text-tertiary">{t(plan.check.installation_mode === "peer" ? "ssh.machineName" : "ssh.name")}</dt><dd className="break-all font-medium text-primary">{plan.request.name}</dd><dt className="text-tertiary">SSH</dt><dd className="break-all text-secondary">{plan.request.alias}</dd><dt className="text-tertiary">{t("ssh.address")}</dt><dd className="break-all font-mono text-secondary">{plan.request.addr}</dd>{plan.request.workspace_dir && <><dt className="text-tertiary">{t("ssh.workspace")}</dt><dd className="break-all font-mono text-secondary">{plan.request.workspace_dir}</dd></>}<dt className="text-tertiary">{t("ssh.level")}</dt><dd className="text-secondary">{levelName(plan.request.level, locale)}</dd></dl>
                    {plan.request.raft_addr && <dl className="grid min-w-0 grid-cols-[minmax(0,auto)_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">
                        <dt className="text-tertiary">{t("ssh.raftAddress")}</dt><dd className="break-all font-mono text-secondary">{plan.request.raft_addr}</dd>
                    </dl>}
                    {!attempted ? <>
                        <div><h3 className="mb-2 text-sm font-semibold text-primary">{t("ssh.effects")}</h3><ul className="list-disc space-y-1 pl-5 text-sm leading-6 text-secondary">{plan.effects.map((effect, index) => <li key={index} className="break-words">{effect}</li>)}</ul></div>
                        <SSHSteps steps={plan.steps} />
                        {!!plan.script && <details className="rounded-lg bg-secondary p-3"><summary className="text-xs text-tertiary">{t("ssh.script")}</summary><pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-words font-mono text-xs text-secondary [overflow-wrap:anywhere]">{plan.script}</pre></details>}
                        <p className="text-xs text-quaternary">{t("ssh.expires", { time: dateTime(plan.expires_at, locale, { dateStyle: "short", timeStyle: "short" }) })}</p>
                        {(!plan.ready || expired) && <p role="status" className="text-sm text-error-primary">{t(expired ? "ssh.expired" : "ssh.blocked")}</p>}
                        <div className="flex flex-wrap gap-2"><Button size="md" isDisabled={!plan.ready || expired || !!busy} onClick={() => void install()}>{t("ssh.install")}</Button><Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setError(""); save({ request: plan.request, check: usableCheck(plan.check) }); }}>{t("ssh.edit")}</Button></div>
                    </> : <>
                        <p role="status" className="text-sm font-medium text-secondary">{t(busy ? canResumeRegistration ? "ssh.recheckingConnection" : "ssh.installing" : connected ? "ssh.connectedHint" : result?.status === "installing" ? "ssh.waitingForResult" : result?.status === "needs_attention" ? result.registered ? "ssh.registered" : "ssh.unregistered" : "ssh.unconfirmed")}</p>
                        {result && <InstallProgress result={result} />}
                        {result && <SSHSteps steps={result.steps} />}
                        {result && <InstallLog result={result} />}
                        {canResumeRegistration && <p className="text-sm leading-6 text-secondary">{t("ssh.resumeHint")}</p>}
                        {result?.status === "needs_attention" && result.registered && <p className="text-sm leading-6 text-tertiary">{t("ssh.attentionHint")}</p>}
                        {canAbandon && <p className="text-sm leading-6 text-tertiary">{t(result.registered ? "ssh.abandonHint" : "ssh.abandonPlanHint")}</p>}
                        <p className="break-all text-xs text-quaternary">{t("ssh.planId")}: <span className="font-mono">{plan.id}</span></p>
                        <div className="flex flex-wrap gap-2">{(!terminal || canResumeRegistration) && <Button size="md" isLoading={busy === "install"} onClick={() => void install()}>{t(canResumeRegistration ? "ssh.resumeRegistration" : "ssh.checkInstallation")}</Button>}{canAbandon && <Button size="md" color="secondary-destructive" isLoading={busy === "abandon"} isDisabled={busy !== null && busy !== "abandon"} onClick={() => void abandon()}>{t("ssh.abandon")}</Button>}<Button size="md" color={connected ? "primary" : "secondary"} isDisabled={!!busy} onClick={close}>{t(connected ? "ssh.done" : "ssh.backToResources")}</Button>{connected && <Button size="md" color="secondary" onClick={() => setEnrolling(true)}>{t("nodeAgents.entry")}</Button>}{terminal && <Button size="md" color="tertiary" isDisabled={!!busy} onClick={() => { setError(""); save({ request: initialRequest }); }}>{t("ssh.connectAnother")}</Button>}</div>
                    </>}
                </section>}
                {busy && busy !== "install" && <p role="status" className="mt-3 text-sm text-tertiary">{t(busy === "check" ? "ssh.checking" : busy === "abandon" ? "ssh.abandoning" : "ssh.planning")}</p>}
                {enrolling && connected && result && <NodeAgentEnrollment node={result.node_id || result.name} name={result.name} onClose={() => setEnrolling(false)} onRegistered={onChanged} />}
                {error && <p role="alert" className="mt-3 break-words text-sm text-error-primary">{error}</p>}
            </DialogBody></DialogSurface>
        </Dialog></Modal>
    </ModalOverlay>;
}

const phaseLabels = { preflight: "ssh.phase.preflight", registration: "ssh.phase.registration", link: "ssh.phase.link", upload: "ssh.phase.upload", installation: "ssh.phase.installation", connectivity: "ssh.phase.connectivity" } as const;

// InstallProgress shows where an installation is among its phases: the
// ones behind it, the one it is in, and, for one that stopped, where.
export function InstallProgress({ result }: { result: SSHInstallResult }) {
    const { t } = useI18n();
    const phases = result.phases ?? [];
    if (phases.length === 0) return null;
    const label = (phase: string) => phase in phaseLabels ? t(phaseLabels[phase as keyof typeof phaseLabels]) : phase;
    const index = result.phase ? phases.indexOf(result.phase) : -1;
    const done = result.status === "connected";
    const stopped = result.status === "needs_attention";
    const completed = done ? phases.length : Math.max(index, 0);
    const total = phases.length;
    const text = done ? t("ssh.progressDone", { total }) : index >= 0 ? t(stopped ? "ssh.progressStopped" : "ssh.progressStep", { current: index + 1, total, phase: label(phases[index]) }) : "";
    return <div className="space-y-2">
        {text && <p aria-live="polite" className="text-sm font-medium text-primary">{text}</p>}
        <div role="progressbar" aria-label={t("ssh.progress")} aria-valuenow={completed} aria-valuemin={0} aria-valuemax={total} aria-valuetext={text || undefined} className="h-2 w-full overflow-hidden rounded-md bg-quaternary">
            <div style={{ transform: `translateX(-${100 - (completed * 100) / total}%)` }} className={`size-full rounded-md transition duration-300 ease-out motion-reduce:transition-none ${stopped ? "bg-fg-error-primary" : done ? "bg-fg-success-primary" : "bg-fg-brand-primary"}`} />
        </div>
        <ol className="flex flex-wrap gap-x-3 gap-y-1 text-xs">{phases.map((phase, i) => <li key={phase} aria-current={i === index && !done ? "step" : undefined} className={i < completed || done ? "text-secondary" : i === index ? (stopped ? "font-medium text-error-primary" : "font-medium text-primary") : "text-quaternary"}>{label(phase)}</li>)}</ol>
    </div>;
}

// InstallLog is what the installation said, in order: Steve narrating each
// phase and the machine's own output. Open by default when something went
// wrong, since that is when the lines matter.
export function InstallLog({ result }: { result: SSHInstallResult }) {
    const { t, locale } = useI18n();
    const lines = result.log ?? [];
    const box = useRef<HTMLDivElement>(null);
    const stopped = result.status === "needs_attention";
    // Follow the tail inside the log box only; the dialog itself stays put.
    useEffect(() => { if (result.status === "installing" && box.current) box.current.scrollTop = box.current.scrollHeight; }, [lines.length, result.status]);
    if (lines.length === 0) return null;
    return <details className="rounded-lg bg-secondary p-3" open={stopped || result.status === "installing"}>
        <summary className="cursor-pointer text-xs font-medium text-secondary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("ssh.logCount", { count: lines.length })}</summary>
        <div ref={box} className="mt-2 max-h-56 overflow-auto font-mono text-xs leading-5" aria-label={t("ssh.log")}>
            {lines.map((line, i) => <div key={i} className={`flex gap-3 whitespace-pre-wrap break-words [overflow-wrap:anywhere] ${line.stream === "stderr" ? "text-error-primary" : line.stream === "steve" ? "text-primary" : "text-secondary"}`}><span className="shrink-0 tabular-nums text-quaternary">{dateTime(line.at, locale, { timeStyle: "medium" })}</span><span className="min-w-0">{line.text}</span></div>)}
        </div>
    </details>;
}

export function SSHSteps({ steps }: { steps: SSHStep[] }) {
    const { t } = useI18n();
    return <ul aria-label={t("ssh.steps")} className="space-y-3 rounded-lg bg-secondary p-3">{steps.map((step, index) => <li key={`${step.id}-${index}`} className="flex min-w-0 gap-2">
        {step.status === "ready" ? <CheckCircle className="mt-0.5 size-4 shrink-0 text-fg-success-primary" aria-hidden="true" /> : <XCircle className="mt-0.5 size-4 shrink-0 text-fg-error-primary" aria-hidden="true" />}
        <div className="min-w-0"><p className="break-words text-sm text-secondary">{step.message}</p>{step.suggestion && <p className="mt-1 break-words text-xs leading-5 text-tertiary">{step.suggestion}</p>}</div>
    </li>)}</ul>;
}

export function ExecutionDataLevel({ value, onChange, isDisabled = false }: { value: string; onChange: (value: string) => void; isDisabled?: boolean }) {
    const { t } = useI18n();
    const descriptions = { public: "ssh.levelPublic", internal: "ssh.levelInternal", restricted: "ssh.levelRestricted", sealed: "ssh.levelSealed" } as const;
    const items = (Object.keys(descriptions) as (keyof typeof descriptions)[]).map((id) => ({ id, label: t(descriptions[id]) }));
    return <Select size="sm" label={t("ssh.executorLevel")} hint={t("ssh.executorLevelHint")} selectedKey={value} isDisabled={isDisabled} onSelectionChange={(key) => key && onChange(String(key))} items={items}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select>;
}
