import { NodeAgentEnrollment } from "@/components/steve/node-agent-enrollment";
import { useEffect, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { CheckCircle, XCircle, X } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useI18n } from "@/providers/locale-provider";
import { dateTime } from "@/lib/format";
import { levelName } from "@/lib/workspaces";
import { checkSSH, discoverSSH, installSSH, planSSH, type SSHCheck, type SSHDiscovery, type SSHInstallRequest, type SSHInstallResult, type SSHPlan, type SSHStep } from "@/lib/api/ssh";

interface SSHRecord { plan: SSHPlan; result: SSHInstallResult }
interface SSHDraft { request: SSHInstallRequest; check?: SSHCheck; plan?: SSHPlan; attempted?: boolean; result?: SSHInstallResult; history?: SSHRecord[] }
const initialRequest: SSHInstallRequest = { alias: "", name: "", addr: "", level: "restricted" };
function draftKey() { return `steve.ssh.connect:${new URL(".", window.location.href).href}`; }
function readDraft(): SSHDraft {
    try {
        const saved = JSON.parse(localStorage.getItem(draftKey()) || "null");
        if (!saved || !saved.request || typeof saved.request.alias !== "string") return { request: initialRequest };
        if (saved.plan && (!saved.plan.id || !Array.isArray(saved.plan.steps) || !Array.isArray(saved.plan.effects))) return { request: initialRequest };
        return saved;
    } catch { return { request: initialRequest }; }
}
function addressFor(host?: string) { return host ? `${host.includes(":") ? `[${host}]` : host}:7701` : ""; }
function suggestedName(alias: string) { return alias.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "").slice(0, 64); }
function sameRequest(a: SSHInstallRequest, b: SSHInstallRequest) { return a.alias === b.alias && a.name === b.name && a.addr === b.addr && a.level === b.level && (a.raft_addr || "") === (b.raft_addr || "") && (a.source_host || "") === (b.source_host || ""); }

