import { useEffect, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useI18n } from "@/providers/locale-provider";
import { discoverNodeAgents, enrollNodeAgent, type NodeAgentCandidate, type NodeAgentDiscovery, type NodeAgentEnrollment, type NodeAgentRequest } from "@/lib/api/node-agents";

interface EnrollmentDraft { candidate: string; agent: string; pending?: NodeAgentRequest }
function readDraft(key: string): EnrollmentDraft {
    try { const value = JSON.parse(localStorage.getItem(key) || "null"); return { candidate: typeof value?.candidate === "string" ? value.candidate : "", agent: typeof value?.agent === "string" ? value.agent : "", ...(value?.pending?.candidate_id && value.pending.agent_id ? { pending: value.pending } : {}) }; }
    catch { return { candidate: "", agent: "" }; }
}
function available(candidate: NodeAgentCandidate) { return candidate.installed && !candidate.requires?.length && !candidate.registered; }
function suggestedAgent(node: string, candidate: string) { return `${node}-${candidate}`.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "").slice(0, 64); }

export function NodeAgentEnrollment({ node, name = node, onClose, onRegistered }: { node: string; name?: string; onClose: () => void; onRegistered: () => void }) {
    const { t } = useI18n();
    const key = `steve.node-agent.enrollment:${new URL(".", window.location.href).href}:${encodeURIComponent(node)}`;
    const [draft, setDraft] = useState(() => readDraft(key));
    const [discovery, setDiscovery] = useState<NodeAgentDiscovery>({ revision: "", agents: [] });
    const [loading, setLoading] = useState(true);
    const [readError, setReadError] = useState("");
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState<NodeAgentEnrollment | null>(null);
    const acting = useRef(false);
    const form = useRef<HTMLDivElement>(null);
    const load = useResourceRead(`node-agents:${node}`, (signal) => discoverNodeAgents(node, signal), (value) => { setDiscovery({ ...value, agents: value.agents || [] }); setReadError(""); setLoading(false); }, (error) => { setReadError(error instanceof Error ? error.message : String(error)); setLoading(false); });
    useEffect(() => { void load(); }, [load]);
    const selected = discovery.agents.find((candidate) => candidate.id === draft.candidate);
    function save(next: EnrollmentDraft) {
        setDraft(next);
        try { localStorage.setItem(key, JSON.stringify(next)); return true; }
        catch { setError(t("nodeAgents.storageError")); return false; }
    }
    async function register() {
        if (acting.current || result) return;
        if (!draft.pending && (!selected || !available(selected))) { setError(t("nodeAgents.selectRequired")); form.current?.querySelector<HTMLInputElement>('input[type="radio"]:not(:disabled)')?.focus(); return; }
        const agent = draft.agent.trim();
        if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(agent)) { setError(t("nodeAgents.invalidName")); form.current?.querySelector<HTMLInputElement>('input[name="node-agent-name"]')?.focus(); return; }
        const body = draft.pending || { candidate_id: draft.candidate, agent_id: agent, expected_revision: discovery.revision };
        if (!body.expected_revision || !save({ ...draft, agent, pending: body })) return;
        acting.current = true; setBusy(true); setError("");
        try {
            let current = discovery;
            if (draft.pending) {
                current = await discoverNodeAgents(node);
                if (!current.revision || !Array.isArray(current.agents)) throw new Error(t("nodeAgents.invalidResponse"));
                setDiscovery(current); setReadError("");
            }
            const request = { ...body, expected_revision: current.revision };
            if (!save({ ...draft, agent, pending: request })) return;
            const receipt = await enrollNodeAgent(node, request);
            const candidate = current.agents.find((item) => item.id === request.candidate_id);
            if (!receipt || receipt.candidate_id !== request.candidate_id || receipt.agent_id !== request.agent_id || receipt.harness !== candidate?.harness || !receipt.revision || receipt.registered !== true) throw new Error(t("nodeAgents.invalidResponse"));
            setResult(receipt);
            try { localStorage.removeItem(key); } catch { /* The acknowledged registration remains authoritative. */ }
            onRegistered();
        } catch (error) {
            setError(error instanceof Error ? error.message : String(error));
            await load();
        } finally { acting.current = false; setBusy(false); }
    }
    return <ModalOverlay isOpen isDismissable={!busy} isKeyboardDismissDisabled={busy} onOpenChange={(open) => { if (!open && !busy) onClose(); }} className="z-[100] motion-reduce:animate-none motion-reduce:duration-0"><Modal className="max-w-xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("nodeAgents.title", { node: name })} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary"><div className="max-h-[min(800px,85dvh)] overflow-y-auto overscroll-contain p-5 sm:p-6" ref={form}>
        <header className="mb-5 space-y-2"><h2 className="break-words text-lg font-semibold text-primary">{t("nodeAgents.title", { node: name })}</h2><p className="text-sm leading-6 text-secondary">{t("nodeAgents.description")}</p><p className="text-xs leading-5 text-tertiary">{t("nodeAgents.presenceHint")}</p></header>
        {result ? <div className="space-y-3"><h3 role="status" className="text-base font-semibold text-primary">{t("nodeAgents.success")}</h3><p className="break-words text-sm leading-6 text-secondary">{t("nodeAgents.successHint", { agent: result.agent_id || draft.agent, node: name })}</p><Button size="md" onClick={onClose}>{t("nodeAgents.done")}</Button></div> : <div className="space-y-4">
            {loading && <p role="status" className="text-sm text-tertiary">{t("nodeAgents.loading")}</p>}
            {!loading && !readError && !discovery.agents.some((candidate) => candidate.installed) && <div className="space-y-1 rounded-lg bg-secondary p-3"><p className="text-sm font-medium text-primary">{t("nodeAgents.empty")}</p><p className="text-xs leading-5 text-tertiary">{t("nodeAgents.emptyHint")}</p></div>}
            <RadioGroup aria-label={t("nodeAgents.tools")} value={draft.candidate} isDisabled={busy || !!draft.pending} onChange={(candidate) => { setError(""); save({ candidate, agent: draft.agent || suggestedAgent(node, candidate) }); }} className="space-y-2">
                {discovery.agents.map((candidate) => <Radio key={candidate.id} value={candidate.id} aria-label={candidate.name} isDisabled={!available(candidate)} className="group flex min-h-12 min-w-0 cursor-pointer gap-3 rounded-lg border border-secondary p-3 data-selected:bg-secondary data-disabled:cursor-default data-focus-visible:outline-2 data-focus-visible:outline-focus-ring"><span className="mt-1 size-4 shrink-0 rounded-full border border-primary group-data-selected:border-4 group-data-selected:border-brand group-data-disabled:opacity-50" aria-hidden="true" /><span className="flex min-w-0 flex-col gap-1"><span className="flex min-w-0 flex-wrap gap-x-3 gap-y-1"><span className="text-sm font-medium text-primary">{candidate.name}</span><span className="text-xs text-tertiary">{t(candidate.registered ? "nodeAgents.registered" : candidate.configured ? "nodeAgents.configured" : candidate.installed ? "nodeAgents.detected" : "nodeAgents.notInstalled")}</span></span>{candidate.executable && <span className="break-all font-mono text-xs text-tertiary">{candidate.executable}</span>}{!!candidate.requires?.length && <span className="text-xs text-error-primary">{t("nodeAgents.requires", { tools: candidate.requires.join(", ") })}</span>}</span></Radio>)}
            </RadioGroup>
            {(selected || draft.pending) && <><Input size="sm" label={t("nodeAgents.agentName")} name="node-agent-name" autoComplete="off" spellCheck="false" placeholder="remote-codex…" hint={t("nodeAgents.agentHint")} value={draft.agent} isDisabled={busy || !!draft.pending} onChange={(agent) => { setError(""); save({ ...draft, agent }); }} /><p className="break-words text-sm leading-6 text-secondary">{t("nodeAgents.effect", { agent: draft.agent || "…", node: name, tool: selected?.name || draft.candidate })}</p></>}
            {draft.pending && <p role="status" className="text-xs leading-5 text-tertiary">{t(selected?.configured && !selected.registered ? "nodeAgents.partial" : "nodeAgents.uncertain")}</p>}
            {readError && <p role="alert" className="break-words text-sm text-error-primary">{readError}</p>}
            {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
            {busy && <p role="status" className="text-sm text-tertiary">{t("nodeAgents.registering")}</p>}
            <div className="flex flex-wrap gap-2"><Button size="md" isLoading={busy} isDisabled={loading || !!readError} onClick={() => void register()}>{t(draft.pending ? "nodeAgents.retry" : "nodeAgents.register")}</Button><Button size="md" color="secondary" isDisabled={busy} onClick={() => { setLoading(true); void load(); }}>{t("nodeAgents.refresh")}</Button><Button size="md" color="tertiary" isDisabled={busy} onClick={onClose}>{t("nodeAgents.close")}</Button></div>
        </div>}
    </div></Dialog></Modal></ModalOverlay>;
}
