import { useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { forceStopAttempt } from "@/lib/api/work";
import { useFleet } from "@/lib/fleet";
import type { Attempt } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";

export function needsForceStop(attempt: Attempt) {
    return attempt.unsettled || attempt.force_stop && attempt.force_stop.level !== "confirmed";
}

export function ForceStopControl({ attempt }: { attempt: Attempt }) {
    const { t } = useI18n();
    const refresh = useFleet((fleet) => fleet.refresh);
    const [sending, setSending] = useState(false);
    const [error, setError] = useState("");
    const active = attempt.force_stop?.level === "kill";
    const reasons: Record<string, string> = {
        restart_required: t("console.forceRestartRequired"), upgrade_required: t("console.forceUpgradeRequired"),
        stop_running: t("console.forceStillRunning"), stop_unproven: t("console.forceUnproven"),
        stop_unsupported: t("console.forceUnsupported"), rejected: t("console.forceRejected"),
    };
    async function force() {
        if (sending || active) return;
        setSending(true); setError("");
        try { await forceStopAttempt(attempt.id); refresh(); }
        catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { setSending(false); }
    }
    return <div className="flex flex-col items-start gap-1">
        <Button size="sm" color="secondary" isDisabled={sending || active} onClick={() => void force()}>{t("console.forceStop")}</Button>
        <span role="status" className="text-xs text-tertiary">{sending ? t("console.forceSending") : active ? t("console.forceRunning") : attempt.force_stop?.level === "exhausted" ? reasons[attempt.force_stop.reason || ""] || t("console.forceUnproven") : t("console.forceHint")}</span>
        {error && <span role="alert" className="max-w-sm whitespace-pre-wrap break-words text-xs text-error-primary">{error}</span>}
    </div>;
}

export function ForceStopBanner({ attempts, retry, uncertain, error }: { attempts: Attempt[]; retry: () => void; uncertain: boolean; error?: string }) {
    const { t } = useI18n();
    if (!attempts.length && !uncertain) return null;
    return <div role="alert" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
        <p>{attempts.length ? t("console.forceTitle") : t("console.stopUncertain")}</p>
        {attempts.length > 0 && <p className="mt-1 text-xs text-tertiary">{t("console.forceHint")}</p>}
        {attempts.map((attempt) => <div key={attempt.id} className="mt-2 flex flex-wrap items-start justify-between gap-2 border-t border-secondary pt-2">
            <span className="break-words text-xs">#{attempt.task_id} · {attempt.agent} · {attempt.node}</span><ForceStopControl attempt={attempt} />
        </div>)}
        {uncertain && <><button type="button" className="mt-2 underline" onClick={retry}>{t("console.retryStop")}</button>
            {error && <details className="mt-2"><summary className="cursor-pointer text-xs text-tertiary">{t("console.details")}</summary><pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap break-words text-xs [overflow-wrap:anywhere]">{error}</pre></details>}</>}
    </div>;
}
