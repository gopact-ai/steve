import { useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { Toggle } from "@/components/base/toggle/toggle";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { useI18n } from "@/providers/locale-provider";
import { useCoordination } from "@/lib/coordination";
import { dateTime } from "@/lib/format";
import type { CoordinationNode, CoordinationEvent } from "@/lib/api/coordination";

export function CoordinationPanel() {
    const { t, locale } = useI18n();
    const { view, pending, history, error, notice, busy, refresh, retry, run, acknowledgeRejection } = useCoordination();
    const [transferring, setTransferring] = useState(false);
    if (!view?.enabled) return pending ? <section aria-label={t("coord.title")} className="space-y-2 rounded-lg bg-primary p-4 ring-1 ring-secondary"><p role="status" className="text-sm text-secondary">{t("coord.unconfirmed")}</p><p className="break-all font-mono text-xs text-tertiary">{pending.body.command_id}</p>{error && <p className="break-words text-sm text-error-primary">{error}</p>}<Button size="sm" color="secondary" onClick={() => void refresh()}>{t("coord.refresh")}</Button></section> : null;
    const name = (id?: string) => view.nodes.find((node) => node.id === id)?.name || id || t("common.unknown");
    const disabled = busy || !!pending || !!error || !view.authoritative;
    const voters = view.nodes.filter((node) => node.voter);
    const canEnable = voters.length >= 3 && voters.filter((node) => node.auto_eligible).length >= 2;
    const targets = view.nodes.filter((node) => node.id !== view.coordinator_id);
    const canTransfer = targets.some((node) => node.voter && node.online && node.ready);
    const showReadiness = view.auto_failover || !view.authoritative;
    const sameCluster = !pending || pending.cluster_id === view.cluster_id;
    const eventLabel = (event: CoordinationEvent) => {
        const messages = { coordinator_initialized: "coord.initialized", coordinator_transferred: "coord.transferred", automatic_failover_enabled: "coord.policyEnabled", automatic_failover_disabled: "coord.policyDisabled", automatic_eligibility_granted: "coord.eligibilityGranted", automatic_eligibility_removed: "coord.eligibilityRemoved", member_joined: "coord.memberJoined", member_removed: "coord.memberRemoved" } as const;
        return t(messages[event.kind as keyof typeof messages] || "coord.changed");
    };
    return <section aria-label={t("coord.title")} className="min-w-0 space-y-3 rounded-lg bg-primary p-4 ring-1 ring-secondary sm:p-5">
        <header className="flex flex-wrap items-start justify-between gap-3"><div className="min-w-0"><h2 className="text-base font-semibold text-primary">{t("coord.title")}</h2><p className="mt-1 break-all text-sm font-semibold text-primary">{t("connection.coordinatedBy", { node: name(view.coordinator_id) })}</p></div><Button size="md" color="secondary" isDisabled={disabled || !canTransfer} onClick={() => setTransferring(true)}>{t("coord.transfer")}</Button></header>
        {view.nodes.length < 2 && <p className="text-xs text-tertiary">{t("coord.oneNode")}</p>}
        {!view.authoritative && <p role="status" className="text-sm text-error-primary">{t("coord.noAuthority")}</p>}
        <div className="space-y-1 text-xs">
            <p className="font-medium text-secondary">{t(view.auto_failover ? "coord.enabled" : "coord.disabled")}</p>
            {showReadiness && <><p role="status" className={view.ready ? "text-secondary" : "text-error-primary"}>{t(view.ready ? "coord.ready" : "coord.notReady")}</p>{view.reason && <p className="break-words leading-5 text-tertiary">{view.reason}</p>}</>}
        </div>
        <details className="border-t border-secondary pt-2">
            <summary className="min-h-10 cursor-pointer content-center text-sm font-medium text-secondary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("coord.settings")}</summary>
            <div className="space-y-4 pt-2"><p className="text-xs leading-5 text-tertiary">{t("coord.description")}</p>
                <div className="space-y-3 rounded-lg bg-secondary p-4">
                    <Toggle size="sm" aria-label={t("coord.policy")} label={t("coord.policy")} isSelected={view.auto_failover} isDisabled={disabled || (!view.auto_failover && !canEnable)} onChange={(enabled) => void run({ kind: "policy", body: { command_id: `policy-${crypto.randomUUID()}`, expected_revision: view.revision, enabled } })} className="min-h-10 max-w-full items-center" />
                    <p className="text-xs leading-5 text-tertiary">{t("coord.policyHint")}</p>
                    {!showReadiness && view.reason && <p className="break-words text-xs leading-5 text-tertiary">{view.reason}</p>}
                    {!canEnable && <p className="text-xs leading-5 text-tertiary">{t("coord.minimum")}</p>}
                </div>
                <div className="space-y-3"><h3 className="text-sm font-semibold text-primary">{t("coord.eligibility")}</h3><p className="text-xs leading-5 text-tertiary">{t("coord.eligibilityHint")}</p>
                    <ul className="divide-y divide-secondary">{view.nodes.map((node) => <li key={node.id} className="flex min-w-0 items-center justify-between gap-3 py-3"><div className="min-w-0"><p className="break-all text-sm font-medium text-primary">{node.name || node.id}{node.local && <span className="ml-2 text-xs font-normal text-tertiary">{t("coord.local")}</span>}</p>{(!node.voter || !node.online || !node.ready || node.reason) && <p className="mt-1 break-words text-xs text-tertiary">{node.reason || t(!node.voter ? "coord.nonVoter" : !node.online ? "coord.offline" : "coord.nodeNotReady")}</p>}</div><Toggle size="sm" className="min-h-10 items-center" aria-label={t("coord.eligibilityNode", { node: node.name || node.id })} isSelected={node.auto_eligible} isDisabled={disabled || !node.voter} onChange={(eligible) => void run({ kind: "eligibility", body: { command_id: `eligibility-${crypto.randomUUID()}`, expected_revision: view.revision, node_id: node.id, eligible } })} /></li>)}</ul>
                </div>
            </div>
        </details>
        {pending && <div className="space-y-2 rounded-lg bg-secondary p-3"><p role="status" className="text-sm font-medium text-primary">{t(busy ? "coord.operationPending" : pending.state === "rejected" ? "coord.rejected" : "coord.unconfirmed")}</p><p className="break-all font-mono text-xs text-tertiary">{t("coord.command")}: {pending.body.command_id}</p>{pending.error && <p className="break-words text-sm text-error-primary">{pending.error}</p>}{!sameCluster && <p className="text-sm text-error-primary">{t("coord.wrongCluster")}</p>}{!busy && (pending.state === "rejected" ? <Button size="sm" color="secondary" onClick={acknowledgeRejection}>{t("coord.reviewAgain")}</Button> : <Button size="sm" isDisabled={!sameCluster} onClick={() => void retry()}>{t("coord.retry")}</Button>)}</div>}
        {notice && !pending && <p role="status" className="text-sm text-secondary">{notice}</p>}
        {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        <div className="flex flex-wrap items-center justify-between gap-2"><span className="text-xs text-quaternary">{t("coord.observed", { time: dateTime(view.observed_at, locale, { dateStyle: "short", timeStyle: "short" }) })}</span><Button size="sm" color="tertiary" isDisabled={busy} onClick={() => void refresh()}>{t("coord.refresh")}</Button></div>
        <details className="border-t border-secondary pt-2"><summary className="min-h-10 cursor-pointer content-center text-sm font-medium text-secondary focus-visible:outline-2 focus-visible:outline-focus-ring">{t("coord.history")}</summary><ul className="mt-3 space-y-3">{view.events.map((event) => <li key={event.id} className="min-w-0 space-y-1"><p className="text-sm text-secondary">{eventLabel(event)}{event.from || event.to ? <span className="ml-2 break-words">{event.from && event.to ? `${name(event.from)} → ${name(event.to)}` : name(event.from || event.to)}</span> : null}</p>{event.reason && <p className="break-words text-xs text-tertiary">{event.reason}</p>}<p className="text-xs text-quaternary">{dateTime(event.at, locale, { dateStyle: "short", timeStyle: "short" })} · {t("coord.actor", { actor: event.actor })}</p></li>)}</ul>{!view.events.length && <p className="mt-3 text-xs text-tertiary">{t("coord.noHistory")}</p>}</details>
        {history.length > 0 && <details className="border-t border-secondary pt-3"><summary className="text-xs text-tertiary">{t("coord.localRecords")}</summary><ul className="mt-3 space-y-3">{history.toReversed().map((operation) => <li key={operation.body.command_id} className="space-y-1"><p className="break-all font-mono text-xs text-secondary">{operation.body.command_id}</p><p className="break-words text-xs text-tertiary">{operation.error}</p></li>)}</ul></details>}
        {transferring && <TransferDialog nodes={targets} from={name(view.coordinator_id)} disabled={disabled} busy={busy} onClose={() => setTransferring(false)} onTransfer={async (target_node_id) => { const done = await run({ kind: "transfer", body: { command_id: `transfer-${crypto.randomUUID()}`, expected_epoch: view.epoch, target_node_id } }); setTransferring(false); return done; }} />}
    </section>;
}

function TransferDialog({ nodes, from, disabled, busy, onClose, onTransfer }: { nodes: CoordinationNode[]; from: string; disabled: boolean; busy: boolean; onClose: () => void; onTransfer: (id: string) => Promise<boolean> }) {
    const { t } = useI18n();
    const [target, setTarget] = useState("");
    const [error, setError] = useState("");
    const radios = useRef<HTMLDivElement>(null);
    const selected = nodes.find((node) => node.id === target && node.online && node.ready && node.voter);
    return <ModalOverlay isOpen isDismissable={!busy} isKeyboardDismissDisabled={busy} onOpenChange={(open) => { if (!open && !busy) onClose(); }} className="motion-reduce:animate-none motion-reduce:duration-0"><Modal className="max-w-lg motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("coord.transferTitle")} className="block rounded-xl bg-primary p-5 ring-1 ring-secondary sm:p-6"><h2 className="text-lg font-semibold text-primary">{t("coord.transferTitle")}</h2><p className="mt-2 text-sm leading-6 text-tertiary">{t("coord.transferHint")}</p><RadioGroup ref={radios} aria-label={t("coord.target")} value={target} onChange={(value) => { setTarget(value); setError(""); }} isDisabled={disabled} className="mt-4 max-h-[40dvh] space-y-2 overflow-y-auto">{nodes.map((node) => <Radio key={node.id} value={node.id} isDisabled={!node.online || !node.ready || !node.voter} className="group flex cursor-pointer items-start gap-2 rounded-lg border border-secondary p-3 data-selected:bg-secondary data-disabled:opacity-60 data-focus-visible:outline-2 data-focus-visible:outline-focus-ring"><span className="mt-1 size-3.5 shrink-0 rounded-full border border-primary group-data-selected:border-4 group-data-selected:border-brand" aria-hidden="true" /><span className="min-w-0"><span className="break-all text-sm font-medium text-primary">{node.name || node.id}</span>{(!node.online || !node.ready || !node.voter || node.reason) && <span className="mt-1 block break-words text-xs text-tertiary">{node.reason || t(!node.voter ? "coord.nonVoter" : !node.online ? "coord.offline" : "coord.nodeNotReady")}</span>}</span></Radio>)}</RadioGroup>{selected && <p className="mt-4 text-sm text-secondary">{t("coord.transferEffect", { from, to: selected.name || selected.id })}</p>}{error && <p role="alert" className="mt-3 text-sm text-error-primary">{error}</p>}<div className="mt-5 flex flex-wrap gap-2"><Button size="md" isDisabled={disabled} isLoading={busy} onClick={() => { if (!selected) { setError(t("coord.targetRequired")); radios.current?.querySelector<HTMLInputElement>('input[type="radio"]:not(:disabled)')?.focus(); return; } void onTransfer(selected.id); }}>{t("coord.transfer")}</Button><Button size="md" color="secondary" isDisabled={busy} onClick={onClose}>{t("common.cancel")}</Button></div></Dialog></Modal></ModalOverlay>;
}
