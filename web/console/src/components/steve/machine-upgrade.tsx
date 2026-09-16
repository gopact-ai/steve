import { useEffect, useRef, useState } from "react";
import { CheckCircle, X, XCircle } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { InstallLog, InstallProgress, SSHSteps } from "@/components/steve/ssh-connect";
import { useI18n } from "@/providers/locale-provider";
import { nodeLabel, nodeLabelIn } from "@/lib/node-name";
import { upgradeSSH, upgradeStatusSSH, type SSHInstallResult } from "@/lib/api/ssh";
import type { Node } from "@/lib/types";

type Outcome = { status: "waiting" | "running" | "done" | "failed"; result?: SSHInstallResult; error?: string };

// MachineUpgrade brings the given machines to this build one after the
// other. The machine being upgraded shows the same phases and record as
// an installation; the rest wait their turn or keep their outcome, and
// any machine with a record can be picked to read it.
export function MachineUpgrade({ nodes, version, onClose, onChanged }: { nodes: Node[]; version: string; onClose: () => void; onChanged: () => void }) {
    const { t } = useI18n();
    const [outcomes, setOutcomes] = useState<Record<string, Outcome>>(() => Object.fromEntries(nodes.map((node) => [node.name, { status: "waiting" }])));
    const [running, setRunning] = useState(false);
    const [shownID, setShownID] = useState<string | null>(null);
    const acting = useRef(false);
    const alive = useRef(true);
    useEffect(() => () => { alive.current = false; }, []);
    const set = (id: string, outcome: Outcome) => { if (alive.current) setOutcomes((all) => ({ ...all, [id]: outcome })); };
    async function upgradeOne(id: string) {
        set(id, { status: "running" }); setShownID(id);
        const following = new AbortController();
        const follow = () => upgradeStatusSSH(id, following.signal).then((live) => { if (!following.signal.aborted && live?.status === "installing") set(id, { status: "running", result: live }); }).catch(() => undefined);
        const timer = window.setInterval(() => void follow(), 1000);
        try {
            const result = await upgradeSSH(id);
            set(id, { status: result.status === "connected" ? "done" : "failed", result });
        } catch (error) {
            set(id, { status: "failed", error: error instanceof Error ? error.message : String(error) });
        } finally { window.clearInterval(timer); following.abort(); }
    }
    async function start(only?: string[]) {
        if (acting.current) return;
        acting.current = true; setRunning(true);
        const targets = nodes.filter((node) => !only || only.includes(node.name));
        for (const node of targets) {
            if (!alive.current) break;
            await upgradeOne(node.name);
        }
        onChanged();
        acting.current = false;
        if (alive.current) setRunning(false);
    }
    const states = Object.values(outcomes);
    const started = states.some((outcome) => outcome.status !== "waiting");
    const finished = started && !running;
    const failed = nodes.filter((node) => outcomes[node.name]?.status === "failed").map((node) => node.name);
    const shown = shownID ? outcomes[shownID] : undefined;
    return <ModalOverlay isOpen isDismissable={!running} isKeyboardDismissDisabled={running} onOpenChange={(open) => { if (!open && !running) onClose(); }} className="motion-reduce:animate-none motion-reduce:duration-0">
        <Modal className="max-w-2xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("fleet.upgradeTitle")} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary">
            <div className="max-h-[min(840px,85dvh)] space-y-4 overflow-y-auto overscroll-contain p-5 sm:p-6">
                <header className="flex items-start gap-3"><div className="min-w-0 flex-1"><h1 className="text-md font-semibold text-primary">{t("fleet.upgradeTitle")}</h1><p className="mt-2 text-sm leading-6 text-tertiary">{t("fleet.upgradeIntro", { version })}</p></div><Button size="sm" color="tertiary" iconLeading={X} aria-label={t("common.close")} isDisabled={running} onClick={onClose} /></header>
                <ul className="divide-y divide-secondary rounded-lg border border-secondary">{nodes.map((node) => {
                    const outcome = outcomes[node.name] ?? { status: "waiting" as const };
                    const readable = outcome.status !== "waiting";
                    return <li key={node.name}>
                        <button type="button" disabled={!readable} aria-pressed={shownID === node.name} onClick={() => setShownID(node.name)} className={`flex w-full min-w-0 flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-left ${readable ? "cursor-pointer hover:bg-secondary" : "cursor-default"} ${shownID === node.name ? "bg-secondary" : ""}`}>
                            <span className="flex min-w-0 flex-1 flex-col"><span className="truncate text-sm font-medium text-primary">{nodeLabel(node)}</span><span className="font-mono text-xs text-tertiary">{node.version || "—"} → {version}</span></span>
                            <span className={`flex items-center gap-1 text-xs ${outcome.status === "done" ? "text-success-primary" : outcome.status === "failed" ? "text-error-primary" : outcome.status === "running" ? "text-primary" : "text-tertiary"}`}>
                                {outcome.status === "done" && <CheckCircle className="size-3.5" aria-hidden="true" />}{outcome.status === "failed" && <XCircle className="size-3.5" aria-hidden="true" />}
                                {t(outcome.status === "done" ? "fleet.upgradeDone" : outcome.status === "failed" ? "fleet.upgradeFailed" : outcome.status === "running" ? "fleet.upgradeRunning" : "fleet.upgradeWaiting")}
                            </span>
                        </button>
                    </li>;
                })}</ul>
                {shown?.result && <section className="space-y-3" aria-live="polite">
                    <h2 className="text-sm font-semibold text-primary">{shownID && nodeLabelIn(nodes, shownID)}</h2>
                    <InstallProgress result={shown.result} />
                    {shown.result.steps.length > 0 && <SSHSteps steps={shown.result.steps} />}
                    <InstallLog result={shown.result} />
                </section>}
                {shown?.error && <p role="alert" className="break-words text-sm text-error-primary">{shown.error}</p>}
                {finished && failed.length === 0 && <p role="status" className="text-sm text-success-primary">{t("fleet.upgradeAllDone", { version })}</p>}
                {finished && failed.length > 0 && <p role="status" className="text-sm leading-6 text-tertiary">{t("fleet.upgradeStoppedHint")}</p>}
                <div className="flex flex-wrap gap-2">
                    {!started && <Button size="md" isDisabled={nodes.length === 0} onClick={() => void start()}>{nodes.length === 1 ? t("fleet.upgradeStartOne") : t("fleet.upgradeStart", { count: nodes.length })}</Button>}
                    {finished && failed.length > 0 && <Button size="md" onClick={() => void start(failed)}>{t("fleet.upgradeRetry")}</Button>}
                    <Button size="md" color={finished && failed.length === 0 ? "primary" : "secondary"} isDisabled={running} onClick={onClose}>{t(finished ? "ssh.done" : "common.cancel")}</Button>
                </div>
            </div>
        </Dialog></Modal>
    </ModalOverlay>;
}
