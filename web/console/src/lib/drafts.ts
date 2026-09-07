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

const activeRequests = new Set<string>();
function load(): DraftState {
    const saved = JSON.parse(localStorage.getItem(storageKey) || "null");
    return {
        drafts: Object.fromEntries(Object.entries(saved?.drafts || {}).filter(([, text]) => typeof text === "string")) as Record<string, string>,
        submissions: Object.fromEntries(Object.entries(saved?.submissions || {}).flatMap(([id, value]) => {
            const pending = value as Submission | null;
            return pending && typeof pending.input === "string" ? [[id, { ...pending, id: typeof pending.id === "string" && pending.id ? pending.id : undefined, quotes: validQuotes(pending.quotes), refs: validMaterials(pending.refs), locale: typeof pending.locale === "string" ? pending.locale : undefined, fromDraft: pending.fromDraft !== false, uncertain: pending.id && activeRequests.has(pending.id) ? pending.uncertain : !pending.rejected && !pending.conflict, active: !!pending.id && activeRequests.has(pending.id) }]] : [];
        })),
        quotes: validQuotes(saved?.quotes),
        materials: Object.fromEntries(Object.entries(saved?.materials || {}).map(([id, refs]) => [id, validMaterials(refs)])),
    };
}

// A submission and the consumed draft/quotes are saved in one write before I/O.
let state: DraftState;
try { state = load(); } catch { state = { drafts: {}, submissions: {}, quotes: [], materials: {} }; }
interface EditingDraft { text: string; expected: string; version: number; issue?: "conflict" | "storage" }
const editing = new Map<string, EditingDraft>();
let writes = Promise.resolve();
function transaction<T>(action: () => T): Promise<T> {
    const work = writes.then(async () => {
        if (!globalThis.navigator?.locks) throw new Error("Draft storage locking is unavailable");
        return navigator.locks.request(storageKey, () => {
            state = load();
            try { return action(); } finally { notify(); }
        });
    });
    writes = work.then(() => {}, () => {});
    return work;
}
if (typeof window !== "undefined") window.addEventListener("storage", (event) => {
    if (event.key !== storageKey && event.key !== null) return;
    try { state = load(); notify(); } catch { /* Retain the last readable state. Writes will fail until storage recovers. */ }
});
let stops: Record<string, StopState> = {};
function save(next: DraftState): boolean {
    try { localStorage.setItem(storageKey, JSON.stringify(next)); }
    catch { return false; }
    state = next;
    activeRequests.clear();
    for (const pending of Object.values(next.submissions)) if (pending.id && pending.active) activeRequests.add(pending.id);
    notify();
    return true;
}
function subscribe(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener); }; }
export function useDraft(id: string): string { return useSyncExternalStore(subscribe, () => editing.get(id)?.text ?? state.drafts[id] ?? ""); }
export function useSavedDraft(id: string): string { return useSyncExternalStore(subscribe, () => state.drafts[id] || ""); }
export function useDraftIssue(id: string) { return useSyncExternalStore(subscribe, () => !globalThis.navigator?.locks ? "unavailable" : editing.get(id)?.issue ?? null); }
export function updateDraft(id: string, value: SetStateAction<string>): Promise<boolean> {
    const previous = editing.get(id);
    const current = previous?.text ?? state.drafts[id] ?? "";
    const entry = previous || { text: current, expected: state.drafts[id] || "", version: 0 };
    const text = typeof value === "function" ? value(current) : value;
    entry.text = text; entry.version++;
    const version = entry.version;
    editing.set(id, entry); notify();
    return transaction(() => {
        if (entry.issue === "conflict" || (state.drafts[id] || "") !== entry.expected) { entry.issue = "conflict"; return false; }
        const drafts = { ...state.drafts };
        if (text) drafts[id] = text; else delete drafts[id];
        if (!save({ ...state, drafts })) { entry.issue = "storage"; return false; }
        entry.expected = text; entry.issue = undefined;
        if (entry.version === version) editing.delete(id);
        return true;
    }).catch(() => { entry.issue = "storage"; notify(); return false; });
}
export function resolveDraftConflict(id: string, choice: "local" | "remote"): Promise<boolean> {
    const expectedVersion = editing.get(id)?.version;
    const expectedSaved = state.drafts[id] || "";
    return transaction(() => {
        const entry = editing.get(id);
        if (!entry) return true;
        if (entry.version !== expectedVersion || (state.drafts[id] || "") !== expectedSaved) return false;
        if (choice === "local") {
            const drafts = { ...state.drafts };
            if (entry.text) drafts[id] = entry.text; else delete drafts[id];
            if (!save({ ...state, drafts })) { entry.issue = "storage"; return false; }
        }
        editing.delete(id); return true;
    }).catch(() => false);
}
export function useQuotes(): [QuoteRef[], (value: SetStateAction<QuoteRef[]>) => Promise<boolean>] {
    return [useSyncExternalStore(subscribe, () => state.quotes), (value) => transaction(() => save({ ...state, quotes: typeof value === "function" ? value(state.quotes) : value })).catch(() => false)];
}
export function useSubmission(id: string): Submission | null { return useSyncExternalStore(subscribe, () => state.submissions[id] || null); }
function beginSubmissionLocked(id: string, input: string, quotes: QuoteRef[], fromDraft = true, locale?: string): Submission | null {
    if (state.submissions[id]) return null;
    if (editing.has(id)) throw new Error("Review the unsaved draft before sending");
    const pending: Submission = { id: commandID(), input, quotes, refs: fromDraft ? state.materials[id] || [] : [], locale, fromDraft, active: true };
    const materials = { ...state.materials }; if (fromDraft) delete materials[id];
    const drafts = { ...state.drafts };
    if (fromDraft) delete drafts[id];
    const remaining = state.quotes.filter((quote) => !quotes.some((carried) => sameQuote(quote, carried)));
    if (!save({ ...state, drafts, materials, quotes: remaining, submissions: { ...state.submissions, [id]: pending } })) throw new Error("Cannot retain pending submission");
    return pending;
}
function retrySubmissionLocked(id: string): Submission | null {
    const current = state.submissions[id];
    if (!current?.id || current.active || current.conflict || current.rejected) return null;
    const pending = { ...current, active: true, error: undefined };
    if (!save({ ...state, submissions: { ...state.submissions, [id]: pending } })) throw new Error("Cannot retain pending submission");
    return pending;
}
function finishSubmissionLocked(id: string, expectedID?: string): boolean {
    if (expectedID && state.submissions[id]?.id !== expectedID) return false;
    const submissions = { ...state.submissions };
    delete submissions[id];
    return save({ ...state, submissions });
}
function failSubmissionLocked(id: string, expectedID: string | undefined, error: string, outcome: "unknown" | "rejected" | "conflict"): boolean {
    const current = state.submissions[id];
    if (!current || current.id !== expectedID) return false;
    // A retry not reaching the server says nothing about the earlier request.
    // Preserve its original identity until acknowledgement or reconciliation.
    if (current.uncertain && outcome === "rejected") outcome = "unknown";
    if (!save({ ...state, submissions: { ...state.submissions, [id]: { ...current, active: false, error, rejected: outcome === "rejected", conflict: outcome === "conflict", uncertain: outcome === "unknown" } } })) return false;
    if (outcome === "rejected") restoreSubmissionLocked(id);
    return true;
}
function reconcileSubmissionLocked(id: string, queue: Exchange[]): boolean {
    const pending = state.submissions[id];
    if (!pending?.id || pending.conflict || pending.rejected) return false;
    const normalized = (text: string) => text.replace(/\r\n/g, "\n").trim();
    const receipt = queue.find((exchange) => exchange.conversation === id && exchange.key === `client:${pending.id}`);
    if (!receipt || normalized(receipt.input) !== normalized(pending.input) || (receipt.quotes?.length || 0) !== pending.quotes.length
        || pending.quotes.some((quote, index) => !sameQuote(quote, receipt.quotes![index]))
        || (pending.refs || []).length !== (receipt.refs || []).length || (pending.refs || []).some((ref, index) => refKey(ref) !== refKey(receipt.refs![index]))
        || (pending.locale && receipt.locale !== pending.locale)) return false;
    return finishSubmissionLocked(id, pending.id);
}
function restoreSubmissionLocked(id: string): boolean {
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
    return save({ ...state, drafts, submissions, quotes, materials: { ...state.materials, [id]: refs } });
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
function addDraftMaterialLocked(conversation: string, ref: DraftMaterial): boolean {
    const previous = state.materials[conversation] || [];
    if (previous.some((item) => refKey(item) === refKey(ref))) return true;
    return save({ ...state, materials: { ...state.materials, [conversation]: [...previous, ref] } });
}
function removeDraftMaterialLocked(conversation: string, ref: MaterialRef): boolean {
    return save({ ...state, materials: { ...state.materials, [conversation]: (state.materials[conversation] || []).filter((item) => refKey(item) !== refKey(ref)) } });
}
export const submissionRefs = (submission: Submission) => submission.refs?.map(wireRef);

export function beginSubmission(id: string, input: string, quotes: QuoteRef[], fromDraft = true, locale?: string, includeDraftQuotes = false) {
    const expectedRefs = (state.materials[id] || []).map(refKey);
    return transaction(() => {
        if (state.submissions[id]) return null;
        if (fromDraft && ((state.drafts[id] || "").trim() !== input.trim() || JSON.stringify((state.materials[id] || []).map(refKey)) !== JSON.stringify(expectedRefs))) {
            if (!editing.has(id)) editing.set(id, { text: input, expected: state.drafts[id] || "", version: 0, issue: "conflict" });
            throw new Error("Review the unsaved draft before sending");
        }
        return beginSubmissionLocked(id, input, includeDraftQuotes ? state.quotes : quotes, fromDraft, locale);
    });
}
export const retrySubmission = (...args: Parameters<typeof retrySubmissionLocked>) => transaction(() => retrySubmissionLocked(...args));
function settleRequest(id: string, expectedID?: string) {
    if (expectedID) activeRequests.delete(expectedID);
    const pending = state.submissions[id];
    if (!pending || (expectedID && pending.id !== expectedID)) return;
    if (pending.id) activeRequests.delete(pending.id);
    state = { ...state, submissions: { ...state.submissions, [id]: { ...pending, active: false, uncertain: !pending.rejected && !pending.conflict } } };
    notify();
}
export const finishSubmission = (...args: Parameters<typeof finishSubmissionLocked>) => transaction(() => finishSubmissionLocked(...args)).catch(() => false).finally(() => settleRequest(args[0], args[1]));
export const failSubmission = (...args: Parameters<typeof failSubmissionLocked>) => transaction(() => failSubmissionLocked(...args)).catch(() => false).finally(() => settleRequest(args[0], args[1]));
export const reconcileSubmission = (...args: Parameters<typeof reconcileSubmissionLocked>) => transaction(() => reconcileSubmissionLocked(...args)).catch(() => false);
export const restoreSubmission = (...args: Parameters<typeof restoreSubmissionLocked>) => transaction(() => restoreSubmissionLocked(...args)).catch(() => false);
export const addDraftMaterial = (...args: Parameters<typeof addDraftMaterialLocked>) => transaction(() => addDraftMaterialLocked(...args)).catch(() => false);
export const removeDraftMaterial = (...args: Parameters<typeof removeDraftMaterialLocked>) => transaction(() => removeDraftMaterialLocked(...args)).catch(() => false);
