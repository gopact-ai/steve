import { useSyncExternalStore, type SetStateAction } from "react";
import type { QuoteRef } from "./types";

interface Submission { input: string; quotes: QuoteRef[]; active: boolean }
interface DraftState { drafts: Record<string, string>; submissions: Record<string, Submission>; quotes: QuoteRef[] }
const storageKey = "steve.console.drafts";
const listeners = new Set<() => void>();
const validQuotes = (value: unknown): QuoteRef[] => Array.isArray(value) ? value.filter((q) => q && typeof q.conversation === "string" && typeof q.reply_id === "string") : [];

function load(): DraftState {
    try {
        const saved = JSON.parse(sessionStorage.getItem(storageKey) || "null");
        return {
            drafts: Object.fromEntries(Object.entries(saved?.drafts || {}).filter(([, text]) => typeof text === "string")) as Record<string, string>,
            submissions: Object.fromEntries(Object.entries(saved?.submissions || {}).flatMap(([id, value]) => {
                const pending = value as Submission | null;
                return pending && typeof pending.input === "string" ? [[id, { input: pending.input, quotes: validQuotes(pending.quotes), active: false }]] : [];
            })),
            quotes: validQuotes(saved?.quotes),
        };
    } catch { return { drafts: {}, submissions: {}, quotes: [] }; }
}

// One shared snapshot survives route mounts. One storage write also makes
// recovery atomic: never discard the pending copy before saving its draft.
let state = load();
function save(next: DraftState, requireStorage = false): boolean {
    let saved = true;
    try { sessionStorage.setItem(storageKey, JSON.stringify(next)); }
    catch { if (requireStorage) return false; saved = false; }
    state = next;
    for (const listener of listeners) listener();
    return saved;
}
function subscribe(listener: () => void) {
    listeners.add(listener);
    return () => { listeners.delete(listener); };
}
export function useDraft(id: string): string {
    return useSyncExternalStore(subscribe, () => state.drafts[id] || "");
}
export function updateDraft(id: string, value: SetStateAction<string>): boolean {
    const text = typeof value === "function" ? value(state.drafts[id] || "") : value;
    const drafts = { ...state.drafts };
    if (text) drafts[id] = text; else delete drafts[id];
    return save({ ...state, drafts });
}
export function useQuotes(): [QuoteRef[], (value: SetStateAction<QuoteRef[]>) => boolean] {
    const quotes = useSyncExternalStore(subscribe, () => state.quotes);
    return [quotes, updateQuotes];
}
function updateQuotes(value: SetStateAction<QuoteRef[]>): boolean {
    return save({ ...state, quotes: typeof value === "function" ? value(state.quotes) : value });
}
export function useSubmission(id: string): Submission | null {
    return useSyncExternalStore(subscribe, () => state.submissions[id] || null);
}
export function beginSubmission(id: string, input: string, quotes: QuoteRef[]): boolean {
    if (state.submissions[id]) return false;
    const pending = { input, quotes, active: true };
    if (!save({ ...state, submissions: { ...state.submissions, [id]: pending } }, true)) throw new Error("Cannot retain pending submission");
    return true;
}
export function finishSubmission(id: string): boolean {
    const submissions = { ...state.submissions };
    delete submissions[id];
    return save({ ...state, submissions });
}
export function restoreSubmission(id: string): boolean {
    const pending = state.submissions[id];
    if (!pending || pending.active) return false;
    const current = state.drafts[id];
    const drafts = { ...state.drafts, [id]: current ? `${pending.input}\n${current}` : pending.input };
    const submissions = { ...state.submissions };
    delete submissions[id];
    const quotes = [...pending.quotes, ...state.quotes.filter((q) => !pending.quotes.some((p) => p.conversation === q.conversation && p.reply_id === q.reply_id))];
    return save({ ...state, drafts, submissions, quotes }, true);
}
