import type { ReactNode } from "react";
import { Badge } from "@/components/base/badges/badges";
import type { BadgeColors } from "@/components/base/badges/badge-types";
import type { FC } from "react";
import { useFleet } from "@/lib/fleet";
import { nodeLabelIn } from "@/lib/node-name";
import { useI18n } from "@/providers/locale-provider";
import { colorOf, stateWordIn } from "@/lib/states";

export { colorOf, stateWordIn } from "@/lib/states";

// useStateWord is stateWordIn bound to the page's language.
export function useStateWord(): (state: string) => string {
    const { t } = useI18n();
    return (state: string) => stateWordIn(state, t);
}

// StateBadge shows a state in the page's words; the internal word is the
// tooltip, for anyone reading logs alongside.
export const StateBadge = ({ state, size = "sm" }: { state: string; size?: "sm" | "md" }) => {
    const { t } = useI18n();
    return <span title={state}><Badge type="pill-color" size={size} color={colorOf(state) as BadgeColors}>{stateWordIn(state, t)}</Badge></span>;
};

// The model's empty node is the hub's own machine; name it, never the role.
export const Where = ({ node }: { node?: string }) => {
    const snap = useFleet((fleet) => fleet.snap);
    const id = node || snap.hub.node;
    return <span className="font-mono text-xs text-tertiary" title={id || undefined}>{id ? nodeLabelIn(snap.nodes, id) : "—"}</span>;
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

// taskState is what to call a task right now: "running" only while an
// attempt is live; an open task nobody is working on is idle — a chat
// thread waiting for its next line — not 进行中.
export function taskState(t: { lifecycle: string; execution?: string }): string {
    if (t.execution === "unknown" && t.lifecycle !== "done" && t.lifecycle !== "cancelled") return "unknown";
    if (t.lifecycle === "running") return t.execution === "running" ? "running" : "idle";
    return t.lifecycle;
}