export function SSHConnect({ onClose, onChanged }: { onClose: () => void; onChanged: () => void }) {
    const { t, locale } = useI18n();
    const [draft, setDraft] = useState(readDraft);
    const [enrolling, setEnrolling] = useState(false);
    const [discovery, setDiscovery] = useState<SSHDiscovery>({ candidates: [], warnings: [], revision: "" });
    const [loading, setLoading] = useState(true);
    const [readError, setReadError] = useState("");
    const [error, setError] = useState("");
    const [busy, setBusy] = useState<"check" | "plan" | "install" | null>(null);
    const acting = useRef(false);
    const fields = useRef<HTMLDivElement>(null);
    const candidates = useRef<HTMLDivElement>(null);
    const [now, setNow] = useState(Date.now);
    const { request, check, plan, attempted, result } = draft;
    const load = useResourceRead("ssh-candidates", discoverSSH, (value) => { setDiscovery({ ...value, candidates: value.candidates || [], warnings: value.warnings || [] }); setReadError(""); setLoading(false); }, (error) => { setReadError(String(error).replace(/^Error: /, "")); setLoading(false); });
    useEffect(() => { if (!plan) void load(); }, [load, plan]);
    useEffect(() => { if (!plan) return; const ms = Date.parse(plan.expires_at) - Date.now(); if (ms <= 0) return; const timer = window.setTimeout(() => setNow(Date.now()), Math.min(ms + 1, 2_147_483_647)); return () => window.clearTimeout(timer); }, [plan]);
    const expired = !!plan && Date.parse(plan.expires_at) <= now;
    const connected = result?.registered === true && result.connected === true && result.status === "connected";
    const terminal = connected || result?.status === "needs_attention";
    function save(next: SSHDraft) {
        next = { ...next, history: next.history || draft.history || [] };
        setDraft(next);
        try { localStorage.setItem(draftKey(), JSON.stringify(next)); return true; }
        catch { setError(t("ssh.storageError")); return false; }
    }
    function edit(patch: Partial<SSHInstallRequest>) { if (attempted || acting.current) return; setError(""); save({ ...draft, request: { ...request, ...patch }, plan: undefined }); }
    async function testConnection() {
        if (acting.current || attempted) return;
        if (!request.alias) { setError(t("ssh.selectRequired")); candidates.current?.querySelector<HTMLInputElement>('input[type="radio"]')?.focus(); return; }
        acting.current = true; setBusy("check"); setError("");
        try {
            const checked = await checkSSH(request.alias);
            if (!checked?.candidate || checked.candidate.alias !== request.alias || !Array.isArray(checked.steps)) throw new Error(t("ssh.responseInvalid"));
            save({ request: { ...request, name: request.name || suggestedName(request.alias), addr: request.addr || addressFor(checked.address) }, check: checked });
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    async function preparePlan() {
        if (acting.current || attempted || !check?.reachable) return;
        const body = { ...request, name: request.name.trim(), addr: request.addr.trim(), raft_addr: request.raft_addr?.trim() || undefined, source_host: request.source_host?.trim() || undefined };
        if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(body.name)) { setError(t("ssh.nameInvalid")); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-node-name"]')?.focus(); return; }
        if (!body.addr) { setError(t("ssh.addressRequired")); fields.current?.querySelector<HTMLInputElement>('input[name="ssh-node-address"]')?.focus(); return; }
        acting.current = true; setBusy("plan"); setError("");
        try {
            const next = await planSSH(body);
            if (!next?.id || !next.request || !sameRequest(next.request, body) || !Array.isArray(next.steps) || !Array.isArray(next.effects) || !Number.isFinite(Date.parse(next.expires_at))) throw new Error(t("ssh.responseInvalid"));
            setNow(Date.now()); save({ request: body, check: next.check, plan: next });
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    async function install() {
        if (acting.current || !plan || terminal) return;
        if (!attempted && (!plan.ready || Date.parse(plan.expires_at) <= Date.now())) { setNow(Date.now()); setError(t(plan.ready ? "ssh.expired" : "ssh.blocked")); return; }
        if (!save({ ...draft, attempted: true })) return;
        acting.current = true; setBusy("install"); setError("");
        try {
            const next = await installSSH(plan.id);
            if (next?.plan_id !== plan.id) throw new Error(t("ssh.otherInstallation"));
            if (typeof next.registered !== "boolean" || typeof next.connected !== "boolean" || !["connected", "needs_attention", "installing"].includes(next.status) || !Array.isArray(next.steps) || (next.status === "connected" && (!next.registered || !next.connected))) throw new Error(t("ssh.responseInvalid"));
            const history = next.status === "connected" || next.status === "needs_attention" ? [...(draft.history || []).filter((record) => record.plan.id !== plan.id), { plan, result: next }] : draft.history;
            save({ ...draft, attempted: true, result: next, history });
            if (next.registered) onChanged();
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { acting.current = false; setBusy(null); }
    }
    function close() {
        if (acting.current) return;
        if (connected) save({ request: initialRequest });
        onClose();
    }
    return <ModalOverlay isOpen isDismissable={!busy} isKeyboardDismissDisabled={!!busy} onOpenChange={(open) => { if (!open) close(); }} className="motion-reduce:animate-none motion-reduce:duration-0">
        <Modal className="max-w-2xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("ssh.title")} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary">
            <div className="max-h-[min(840px,85dvh)] overflow-y-auto overscroll-contain p-5 sm:p-6">
                <header className="mb-5 flex items-start gap-3"><div className="min-w-0 flex-1"><h1 className="text-lg font-semibold text-primary">{t("ssh.title")}</h1><p className="mt-2 text-sm leading-6 text-tertiary">{t("ssh.description")}</p></div><Button size="sm" color="tertiary" iconLeading={X} aria-label={t("ssh.close")} isDisabled={!!busy} onClick={close} /></header>
                {!check && !plan && <section className="space-y-4">
                    {loading && <p role="status" className="text-sm text-tertiary">{t("ssh.loading")}</p>}
                    {!loading && !readError && discovery.candidates.length === 0 && <div className="space-y-2 rounded-lg bg-secondary p-4"><h2 className="text-sm font-semibold text-primary">{t("ssh.empty")}</h2><p className="text-sm leading-6 text-tertiary">{t("ssh.emptyHint")}</p></div>}
                    <RadioGroup ref={candidates} aria-label={t("ssh.candidates")} value={request.alias} isDisabled={!!busy} onChange={(alias) => { setError(""); save({ request: { ...initialRequest, alias, name: suggestedName(alias) } }); }} className="space-y-2">
                        {discovery.candidates.map((candidate) => <Radio key={candidate.alias} value={candidate.alias} className="group flex min-h-12 cursor-pointer gap-3 rounded-lg border border-secondary p-3 data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring">
                            <span className="mt-1 size-4 shrink-0 rounded-full border border-primary group-data-selected:border-4 group-data-selected:border-brand" aria-hidden="true" />
                            <span className="flex min-w-0 flex-col gap-1"><span className="text-sm font-semibold text-primary">{candidate.alias}</span><span className="break-all font-mono text-xs text-secondary">{candidate.user ? candidate.user + "@" : ""}{candidate.host_name}:{candidate.port}</span>{candidate.proxy_jump && <span className="break-words text-xs text-tertiary">{t("ssh.via", { proxy: candidate.proxy_jump })}</span>}{candidate.has_proxy_command && <span className="text-xs text-tertiary">{t("ssh.proxyCommand")}</span>}{candidate.conditional && <span className="text-xs text-tertiary">{t("ssh.conditional")}</span>}</span>
                        </Radio>)}
                    </RadioGroup>
                    {discovery.warnings.length > 0 && <details className="rounded-lg bg-secondary p-3" open><summary className="text-xs font-medium text-secondary">{t("ssh.warnings")}</summary><ul className="mt-2 space-y-2 text-xs leading-5 text-tertiary">{discovery.warnings.map((warning, i) => <li key={i}><p>{warning.message}</p><span className="break-all font-mono">{warning.source}{warning.line ? `:${warning.line}` : ""}</span></li>)}</ul></details>}
                    {!!draft.history?.length && <details className="rounded-lg border border-secondary p-3"><summary className="text-xs font-medium text-tertiary">{t("ssh.history", { count: draft.history.length })}</summary><ul className="mt-3 space-y-3">{draft.history.toReversed().map((record) => <li key={record.plan.id} className="flex min-w-0 flex-wrap items-center justify-between gap-2"><div className="min-w-0"><p className="break-all text-sm font-medium text-primary">{record.plan.request.name}</p><p className="text-xs text-tertiary">{t(record.result.connected ? "ssh.historyConnected" : "ssh.attention")}</p></div><Button size="sm" color="secondary" onClick={() => { setError(""); save({ request: record.plan.request, check: record.plan.check, plan: record.plan, attempted: true, result: record.result }); }}>{t("ssh.viewRecord")}</Button></li>)}</ul></details>}
                    {readError && <p role="alert" className="break-words text-sm text-error-primary">{readError}</p>}
                    <div className="flex flex-wrap gap-2"><Button size="md" isLoading={busy === "check"} isDisabled={loading || !!readError} onClick={() => void testConnection()}>{t("ssh.check")}</Button><Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setLoading(true); void load(); }}>{t("ssh.refresh")}</Button></div>
                </section>}
                {check && !plan && <section className="space-y-4" ref={fields}>
                    <p role="status" className="text-sm font-medium text-secondary">{t(check.reachable ? "ssh.reachable" : "ssh.notReachable")}</p>
                    <SSHSteps steps={check.steps} />
                    {check.reachable && <><Input size="sm" label={t("ssh.name")} name="ssh-node-name" autoComplete="off" spellCheck="false" placeholder="worker-west…" hint={t("ssh.nameHint")} value={request.name} onChange={(name) => edit({ name })} isDisabled={!!busy} />
                        <Input size="sm" label={t("ssh.address")} name="ssh-node-address" autoComplete="off" spellCheck="false" placeholder="192.0.2.7:7701…" hint={t("ssh.addressHint")} value={request.addr} onChange={(addr) => edit({ addr })} isDisabled={!!busy} />
                        <details className="rounded-lg border border-secondary p-3">
                            <summary className="cursor-pointer text-sm font-medium text-secondary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("ssh.networkSettings")}</summary>
                            <div className="mt-4 space-y-4">
                                <p className="text-xs leading-5 text-tertiary">{t("ssh.networkHint")}</p>
                                <Input size="sm" label={t("ssh.raftAddress")} name="ssh-raft-address" autoComplete="off" spellCheck="false" placeholder="192.0.2.7:7702…" hint={t("ssh.raftAddressHint")} value={request.raft_addr || ""} onChange={(raft_addr) => edit({ raft_addr })} isDisabled={!!busy} />
                                <Input size="sm" label={t("ssh.sourceHost")} name="ssh-source-host" autoComplete="off" spellCheck="false" placeholder="192.0.2.4…" hint={t("ssh.sourceHostHint")} value={request.source_host || ""} onChange={(source_host) => edit({ source_host })} isDisabled={!!busy} />
                            </div>
                        </details>
                        <Select size="sm" label={t("ssh.level")} hint={t("ssh.levelHint")} selectedKey={request.level} isDisabled={!!busy} onSelectionChange={(value) => value && edit({ level: String(value) })} items={["public", "internal", "restricted", "sealed"].map((id) => ({ id, label: levelName(id, locale) }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></>}
                    <div className="flex flex-wrap gap-2">{check.reachable && <Button size="md" isLoading={busy === "plan"} onClick={() => void preparePlan()}>{t("ssh.review")}</Button>}<Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setError(""); save({ request }); }}>{t("ssh.changeMachine")}</Button></div>
                </section>}
                {plan && <section className="space-y-4">
                    <h2 className="text-base font-semibold text-primary">{t(connected ? "ssh.connected" : result?.status === "needs_attention" ? "ssh.attention" : "ssh.plan")}</h2>
                    <dl className="grid min-w-0 grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-xs"><dt className="text-tertiary">{t("ssh.name")}</dt><dd className="break-all font-medium text-primary">{plan.request.name}</dd><dt className="text-tertiary">SSH</dt><dd className="break-all text-secondary">{plan.request.alias}</dd><dt className="text-tertiary">{t("ssh.address")}</dt><dd className="break-all font-mono text-secondary">{plan.request.addr}</dd><dt className="text-tertiary">{t("ssh.level")}</dt><dd className="text-secondary">{levelName(plan.request.level, locale)}</dd></dl>
                    {(plan.request.raft_addr || plan.request.source_host) && <dl className="grid min-w-0 grid-cols-[minmax(0,auto)_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">
                        {plan.request.raft_addr && <><dt className="text-tertiary">{t("ssh.raftAddress")}</dt><dd className="break-all font-mono text-secondary">{plan.request.raft_addr}</dd></>}
                        {plan.request.source_host && <><dt className="text-tertiary">{t("ssh.sourceHost")}</dt><dd className="break-all font-mono text-secondary">{plan.request.source_host}</dd></>}
                    </dl>}
                    {!attempted ? <>
                        <div><h3 className="mb-2 text-sm font-semibold text-primary">{t("ssh.effects")}</h3><ul className="list-disc space-y-1 pl-5 text-sm leading-6 text-secondary">{plan.effects.map((effect, index) => <li key={index} className="break-words">{effect}</li>)}</ul></div>
                        <SSHSteps steps={plan.steps} />
                        {!!plan.script && <details className="rounded-lg bg-secondary p-3"><summary className="text-xs text-tertiary">{t("ssh.script")}</summary><pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-words font-mono text-xs text-secondary [overflow-wrap:anywhere]">{plan.script}</pre></details>}
                        <p className="text-xs text-quaternary">{t("ssh.expires", { time: dateTime(plan.expires_at, locale, { dateStyle: "short", timeStyle: "short" }) })}</p>
                        {(!plan.ready || expired) && <p role="status" className="text-sm text-error-primary">{t(expired ? "ssh.expired" : "ssh.blocked")}</p>}
                        <div className="flex flex-wrap gap-2"><Button size="md" isDisabled={!plan.ready || expired || !!busy} onClick={() => void install()}>{t("ssh.install")}</Button><Button size="md" color="secondary" isDisabled={!!busy} onClick={() => { setError(""); save({ request: plan.request, check: plan.check }); }}>{t("ssh.edit")}</Button></div>
                    </> : <>
                        <p role="status" className="text-sm font-medium text-secondary">{t(busy ? "ssh.installing" : connected ? "ssh.connectedHint" : result?.status === "installing" ? "ssh.waitingForResult" : result?.status === "needs_attention" ? result.registered ? "ssh.registered" : "ssh.unregistered" : "ssh.unconfirmed")}</p>
                        {result && <SSHSteps steps={result.steps} />}
                        {result?.registered && !connected && <p className="text-sm leading-6 text-tertiary">{t("ssh.attentionHint")}</p>}
                        <p className="break-all text-xs text-quaternary">{t("ssh.planId")}: <span className="font-mono">{plan.id}</span></p>
                        <div className="flex flex-wrap gap-2">{!terminal && <Button size="md" isLoading={busy === "install"} onClick={() => void install()}>{t("ssh.checkInstallation")}</Button>}<Button size="md" color={connected ? "primary" : "secondary"} isDisabled={!!busy} onClick={close}>{t(connected ? "ssh.done" : "ssh.backToResources")}</Button>{connected && <Button size="md" color="secondary" onClick={() => setEnrolling(true)}>{t("nodeAgents.entry")}</Button>}{terminal && <Button size="md" color="tertiary" onClick={() => { setError(""); save({ request: initialRequest }); }}>{t("ssh.connectAnother")}</Button>}</div>
                    </>}
                </section>}
                {busy && busy !== "install" && <p role="status" className="mt-3 text-sm text-tertiary">{t(busy === "check" ? "ssh.checking" : "ssh.planning")}</p>}
                {enrolling && connected && result && <NodeAgentEnrollment node={result.node_id || result.name} name={result.name} onClose={() => setEnrolling(false)} onRegistered={onChanged} />}
                {error && <p role="alert" className="mt-3 break-words text-sm text-error-primary">{error}</p>}
            </div>
        </Dialog></Modal>
    </ModalOverlay>;
}

function SSHSteps({ steps }: { steps: SSHStep[] }) {
    const { t } = useI18n();
    return <ul aria-label={t("ssh.steps")} className="space-y-3 rounded-lg bg-secondary p-3">{steps.map((step, index) => <li key={`${step.id}-${index}`} className="flex min-w-0 gap-2">
        {step.status === "ready" ? <CheckCircle className="mt-0.5 size-4 shrink-0 text-fg-success-primary" aria-hidden="true" /> : <XCircle className="mt-0.5 size-4 shrink-0 text-fg-error-primary" aria-hidden="true" />}
        <div className="min-w-0"><p className="break-words text-sm text-secondary">{step.message}</p>{step.suggestion && <p className="mt-1 break-words text-xs leading-5 text-tertiary">{step.suggestion}</p>}</div>
    </li>)}</ul>;
}
