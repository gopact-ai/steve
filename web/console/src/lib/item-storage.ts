// Keeps a record in localStorage as one key per entry instead of one shared
// JSON value. Windows in separate renderer processes see each other's writes
// only after the browser propagates them, and a Web Lock orders their saves
// but not that propagation: a read-modify-write of a shared value can start
// from a stale copy and drop another window's edit. Writing only the entries
// that changed means edits to different entries never overwrite each other,
// and the last write to an entry wins.

export type ItemChange = readonly [key: string, value: string | null];

export function readItems(prefix: string): [string, string][] {
    const items: [string, string][] = [];
    for (let index = 0; index < localStorage.length; index++) {
        const key = localStorage.key(index);
        if (!key?.startsWith(prefix)) continue;
        const value = localStorage.getItem(key);
        if (value !== null) items.push([key.slice(prefix.length), value]);
    }
    return items;
}

// A null value means the entry is absent and its key is removed.
export function itemChanges(prefix: string, before: Record<string, string | null>, after: Record<string, string | null>): ItemChange[] {
    const changes: ItemChange[] = [];
    for (const id of new Set([...Object.keys(before), ...Object.keys(after)])) {
        const was = Object.hasOwn(before, id) ? before[id] : null, now = Object.hasOwn(after, id) ? after[id] : null;
        if (was !== now) changes.push([prefix + id, now]);
    }
    return changes;
}

// Writes precede removals, so a transition that moves state between entries
// is never observed, or left by a failed write, with the state missing from
// both. A failure restores the entries already changed and rethrows.
export function applyItemChanges(changes: readonly ItemChange[]): void {
    const applied: ItemChange[] = [];
    try {
        for (const [key, value] of [...changes.filter(([, value]) => value !== null), ...changes.filter(([, value]) => value === null)]) {
            applied.push([key, localStorage.getItem(key)]);
            if (value === null) localStorage.removeItem(key); else localStorage.setItem(key, value);
        }
    } catch (error) {
        for (const [key, previous] of applied.reverse()) {
            try { if (previous === null) localStorage.removeItem(key); else localStorage.setItem(key, previous); } catch { /* Keep restoring the rest. */ }
        }
        throw error;
    }
}

// Stored values from a retired layout are not read. Removing them is best effort.
export function discardItem(key: string) { try { localStorage.removeItem(key); } catch { /* Storage unavailable. */ } }
