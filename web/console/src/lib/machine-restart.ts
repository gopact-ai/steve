import type { Translator } from "./i18n";
import type { SSHAutoStart, SSHInstallResult, SSHRestartState } from "./api/ssh";

// A machine's node process is restarted over SSH by the node serving the
// console, the one that holds the machine's link. These are the rules the
// fleet drawer follows for it, apart from how they are drawn.

export type AutoStartTone = "quiet" | "info" | "warn" | "bad";
export interface AutoStartLine { tone: AutoStartTone; title: string; detail?: string; hint?: string }

/**
 * restartOffered is whether the drawer offers to restart a machine: never
 * the coordinator, which restarts with its application rather than over
 * SSH, and otherwise only once the node serving the console answered for
 * the machine (state is null until then, and after it refused).
 */
export const restartOffered = (role: string | undefined, state: SSHRestartState | null | undefined) => role !== "hub" && !!state;

/** restartConfirms is whether a restart is confirmed first: an online machine has executions to interrupt. */
export const restartConfirms = (up: boolean) => up;

/**
 * restartHiddenBy is whether a failed status read means the machine is not
 * restarted from here: the node refuses it (4xx) or has no SSH service
 * (501). Anything else passes, and what was shown stays.
 */
export const restartHiddenBy = (status: number) => status === 501 || (status >= 400 && status < 500);

/** restartRunning is whether the machine's latest restart is still running. */
export const restartRunning = (state: SSHRestartState | null | undefined) => state?.restart?.status === "installing";

/**
 * restartShown is the restart the drawer shows, and whether it runs
 * automatically: the machine's restart while it runs; nothing while one
 * asked for here has not answered; then the one asked for here, until a
 * later restart ran; and otherwise one that did not bring the machine back,
 * whose record matters. A restart that brought it back says so in the
 * machine's last restart.
 */
export function restartShown(state: SSHRestartState | null | undefined, asked: SSHInstallResult | null, posting: boolean): { restart?: SSHInstallResult; automatic: boolean } {
    const latest = state?.restart;
    if (latest?.status === "installing") return { restart: latest, automatic: !!state?.automatic };
    if (posting) return { automatic: false };
    if (asked && (!latest || latest.plan_id === asked.plan_id)) return { restart: asked, automatic: false };
    if (latest?.status === "needs_attention") return { restart: latest, automatic: !!state?.automatic };
    return { automatic: false };
}

/**
 * restartPollDelay is how long until the machine's restart status is read
 * again: every second while a restart runs, or was just asked for here and
 * may not show yet, and every few seconds otherwise.
 */
export const restartPollDelay = (state: SSHRestartState | null | undefined, posting: boolean) =>
    posting || restartRunning(state) || state?.auto_start?.state === "attempting" ? 1000 : 5000;

/**
 * autoStartLine says how automatic start stands for a machine: what it
 * watches for, what it is doing, how many starts it made since the machine
 * last stayed online, and, once it stopped, why and how it resumes. A
 * running node process is never ended automatically; the line points at
 * the manual restart instead. `time` formats an instant; nothing is said
 * where the node serving the console does not watch the machine.
 */
export function autoStartLine(auto: SSHAutoStart | undefined, tr: Translator, time: (at: string) => string): AutoStartLine | null {
    if (!auto) return null;
    const { attempts, limit } = auto;
    const detail = auto.last_error ? { detail: auto.last_error } : {};
    switch (auto.state) {
        case "watching":
            return attempts > 0
                ? { tone: "info", title: tr("fleet.autoStartStarted", { attempts, limit }) }
                : { tone: "quiet", title: tr("fleet.autoStartWatching") };
        case "waiting":
            return { tone: "warn", title: tr("fleet.autoStartWaiting", { since: auto.offline_since ? time(auto.offline_since) : "—" }) };
        case "unreachable":
            return { tone: "warn", title: tr("fleet.autoStartUnreachable"), ...detail };
        case "attempting":
            return { tone: "info", title: tr("fleet.autoStartAttempting", { attempt: attempts + 1, limit }) };
        case "retrying":
            return {
                tone: "warn", title: tr("fleet.autoStartRetrying", { next: auto.next_at ? time(auto.next_at) : "—" }), ...detail,
                ...(attempts > 0 ? { hint: tr("fleet.autoStartFailures", { attempts, limit }) } : {}),
            };
        case "peer_running":
            return { tone: "warn", title: tr("fleet.autoStartPeerRunning"), hint: tr("fleet.autoStartPeerRunningHint") };
        case "stopped":
            return { tone: "bad", title: tr("fleet.autoStartStopped"), ...detail, hint: tr("fleet.autoStartStoppedHint") };
        default:
            return { tone: "quiet", title: auto.state };
    }
}
