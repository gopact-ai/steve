import type { Placement, Workspace } from "@/lib/types";

// Words for where a project is. A workspace is a directory on a machine:
// the home is the project's own directory, a copy is one it keeps
// elsewhere, a worktree is one an attempt checked out and will discard.
export const kindWords: Record<string, string> = { canonical: "主目录", copy: "副本", worktree: "工作树" };
export const stateWords: Record<string, string> = { ready: "可用", provisioning: "正在克隆", failed: "克隆失败" };

export function kindWord(kind: string): string { return kindWords[kind] ?? kind; }

// placeLabel is "主目录 · hub" or "副本 · node-a".
export function placeLabel(w: Placement | Workspace): string { return `${kindWord(w.kind)} · ${w.node}`; }
