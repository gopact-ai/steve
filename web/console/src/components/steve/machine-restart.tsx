import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { InstallLog, InstallProgress, SSHSteps } from "@/components/steve/ssh-connect";
import { useI18n } from "@/providers/locale-provider";
import { restartSSH, restartStatusSSH, type SSHInstallResult, type SSHRestartState } from "@/lib/api/ssh";
import { HTTPError, message } from "@/lib/http";
import { relative, when } from "@/lib/format";
import { restartLine, type HistoryTone } from "@/lib/history-lines";
import { autoStartLine, restartConfirms, restartHiddenBy, restartOffered, restartPollDelay, restartRunning, restartShown, type AutoStartTone } from "@/lib/machine-restart";
import { nodeLabel, useNodeLabel } from "@/lib/node-name";
import type { Node } from "@/lib/types";

const toneText: Record<AutoStartTone | HistoryTone, string> = {
    quiet: "text-tertiary", info: "text-secondary", good: "text-success-primary", warn: "text-warning-primary", bad: "text-error-primary",
};

// MachineRestart restarts a machine's node process over SSH from its
// drawer, where the node serving the console restarts that machine: an
// online machine asks first, since its running executions are interrupted
// and confirmed through the normal stop; an offline one restarts at once.
// It follows a running restart, manual or automatic, and says how
// automatic start stands and what the machine's last restart did.
export function MachineRestart({ n, onChanged }: { n: Node; onChanged: () => void }) {
    const { t, locale } = useI18n();
    const nodeName = useNodeLabel();
    const [state, setState] = useState<SSHRestartState | null>(null);
    const [posting, setPosting] = useState(false);
    const [confirming, setConfirming] = useState(false);
    const [result, setResult] = useState<SSHInstallResult | null>(null);
    const [error, setError] = useState("");
    const latest = useRef<SSHRestartState | null>(null);
    const alive = useRef(true);
    useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
    const node = n.name, coordinator = n.role === "hub";
    // The status is read at once, then every few seconds, every second
    // while a restart runs; not while the page is hidden, and no more once
    // the node said it does not restart this machine.
    useEffect(() => {
        if (coordinator) return;
        let stopped = false, refused = false, timer = 0;
        let reading: AbortController | undefined;
        const schedule = () => { window.clearTimeout(timer); timer = window.setTimeout(() => void read(), restartPollDelay(latest.current, posting)); };
        async function read() {
            window.clearTimeout(timer);
            if (stopped || refused || document.hidden) return;
            reading?.abort();
            const current = reading = new AbortController();
            try {
                const next = await restartStatusSSH(node, current.signal);
                if (stopped || current.signal.aborted) return;
                latest.current = next; setState(next);
            } catch (failure) {
                if (stopped || current.signal.aborted) return;
                if (failure instanceof HTTPError && restartHiddenBy(failure.status)) {
                    refused = true; latest.current = null; setState(null);
                    return;
                }
            }
            schedule();
        }
        const shown = () => { if (!document.hidden) void read(); };
        document.addEventListener("visibilitychange", shown);
        void read();
        return () => { stopped = true; window.clearTimeout(timer); reading?.abort(); document.removeEventListener("visibilitychange", shown); };
    }, [node, coordinator, posting]);
    if (!restartOffered(n.role, state)) return null;
    async function restart() {
        setConfirming(false); setError(""); setResult(null); setPosting(true);
        try {
            const done = await restartSSH(node);
            if (alive.current) setResult(done);
        } catch (failure) {
            if (alive.current) setError(message(failure));
        } finally {
            if (alive.current) setPosting(false);
            onChanged();
        }
    }
    const running = restartRunning(state);
    const { restart: shown, automatic } = restartShown(state, result, posting);
    const auto = autoStartLine(state?.auto_start, t, (at) => when(at, locale));
    const last = n.last_restart && restartLine(n.last_restart, nodeLabel(n), t, nodeName);
    return <section aria-label={t("fleet.nodeProcess")} className="space-y-3 rounded-lg border border-secondary p-3">
        <div className="flex min-w-0 flex-wrap items-center gap-3">
            <div className="min-w-0 flex-1">
                <h3 className="text-sm font-medium text-primary">{t("fleet.nodeProcess")}</h3>
                <p className="text-xs text-tertiary">{t("fleet.restartHint")}</p>
            </div>
            {confirming
                ? <><Button size="sm" color="secondary" onClick={() => setConfirming(false)}>{t("common.cancel")}</Button><Button size="sm" color="primary-destructive" onClick={() => void restart()}>{t("fleet.confirmRestart")}</Button></>
                : <Button size="sm" color="secondary" isLoading={posting} showTextWhileLoading isDisabled={posting || running} onClick={() => restartConfirms(n.up) ? setConfirming(true) : void restart()}>{t("fleet.restartNode")}</Button>}
        </div>
        {confirming && <p className="text-xs leading-5 text-warning-primary">{t("fleet.restartConfirmHint")}</p>}
        {error && <p role="alert" className="break-words text-xs text-error-primary">{error}</p>}
        {auto && <div className="space-y-0.5 text-xs">
            <p><span className="font-medium text-secondary">{t("fleet.autoStart")}</span> <span className={toneText[auto.tone]}>{auto.title}</span></p>
            {auto.detail && <p className="break-words text-tertiary">{auto.detail}</p>}
            {auto.hint && <p className="text-tertiary">{auto.hint}</p>}
        </div>}
        {last && <div className="space-y-0.5 text-xs">
            <p><span className="font-medium text-secondary">{t("fleet.lastRestart")}</span> <span className={toneText[last.tone]}>{last.title}</span></p>
            <p className="text-tertiary">{[relative(n.last_restart!.at, locale), ...last.facts].join(" · ")}</p>
            {last.note && <p className="break-words text-tertiary">{last.note}</p>}
        </div>}
        {shown && <div className="space-y-3" aria-live="polite">
            <h4 className="text-xs font-medium text-secondary">{t(automatic ? "fleet.autoStartProgress" : "fleet.restartProgress")}</h4>
            <InstallProgress result={shown} />
            {shown.steps.length > 0 && <SSHSteps steps={shown.steps} />}
            <InstallLog result={shown} />
        </div>}
    </section>;
}
