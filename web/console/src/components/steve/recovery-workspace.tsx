import { useI18n } from "@/providers/locale-provider";
import type { RecoveryWorkspace } from "@/lib/types";

export function RecoveryWorkspaceNotice({ recovery }: { recovery: RecoveryWorkspace }) {
    const { t } = useI18n();
    return <div role="status" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
        <p className="font-semibold">{t("console.recoveryWorkspaceTitle")}</p>
        <p>{t(recovery.phase === "ready" ? "console.recoveryWorkspaceReady" : recovery.phase === "working" ? "console.recoveryWorkspaceWorking" : "console.recoveryWorkspacePreparing")}</p>
        {recovery.path && <p className="mt-1 break-all font-mono text-xs text-tertiary">{recovery.path}</p>}
    </div>;
}
