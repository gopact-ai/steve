import { lazy, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import { Button } from "@/components/base/buttons/button";
import { LazyRegion } from "@/components/steve/lazy-region";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { message } from "@/lib/http";
import { fetchDesktopStatus, type DesktopStatus, type SetupStep } from "@/lib/api/desktop";

const DesktopSetupDialog = lazy(() => import("./desktop-setup-dialog").then((module) => ({ default: module.DesktopSetupDialog })));

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
    if (open && status) return <div className="fixed right-4 bottom-4 z-50 max-w-md rounded-lg shadow-lg ring-1 ring-secondary">
        <LazyRegion resetKey={`${status.node_id}:${entry || "guide"}`} onClose={close}>
            <DesktopSetupDialog key={status.node_id} status={status} entry={entry} onClose={close} onStatus={(next) => { setStatus(next); refresh(); }} />
        </LazyRegion>
    </div>;
    if (requested && (error || status?.enabled === false)) return <div role="status" className="fixed right-4 bottom-4 z-50 flex max-w-md flex-wrap items-center gap-3 rounded-lg bg-primary p-4 text-sm text-secondary shadow-lg ring-1 ring-secondary">
        <p>{error ? `${t("desktop.statusError")} ${error}` : t("desktop.unavailable")}</p>
        {error && <Button size="sm" color="secondary" onClick={() => void load()}>{t("desktop.checkAgain")}</Button>}
        <Button size="sm" color="tertiary" onClick={close}>{t("desktop.close")}</Button>
    </div>;
    return null;
}
