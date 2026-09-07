import type { Placement, Workspace } from "@/lib/types";
import { translate, type Locale } from "./i18n.ts";

// Words for where a project is. A workspace is a directory on a machine:
// the home is the project's own directory, a copy is one it keeps
// elsewhere, a worktree is one an attempt checked out and will discard.

export function kindWord(kind: string, locale: Locale = "zh"): string {
    const key = ({ canonical: "workspace.canonical", copy: "workspace.copy", worktree: "workspace.worktree" } as const)[kind as "canonical" | "copy" | "worktree"];
    return key ? translate(locale, key) : kind;
}
export function workspaceState(state: string, locale: Locale): string {
    const key = ({ ready: "status.available", provisioning: "workspace.provisioning", failed: "workspace.failed" } as const)[state as "ready" | "provisioning" | "failed"];
    return key ? translate(locale, key) : state;
}
export function levelName(level: string, locale: Locale): string {
    const key = ({ public: "level.public", internal: "level.internal", restricted: "level.restricted", sealed: "level.sealed" } as const)[level as "public" | "internal" | "restricted" | "sealed"];
    return key ? translate(locale, key) : level;
}

// placeLabel is "主目录 · hub" or "副本 · node-a".
export function placeLabel(w: Placement | Workspace, locale: Locale = "zh"): string { return `${kindWord(w.kind, locale)} · ${w.node}`; }
