import type { ReactNode } from "react";
import { Badge } from "@/components/base/badges/badges";
import type { BadgeColors } from "@/components/base/badges/badge-types";
import type { FC } from "react";
import { useFleet } from "@/lib/fleet";

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

const stateWords: Record<string, string> = {
    draft: "草稿", running: "进行中", unknown: "状态未知", blocked: "受阻", review: "待审", done: "已完成", failed: "失败", paused: "已暂停", cancelled: "已取消",
    pending: "待执行", dispatching: "投递中", accepted: "已接收", ready: "可执行", verifying: "验证中", "awaiting-human": "等你回答", skipped: "跳过",
    up: "在线", down: "离线", ready_agent: "可用", blocked_agent: "不可用", idle: "空闲",
    bound: "已绑定", committed: "已提交", verified: "已验证", pass: "通过", fail: "未通过", succeeded: "成功",
    leased: "已租", prepared: "已准备", applying: "应用中", transferring: "传输中", present: "在", lost: "丢失", expired: "过期",
    "merge-conflicted": "合并冲突", "apply-conflicted": "应用冲突", "commit-conflicted": "提交冲突", "bind-conflict": "绑定冲突", "outcome-unknown": "结果未知",
    merged: "已合并", locked: "已锁", quarantined: "隔离", proposed: "待定", "recovery-pending": "待恢复",
};

// StateBadge shows a state in the page's words; the internal word is the
// tooltip, for anyone reading logs alongside.
export const StateBadge = ({ state, size = "sm" }: { state: string; size?: "sm" | "md" }) => (
    <span title={state}><Badge type="pill-color" size={size} color={colorOf(state)}>{stateWords[state] ?? state}</Badge></span>
);

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
