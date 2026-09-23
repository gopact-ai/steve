import type { MessageKey, Translator } from "./i18n";

// The colours a state may wear. Matches the badge palette by name.
export type StateColor = "success" | "error" | "blue" | "warning" | "gray";

// One vocabulary of colours for every state the ledger speaks.
export function colorOf(state: string): StateColor {
    switch (state) {
        case "done": case "bound": case "committed": case "verified": case "up": case "ready": case "pass": case "succeeded":
        case "approved": case "completed": case "published": case "durable":
            return "success";
        case "failed": case "down": case "lost": case "expired": case "bind-conflict": case "fail":
        case "merge-conflicted": case "apply-conflicted": case "cancelled": case "outcome-unknown":
            return "error";
        case "running": case "applying": case "transferring": case "present": case "prepared": case "leased": case "verifying": case "locked": case "merged":
        case "executing": case "landing": case "snapshotted": case "claimed":
            return "blue";
        case "quarantined": case "paused": case "blocked": case "review": case "recovery-pending": case "proposed":
        case "interrupted": case "denied": case "bind-ready":
            return "warning";
        default:
            return "gray";
    }
}

const stateWords = {
    "draft": "status.draft",
    "running": "status.inProgress",
    "unknown": "status.unknown",
    "blocked": "status.blocked",
    "review": "status.review",
    "done": "status.done",
    "failed": "status.failed",
    "paused": "status.paused",
    "cancelled": "status.cancelled",
    "pending": "status.pending",
    "dispatching": "status.dispatching",
    "accepted": "status.accepted",
    "ready": "status.ready",
    "verifying": "status.verifying",
    "awaiting-human": "status.awaitingHuman",
    "skipped": "status.skipped",
    "up": "status.up",
    "down": "status.down",
    "ready_agent": "status.available",
    "blocked_agent": "status.unavailable",
    "idle": "status.idle",
    "bound": "status.bound",
    "committed": "status.committed",
    "verified": "status.verified",
    "pass": "status.pass",
    "fail": "status.fail",
    "succeeded": "status.succeeded",
    "leased": "status.leased",
    "prepared": "status.prepared",
    "applying": "status.applying",
    "transferring": "status.transferring",
    "present": "status.present",
    "lost": "status.lost",
    "expired": "status.expired",
    "merge-conflicted": "status.mergeConflict",
    "apply-conflicted": "status.applyConflict",
    "bind-conflict": "status.bindConflict",
    "outcome-unknown": "status.outcomeUnknown",
    "merged": "status.merged",
    "locked": "status.locked",
    "quarantined": "status.quarantined",
    "proposed": "status.proposed",
    "recovery-pending": "status.recoveryPending",
    "snapshotted": "status.snapshotted",
    "published": "status.published",
    "durable": "status.durable",
    "bind-ready": "status.bindReady",
    "superseded": "status.superseded",
    "claimed": "status.claimed",
    "approved": "status.approved",
    "denied": "status.denied",
    "interrupted": "status.interrupted",
    "executing": "status.running",
    "landing": "status.landing",
    "completed": "status.done"
} as const satisfies Record<string, MessageKey>;

// stateWordIn turns a ledger state into the page's word for it, and
// leaves an unknown token alone so nothing is silently dropped.
export const stateWordIn = (state: string, t: Translator) => {
    const key = stateWords[state as keyof typeof stateWords];
    return key ? t(key) : state;
};
