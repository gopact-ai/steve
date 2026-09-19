import type { Project } from "./types";

// The order of the projects themselves is the reader's, not the
// alphabet's: a sidebar is a desk, and people put the thing they open
// every morning at the top. A drag is the whole gesture — there is no
// save — so the order is written the moment it changes.
export const PROJECT_ORDER = "steve.projects.order";

export function storedProjectOrder(): string[] {
    try {
        const raw: unknown = JSON.parse(localStorage.getItem(PROJECT_ORDER) || "[]");
        return Array.isArray(raw) ? raw.filter((id): id is string => typeof id === "string") : [];
    } catch { return []; }
}

// A project the reader has never moved keeps the order it always had —
// the default project first, then by name — and sits after the ones that
// were moved. Sorting is stable, so a new project joins the end without
// disturbing anything the reader arranged.
export function arrangeProjects(projects: Project[], order: readonly string[]): Project[] {
    const natural = [...projects].sort((a, b) => (a.default ? -1 : b.default ? 1 : a.id.localeCompare(b.id)));
    const rank = new Map(order.map((id, index) => [id, index]));
    return natural.sort((a, b) => {
        const left = rank.get(a.id), right = rank.get(b.id);
        if (left === undefined && right === undefined) return 0;
        if (left === undefined) return 1;
        if (right === undefined) return -1;
        return left - right;
    });
}

// moved is the list after one project is dropped onto another, or nudged
// by a step from the keyboard. It is the same operation either way.
export function movedProjects(ids: readonly string[], id: string, to: number): string[] {
    const from = ids.indexOf(id);
    if (from < 0 || to < 0 || to >= ids.length || to === from) return [...ids];
    const next = [...ids];
    next.splice(from, 1);
    next.splice(to, 0, id);
    return next;
}
