import type { ReactNode } from "react";
import { Badge } from "@/components/base/badges/badges";
import type { BadgeColors } from "@/components/base/badges/badge-types";
import { EmptyState } from "@/components/application/empty-state/empty-state";
import type { FC } from "react";

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

export const StateBadge = ({ state, size = "sm" }: { state: string; size?: "sm" | "md" }) => (
    <Badge type="pill-color" size={size} color={colorOf(state)}>{state}</Badge>
);

export const Where = ({ node }: { node?: string }) => (
    <span className="font-mono text-xs text-tertiary">{node || "hub"}</span>
);

export const Mono = ({ children, className }: { children: ReactNode; className?: string }) => (
    <code className={`font-mono text-xs text-secondary ${className ?? ""}`}>{children}</code>
);

export const Nothing = ({ icon, title, children }: { icon: FC<{ className?: string }>; title: string; children?: ReactNode }) => (
    <EmptyState size="sm" className="py-10">
        <EmptyState.Header>
            <EmptyState.FeaturedIcon icon={icon} color="gray" theme="modern" />
        </EmptyState.Header>
        <EmptyState.Content>
            <EmptyState.Title>{title}</EmptyState.Title>
            {children ? <EmptyState.Description>{children}</EmptyState.Description> : null}
        </EmptyState.Content>
    </EmptyState>
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
    <section className="flex flex-col gap-4">
        <div className="flex items-end justify-between gap-4">
            <div>
                <h2 className="text-lg font-semibold text-primary">{title}</h2>
                {description && <p className="text-sm text-tertiary">{description}</p>}
            </div>
            {aside}
        </div>
        {children}
    </section>
);
