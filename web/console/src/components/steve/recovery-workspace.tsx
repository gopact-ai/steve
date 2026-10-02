import { useI18n } from "@/providers/locale-provider";
import type { RecoveryWorkspace } from "@/lib/types";

export function RecoveryWorkspaceNotice({ recovery }: { recovery: RecoveryWorkspace }) {
    const { t } = useI18n();
    const released = recovery.phase === "released";
    const status = recovery.phase === "draining" ? "console.recoveryWorkspaceDraining" : recovery.phase === "capture" ? "console.recoveryWorkspaceCapture" : recovery.phase === "landing" ? "console.recoveryWorkspaceLanding" : released ? recovery.cleanup_pending ? "console.recoveryWorkspaceCleanup" : "console.recoveryWorkspaceReleased" : recovery.phase === "ready" ? "console.recoveryWorkspaceReady" : recovery.phase === "working" ? "console.recoveryWorkspaceWorking" : recovery.phase === "materializing" ? "console.recoveryWorkspaceMaterializing" : "console.recoveryWorkspacePreparing";
    return <div role="status" className="mx-auto mb-2 max-w-3xl rounded-lg bg-warning-primary p-3 text-sm text-secondary">
        <p className="font-semibold">{t(released ? "console.recoveryWorkspaceReleasedTitle" : "console.recoveryWorkspaceTitle")}</p>
        <p>{t(status)}</p>
        {recovery.error && <p className="mt-1 break-words text-xs text-tertiary">{recovery.error}</p>}
        {recovery.path && <p className="mt-1 text-xs text-tertiary">{t("console.recoveryWorkspaceNode", { node: recovery.node || t("console.recoveryWorkspaceHub") })}</p>}
        {recovery.path && <p className="mt-1 break-all font-mono text-xs text-tertiary">{recovery.path}</p>}
    </div>;
}
