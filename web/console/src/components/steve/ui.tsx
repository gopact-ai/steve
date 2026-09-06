import type { ReactNode } from "react";
import { Badge } from "@/components/base/badges/badges";
import type { BadgeColors } from "@/components/base/badges/badge-types";
import type { FC } from "react";
import { useFleet } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import type { MessageKey } from "@/lib/i18n";

// One vocabulary of colours for every state the ledger speaks.
export function colorOf(state: string): BadgeColors {
    switch (state) {
        case "done": case "bound": case "committed": case "verified": case "up": case "ready": case "pass": case "succeeded":
            return "success";
        case "failed": case "down": case "lost": case "expired": case "bind-conflict": case "fail":
        case "merge-conflicted": case "apply-conflicted": case "commit-conflicted": case "cancelled": case "outcome-unknown":
            return "error";
        case "running": case "applying": case "transferring": case "present": case "prepared": case "leased": case "verifying": case "locked": case "merged":
            return "blue";
        case "quarantined": case "paused": case "blocked": case "review": case "recovery-pending": case "proposed":
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
    "commit-conflicted": "status.commitConflict",
    "bind-conflict": "status.bindConflict",
    "outcome-unknown": "status.outcomeUnknown",
    "merged": "status.merged",
    "locked": "status.locked",
    "quarantined": "status.quarantined",
    "proposed": "status.proposed",
    "recovery-pending": "status.recoveryPending"
} as const satisfies Record<string, MessageKey>;

// StateBadge shows a state in the page's words; the internal word is the
// tooltip, for anyone reading logs alongside.
export const StateBadge = ({ state, size = "sm" }: { state: string; size?: "sm" | "md" }) => {
    const { t } = useI18n();
    const key = stateWords[state as keyof typeof stateWords];
    return <span title={state}><Badge type="pill-color" size={size} color={colorOf(state)}>{key ? t(key) : state}</Badge></span>;
};

// The model's empty node is the hub's own machine; name it, never the role.
export const Where = ({ node }: { node?: string }) => {
    const { snap } = useFleet();
    return <span className="font-mono text-xs text-tertiary">{node || snap.hub.node || "—"}</span>;
};

export const Mono = ({ children, className }: { children: ReactNode; className?: string }) => (
    <code className={`font-mono text-xs text-secondary ${className ?? ""}`}>{children}</code>
);

export const Nothing = ({ icon: Icon, title, children }: { icon: FC<{ className?: string }>; title: string; children?: ReactNode }) => (
    <div className="workbench-empty flex min-w-0 flex-col items-center gap-2 px-6 py-10 text-center">
        <span aria-hidden="true" className="mb-1 text-fg-quaternary"><Icon className="size-6" /></span>
        <p className="text-sm font-medium text-secondary">{title}</p>
        {children ? <div className="max-w-sm text-xs leading-relaxed text-tertiary">{children}</div> : null}
    </div>
);

export const Tags = ({ items }: { items?: string[] }) =>
    items && items.length ? (
        <span className="flex flex-wrap gap-1">
            {items.map((t) => (
                <Badge key={t} type="modern" size="sm" color="gray">{t}</Badge>
            ))}
        </span>
    ) : (
        <span className="text-quaternary">—</span>
    );

export const Section = ({ title, description, aside, children }: { title: string; description?: string; aside?: ReactNode; children: ReactNode }) => (
    <section className="flex min-w-0 flex-col gap-4">
        <div className="flex flex-wrap items-end justify-between gap-4">
            <div className="min-w-0">
                <h2 className="text-base font-semibold text-primary">{title}</h2>
                {description && <p className="text-sm text-tertiary">{description}</p>}
            </div>
            {aside}
        </div>
        {children}
    </section>
);

// taskState is what to call a task right now: "running" only while an
// attempt is live; an open task nobody is working on is idle — a chat
// thread waiting for its next line — not 进行中.
export function taskState(t: { lifecycle: string; execution?: string }): string {
    if (t.execution === "unknown" && t.lifecycle !== "done" && t.lifecycle !== "cancelled") return "unknown";
    if (t.lifecycle === "running") return t.execution === "running" ? "running" : "idle";
    return t.lifecycle;
}
