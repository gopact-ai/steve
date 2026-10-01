import { useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { ConfirmDialog } from "@/components/steve/confirm";
import type { Attempt } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";

export function AbandonExecutionControl({ attempt, onAbandon }: { attempt: Attempt; onAbandon: (id: string, revision: number) => Promise<void> }) {
    const { t } = useI18n();
    const [sending, setSending] = useState(false);
    const [confirmation, setConfirmation] = useState<{ id: string; task: string; revision: number } | null>(null);
    const revision = attempt.force_stop?.revision ?? 0;
    const eligible = !!attempt.unsettled && !attempt.abandoned && attempt.force_stop?.level === "exhausted";
    const valid = confirmation && eligible && confirmation.id === attempt.id && confirmation.revision === revision;
    if (confirmation && !valid) setConfirmation(null);
    async function submit() {
        if (!confirmation || !valid || sending) return;
        setSending(true);
        try { await onAbandon(confirmation.id, confirmation.revision); }
        finally { setSending(false); }
    }
    if (attempt.abandoned) return <span role="status" className="text-xs text-warning-primary">{t(attempt.abandoned.projected_at ? "console.abandoned" : "console.abandonPending")}</span>;
    if (!eligible) return null;
    return <>
        <Button size="sm" color="secondary-destructive" isDisabled={sending} onClick={() => setConfirmation({ id: attempt.id, task: attempt.task_id || "?", revision })}>{t("console.abandon")}</Button>
        {confirmation && <ConfirmDialog title={t("console.abandonTitle", { task: confirmation.task })}
            body={t("console.abandonBody", { attempt: confirmation.id })} confirmLabel={t("console.abandonConfirm")}
            onConfirm={submit} onClose={() => setConfirmation(null)} />}
    </>;
}
