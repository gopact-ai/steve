import { AbandonExecutionControl } from "@/components/steve/abandon-execution";
import { useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { ConfirmDialog } from "@/components/steve/confirm";
import type { Attempt } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";

export function needsForceStop(attempt: Attempt) {
    return attempt.unsettled || attempt.force_stop && attempt.force_stop.level !== "confirmed";
}

export function ForceStopControl({ attempt, onForce, onAbandon }: { attempt: Attempt; onAbandon: (id: string, revision: number) => Promise<void>; onForce: (id: string, expectedRevision: number) => Promise<void> }) {
    const { t } = useI18n();
    const [sending, setSending] = useState(false);
    const [confirming, setConfirming] = useState<(Pick<Attempt, "id" | "task_id"> & { revision: number }) | null>(null);
    const level = attempt.force_stop?.level;
    const active = level === "kill" || level === "restart" || level === "await";
    const revision = attempt.force_stop?.revision ?? 0;
    const eligible = !attempt.abandoned && needsForceStop(attempt) && level !== "confirmed" && !active;
    const validConfirmation = confirming && confirming.id === attempt.id && confirming.revision === revision && eligible;
    if (confirming && !validConfirmation) setConfirming(null);
    const progress = level === "restart" ? t(attempt.force_stop?.restart_id ? "console.forceRestarting" : "console.forceRestartDiscovering") : level === "await" ? t("console.forceAwaiting") : t("console.forceRunning");
    const reasons: Record<string, string> = {
        restart_required: t("console.forceRestartRequired"), upgrade_required: t("console.forceUpgradeRequired"),
        stop_running: t("console.forceStillRunning"), stop_unproven: t("console.forceUnproven"),
        stop_unsupported: t("console.forceUnsupported"), rejected: t("console.forceRejected"),
        restart_self: t("console.forceRestartSelf"), restart_permission: t("console.forceRestartPermission"),
        restart_no_holder: t("console.forceRestartNoHolder"), restart_unavailable: t("console.forceRestartNoHolder"),
        restart_timeout: t("console.forceRestartTimeout"), restart_failed: t("console.forceRestartFailed"),
        restart_ambiguous_holder: t("console.forceRestartAmbiguous"), restart_conflicting_plans: t("console.forceRestartConflict"),
        restart_status_lost: t("console.forceRestartLost"), await_timeout: t("console.forceAwaitTimeout"),
        restart_upgrade_required: t("console.forceSafeUpgrade"), restart_stop_unsupported: t("console.forceSafeUnsupported"), restart_identity_unproven: t("console.forceSafeUnproven"),
    };
    async function force() {
        if (sending || !confirming || !validConfirmation) return;
        setSending(true);
        try { await onForce(confirming.id, confirming.revision); }
        finally { setSending(false); }
    }
    return <div className="flex flex-col items-start gap-1">
        {!attempt.abandoned && <Button size="sm" color="secondary" isDisabled={sending || !eligible} onClick={() => setConfirming({ id: attempt.id, task_id: attempt.task_id, revision })}>{t("console.forceStop")}</Button>}
        {!attempt.abandoned && <span role="status" className="text-xs text-tertiary">{sending ? t("console.forceSending") : active ? progress : attempt.force_stop?.level === "exhausted" ? reasons[attempt.force_stop.reason || ""] || t("console.forceUnproven") : t("console.forceHint")}</span>}
        <AbandonExecutionControl attempt={attempt} onAbandon={onAbandon} />
        {confirming && <ConfirmDialog title={t("console.forceConfirmTitle", { task: confirming.task_id || "?" })}
            body={t("console.forceConfirmBody", { task: confirming.task_id || "?", attempt: confirming.id })}
            confirmLabel={t("console.forceConfirm")} onConfirm={force} onClose={() => setConfirming(null)} />}
    </div>;
}

export function ForceStopBanner({ attempts, onForce, onAbandon, retry, uncertain, error }: { attempts: Attempt[]; onAbandon: (id: string, revision: number) => Promise<void>; onForce: (id: string, expectedRevision: number) => Promise<void>; retry: () => void; uncertain: boolean; error?: string }) {
    const { t } = useI18n();
    if (!attempts.length && !uncertain) return null;
    return <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
        <p>{attempts.length ? t("console.forceTitle") : t("console.stopUncertain")}</p>
        {attempts.length > 0 && <p className="mt-1 text-xs text-tertiary">{t("console.forceHint")}</p>}
        {attempts.map((attempt) => <div key={attempt.id} className="mt-2 flex flex-wrap items-start justify-between gap-2 border-t border-secondary pt-2">
            <span className="break-words text-xs">#{attempt.task_id} · {attempt.agent} · {attempt.node}</span><ForceStopControl attempt={attempt} onForce={onForce} onAbandon={onAbandon} />
        </div>)}
        {uncertain && <><button type="button" className="mt-2 underline" onClick={retry}>{t("console.retryStop")}</button>
            {error && <details className="mt-2"><summary className="cursor-pointer text-xs text-tertiary">{t("console.details")}</summary><pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap break-words text-xs [overflow-wrap:anywhere]">{error}</pre></details>}</>}
    </div>;
}
