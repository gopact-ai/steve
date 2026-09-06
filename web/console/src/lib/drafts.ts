import { useSyncExternalStore, type SetStateAction } from "react";
import { refKey, wireRef } from "./material-ref.ts";
import type { Exchange, QuoteRef, DraftMaterial, MaterialRef } from "./types";

export interface Submission { id?: string; input: string; quotes: QuoteRef[]; refs?: DraftMaterial[]; locale?: string; active: boolean; fromDraft: boolean; error?: string; rejected?: boolean; conflict?: boolean; uncertain?: boolean }
interface DraftState { drafts: Record<string, string>; submissions: Record<string, Submission>; quotes: QuoteRef[]; materials: Record<string, DraftMaterial[]> }
interface StopState { id: string; active: boolean; uncertain?: boolean; error?: string; message?: string }
const storageKey = "steve.console.drafts";
const listeners = new Set<() => void>();
const validQuotes = (value: unknown): QuoteRef[] => Array.isArray(value) ? value.filter((q) => q && typeof q.conversation === "string" && typeof q.reply_id === "string") : [];
const validMaterials = (value: unknown): DraftMaterial[] => Array.isArray(value) ? value.filter((item) => item && typeof item.id === "string" && typeof item.project === "string" && typeof item.title === "string" && ["text", "image", "binary"].includes(item.kind) && typeof item.mime === "string" && Number.isFinite(item.size)) : [];
const emptyMaterials: DraftMaterial[] = [];
const sameQuote = (a: QuoteRef, b: QuoteRef) => a.conversation === b.conversation && a.reply_id === b.reply_id;
const notify = () => { for (const listener of listeners) listener(); };
const commandID = () => { try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(16).slice(2)}`; } };

function load(): DraftState {
    try {
        const saved = JSON.parse(sessionStorage.getItem(storageKey) || "null");
        return {
            drafts: Object.fromEntries(Object.entries(saved?.drafts || {}).filter(([, text]) => typeof text === "string")) as Record<string, string>,
            submissions: Object.fromEntries(Object.entries(saved?.submissions || {}).flatMap(([id, value]) => {
                const pending = value as Submission | null;
                return pending && typeof pending.input === "string" ? [[id, { ...pending, id: typeof pending.id === "string" && pending.id ? pending.id : undefined, quotes: validQuotes(pending.quotes), refs: validMaterials(pending.refs), locale: typeof pending.locale === "string" ? pending.locale : undefined, fromDraft: pending.fromDraft !== false, uncertain: !pending.rejected && !pending.conflict, active: false }]] : [];
            })),
            quotes: validQuotes(saved?.quotes),
            materials: Object.fromEntries(Object.entries(saved?.materials || {}).map(([id, refs]) => [id, validMaterials(refs)])),
        };
    } catch { return { drafts: {}, submissions: {}, quotes: [], materials: {} }; }
}

// A submission and the consumed draft/quotes are saved in one write before I/O.
let state = load();
let stops: Record<string, StopState> = {};
function save(next: DraftState, requireStorage = false): boolean {
    let saved = true;
    try { sessionStorage.setItem(storageKey, JSON.stringify(next)); }
    catch { if (requireStorage) return false; saved = false; }
    state = next;
    notify();
    return saved;
}
function subscribe(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener); }; }
export function useDraft(id: string): string { return useSyncExternalStore(subscribe, () => state.drafts[id] || ""); }
export function updateDraft(id: string, value: SetStateAction<string>): boolean {
    const text = typeof value === "function" ? value(state.drafts[id] || "") : value;
    const drafts = { ...state.drafts };
    if (text) drafts[id] = text; else delete drafts[id];
    return save({ ...state, drafts });
}
export function useQuotes(): [QuoteRef[], (value: SetStateAction<QuoteRef[]>) => boolean] {
    return [useSyncExternalStore(subscribe, () => state.quotes), (value) => save({ ...state, quotes: typeof value === "function" ? value(state.quotes) : value })];
}
export function useSubmission(id: string): Submission | null { return useSyncExternalStore(subscribe, () => state.submissions[id] || null); }
export function beginSubmission(id: string, input: string, quotes: QuoteRef[], fromDraft = true, locale?: string): Submission | null {
    if (state.submissions[id]) return null;
    const pending: Submission = { id: commandID(), input, quotes, refs: fromDraft ? state.materials[id] || [] : [], locale, fromDraft, active: true };
    const materials = { ...state.materials }; if (fromDraft) delete materials[id];
    const drafts = { ...state.drafts };
    if (fromDraft) delete drafts[id];
    const remaining = state.quotes.filter((quote) => !quotes.some((carried) => sameQuote(quote, carried)));
    if (!save({ ...state, drafts, materials, quotes: remaining, submissions: { ...state.submissions, [id]: pending } }, true)) throw new Error("Cannot retain pending submission");
    return pending;
}
export function retrySubmission(id: string): Submission | null {
    const current = state.submissions[id];
    if (!current?.id || current.active || current.conflict || current.rejected) return null;
    const pending = { ...current, active: true, error: undefined };
    if (!save({ ...state, submissions: { ...state.submissions, [id]: pending } }, true)) throw new Error("Cannot retain pending submission");
    return pending;
}
export function finishSubmission(id: string, expectedID?: string): boolean {
    if (expectedID && state.submissions[id]?.id !== expectedID) return false;
    const submissions = { ...state.submissions };
    delete submissions[id];
    return save({ ...state, submissions });
}
export function failSubmission(id: string, expectedID: string | undefined, error: string, outcome: "unknown" | "rejected" | "conflict"): boolean {
    const current = state.submissions[id];
    if (!current || current.id !== expectedID) return false;
    // A retry not reaching the server says nothing about the earlier request.
    // Preserve its original identity until acknowledgement or reconciliation.
    if (current.uncertain && outcome === "rejected") outcome = "unknown";
    save({ ...state, submissions: { ...state.submissions, [id]: { ...current, active: false, error, rejected: outcome === "rejected", conflict: outcome === "conflict", uncertain: outcome === "unknown" } } });
    if (outcome === "rejected") restoreSubmission(id);
    return true;
}
export function reconcileSubmission(id: string, queue: Exchange[]): boolean {
    const pending = state.submissions[id];
    if (!pending?.id || pending.conflict || pending.rejected) return false;
    const normalized = (text: string) => text.replace(/\r\n/g, "\n").trim();
    const receipt = queue.find((exchange) => exchange.conversation === id && exchange.key === `client:${pending.id}`);
    if (!receipt || normalized(receipt.input) !== normalized(pending.input) || (receipt.quotes?.length || 0) !== pending.quotes.length
        || pending.quotes.some((quote, index) => !sameQuote(quote, receipt.quotes![index]))
        || (pending.refs || []).length !== (receipt.refs || []).length || (pending.refs || []).some((ref, index) => refKey(ref) !== refKey(receipt.refs![index]))
        || (pending.locale && receipt.locale !== pending.locale)) return false;
    finishSubmission(id, pending.id);
    return true;
}
export function restoreSubmission(id: string): boolean {
    const pending = state.submissions[id];
    // Keyed uncertain submissions must retry their original identity, not become
    // a fresh command. Older keyless pending entries require explicit recovery.
    if (!pending || pending.active || (pending.id && !pending.rejected)) return false;
    const current = state.drafts[id];
    const drafts = { ...state.drafts };
    if (pending.fromDraft) drafts[id] = current ? `${pending.input}\n${current}` : pending.input;
    const submissions = { ...state.submissions };
    delete submissions[id];
    const quotes = [...pending.quotes, ...state.quotes.filter((quote) => !pending.quotes.some((carried) => sameQuote(quote, carried)))];
    const refs = [...(pending.refs || []), ...(state.materials[id] || []).filter((ref) => !(pending.refs || []).some((old) => refKey(old) === refKey(ref)))];
    return save({ ...state, drafts, submissions, quotes, materials: { ...state.materials, [id]: refs } }, true);
}

// Stop requests outlive a route instance. A transport failure retains the same
// command identity for the next explicit stop, while the original guard stays shared.
export function useStops() { return useSyncExternalStore(subscribe, () => stops); }
export function beginStop(conversation: string): string | null {
    const current = stops[conversation];
    if (current?.active) return null;
    const id = current?.uncertain ? current.id : commandID();
    stops = { ...stops, [conversation]: { id, active: true, uncertain: current?.uncertain } };
    notify();
    return id;
}
export function finishStop(conversation: string, id: string, result: { error?: string; message?: string; uncertain?: boolean }) {
    if (stops[conversation]?.id !== id) return;
    const uncertain = result.uncertain || (!!result.error && stops[conversation].uncertain);
    stops = { ...stops, [conversation]: { id, active: false, ...result, uncertain } };
    notify();
}
export function isStopPending(conversation: string) { return !!(stops[conversation]?.active || stops[conversation]?.uncertain); }
export function clearStopNotice(conversation: string) {
    if (isStopPending(conversation)) return;
    const next = { ...stops }; delete next[conversation]; stops = next; notify();
}

export function useMaterials(id: string): DraftMaterial[] { return useSyncExternalStore(subscribe, () => state.materials[id] || emptyMaterials); }
export function addDraftMaterial(conversation: string, ref: DraftMaterial): boolean {
    const previous = state.materials[conversation] || [];
    if (previous.some((item) => refKey(item) === refKey(ref))) return true;
    return save({ ...state, materials: { ...state.materials, [conversation]: [...previous, ref] } }, true);
}
export function removeDraftMaterial(conversation: string, ref: MaterialRef): boolean {
    return save({ ...state, materials: { ...state.materials, [conversation]: (state.materials[conversation] || []).filter((item) => refKey(item) !== refKey(ref)) } }, true);
}
export const submissionRefs = (submission: Submission) => submission.refs?.map(wireRef);
