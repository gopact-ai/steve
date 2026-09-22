import { useSyncExternalStore, type SetStateAction } from "react";
import { refKey, wireRef } from "./material-ref.ts";
import { applyItemChanges, discardItem, itemChanges, readItems } from "./item-storage.ts";
import type { Exchange, QuoteRef, DraftMaterial, MaterialRef } from "./types";
export interface Submission { id?: string; input: string; quotes: QuoteRef[]; refs?: DraftMaterial[]; locale?: string; active: boolean; fromDraft: boolean; error?: string; rejected?: boolean; conflict?: boolean; uncertain?: boolean; rewind?: string }
// Rewind is the line a thread is being taken back to while its
// replacement is typed. It is kept beside the draft, and for the same
// reason: a reload in the middle of an edit must not quietly turn the
// rewrite into a new message appended under the old one.
export interface Rewind { reply: string; restore: string }
interface DraftState { drafts: Record<string, string>; submissions: Record<string, Submission>; quotes: QuoteRef[]; quoteOrder: Record<string, number>; materials: Record<string, DraftMaterial[]>; rewinds: Record<string, Rewind> }
interface StopState { id: string; active: boolean; uncertain?: boolean; error?: string; message?: string }
// Each conversation's text, attachments, pending submission and rewind, and
// each carried quote, is stored under its own key (see item-storage.ts).
// The lock keeps the name of the former single stored value, now discarded.
const lockName = "steve.console.drafts";
const itemPrefix = "steve.console.draft.";
const keys = { drafts: `${itemPrefix}text:`, materials: `${itemPrefix}materials:`, submissions: `${itemPrefix}submission:`, rewinds: `${itemPrefix}rewind:`, quotes: `${itemPrefix}quote:` } as const;
const listeners = new Set<() => void>();
const validQuotes = (value: unknown): QuoteRef[] => Array.isArray(value) ? value.filter((q) => q && typeof q.conversation === "string" && typeof q.reply_id === "string") : [];
const validMaterials = (value: unknown): DraftMaterial[] => Array.isArray(value) ? value.filter((item) => item && typeof item.id === "string" && typeof item.project === "string" && typeof item.title === "string" && ["text", "image", "binary"].includes(item.kind) && typeof item.mime === "string" && Number.isFinite(item.size)) : [];
const emptyMaterials: DraftMaterial[] = [];
const sameQuote = (a: QuoteRef, b: QuoteRef) => a.conversation === b.conversation && a.reply_id === b.reply_id;
const notify = () => { for (const listener of listeners) listener(); };
const quoteKey = (quote: QuoteRef) => JSON.stringify([quote.conversation, quote.reply_id]);
const parse = (value: string): unknown => { try { return JSON.parse(value); } catch { return null; } };
const commandID = () => { try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(16).slice(2)}`; } };

const activeRequests = new Set<string>();
function stored<T>(prefix: string, valid: (value: unknown) => T | null): Record<string, T> {
    return Object.fromEntries(readItems(prefix).flatMap(([id, raw]) => { const value = valid(parse(raw)); return value === null ? [] : [[id, value]]; }));
}
function load(): DraftState {
    const quotes = Object.values(stored(keys.quotes, (value) => {
        const entry = value as { quote?: unknown; order?: unknown } | null;
        const [quote] = validQuotes([entry?.quote]);
        return quote && typeof entry!.order === "number" && Number.isFinite(entry!.order) ? { quote, order: entry!.order } : null;
    })).sort((a, b) => a.order - b.order || quoteKey(a.quote).localeCompare(quoteKey(b.quote)));
    return {
        drafts: Object.fromEntries(readItems(keys.drafts)),
        submissions: stored(keys.submissions, (value) => {
            const pending = value as Submission | null;
            return pending && typeof pending.input === "string" ? { ...pending, id: typeof pending.id === "string" && pending.id ? pending.id : undefined, quotes: validQuotes(pending.quotes), refs: validMaterials(pending.refs), locale: typeof pending.locale === "string" ? pending.locale : undefined, fromDraft: pending.fromDraft !== false, uncertain: pending.id && activeRequests.has(pending.id) ? pending.uncertain : !pending.rejected && !pending.conflict, active: !!pending.id && activeRequests.has(pending.id) } : null;
        }),
        quotes: quotes.map((entry) => entry.quote),
        quoteOrder: Object.fromEntries(quotes.map((entry) => [quoteKey(entry.quote), entry.order])),
        materials: stored(keys.materials, validMaterials),
        rewinds: stored(keys.rewinds, (value) => {
            const entry = value as Rewind | null;
            return entry && typeof entry.reply === "string" && entry.reply ? { reply: entry.reply, restore: typeof entry.restore === "string" ? entry.restore : "" } : null;
        }),
    };
}
// A quote keeps its stored position. One added or moved is placed between
// its neighbours, so windows adding different quotes never renumber shared ones.
function orderQuotes(quotes: QuoteRef[], known: Record<string, number>): Record<string, number> {
    const order: Record<string, number> = {};
    let previous = -Infinity;
    quotes.forEach((quote, index) => {
        const key = quoteKey(quote), own = Object.hasOwn(known, key) ? known[key] : undefined;
        if (own !== undefined && own > previous) { order[key] = previous = own; return; }
        const next = quotes.slice(index + 1).map((later) => Object.hasOwn(known, quoteKey(later)) ? known[quoteKey(later)] : undefined).find((value) => value !== undefined && value > previous);
        order[key] = previous = previous === -Infinity ? (next === undefined ? 0 : next - 1) : next === undefined ? previous + 1 : (previous + next) / 2;
    });
    return order;
}
function encode(value: DraftState): Record<keyof typeof keys, Record<string, string | null>> {
    const each = <T>(record: Record<string, T>, write: (item: T) => string | null) => Object.fromEntries(Object.entries(record).map(([id, item]) => [id, write(item)]));
    return {
        submissions: each(value.submissions, (pending) => JSON.stringify(pending)),
        drafts: each(value.drafts, (text) => text || null),
        materials: each(value.materials, (refs) => refs.length ? JSON.stringify(refs) : null),
        rewinds: each(value.rewinds, (entry) => JSON.stringify(entry)),
        quotes: Object.fromEntries(value.quotes.map((quote) => [quoteKey(quote), JSON.stringify({ quote, order: value.quoteOrder[quoteKey(quote)] })])),
    };
}

// A submission is saved before the draft and quotes it consumes are removed,
// all before I/O, so no window can observe or be left with neither.
let state: DraftState;
try { state = load(); } catch { state = { drafts: {}, submissions: {}, quotes: [], quoteOrder: {}, materials: {}, rewinds: {} }; }
discardItem(lockName);
interface EditingDraft { text: string; expected: string; version: number; issue?: "conflict" | "storage" }
const editing = new Map<string, EditingDraft>();
let writes = Promise.resolve();
function transaction<T>(action: () => T): Promise<T> {
    const work = writes.then(async () => {
        if (!globalThis.navigator?.locks) throw new Error("Draft storage locking is unavailable");
        return navigator.locks.request(lockName, () => {
            state = load();
            try { return action(); } finally { notify(); }
        });
    });
    writes = work.then(() => {}, () => {});
    return work;
}
if (typeof window !== "undefined") window.addEventListener("storage", (event) => {
    if (event.key !== null && !event.key.startsWith(itemPrefix)) return;
    try { state = load(); notify(); } catch { /* Retain the last readable state. Writes will fail until storage recovers. */ }
});
let stops: Record<string, StopState> = {};
function save(value: DraftState): boolean {
    const next = { ...value, quoteOrder: orderQuotes(value.quotes, state.quoteOrder) };
    const before = encode(state), after = encode(next);
    try { applyItemChanges((Object.keys(keys) as (keyof typeof keys)[]).flatMap((kind) => itemChanges(keys[kind], before[kind], after[kind]))); }
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
function beginSubmissionLocked(id: string, input: string, quotes: QuoteRef[], fromDraft = true, locale?: string, rewind?: string): Submission | null {
    if (state.submissions[id]) return null;
    if (editing.has(id)) throw new Error("Review the unsaved draft before sending");
    const pending: Submission = { id: commandID(), input, quotes, refs: fromDraft ? state.materials[id] || [] : [], locale, fromDraft, active: true, rewind: rewind || undefined };
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

export function beginSubmission(id: string, input: string, quotes: QuoteRef[], fromDraft = true, locale?: string, includeDraftQuotes = false, rewind?: string) {
    const expectedRefs = (state.materials[id] || []).map(refKey);
    return transaction(() => {
        if (state.submissions[id]) return null;
        if (fromDraft && ((state.drafts[id] || "").trim() !== input.trim() || JSON.stringify((state.materials[id] || []).map(refKey)) !== JSON.stringify(expectedRefs))) {
            if (!editing.has(id)) editing.set(id, { text: input, expected: state.drafts[id] || "", version: 0, issue: "conflict" });
            throw new Error("Review the unsaved draft before sending");
        }
        return beginSubmissionLocked(id, input, includeDraftQuotes ? state.quotes : quotes, fromDraft, locale, rewind);
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

// A thread is taken back to a line already sent by editing it: the line
// goes back in the box, and what was being typed is kept so cancelling
// gives it back. The target survives a reload, because the send that
// follows means something different from an ordinary one.
export function useRewind(id: string): Rewind | null { return useSyncExternalStore(subscribe, () => state.rewinds[id] || null); }
export const rewindOf = (id: string) => state.rewinds[id] || null;
// The draft itself is placed through updateDraft, like any other text put
// in the box, so an edit already in flight is not written around.
export function beginRewind(id: string, reply: string, restore: string) {
    return transaction(() => save({ ...state, rewinds: { ...state.rewinds, [id]: { reply, restore: state.rewinds[id]?.restore ?? restore } } })).catch(() => false);
}
export function endRewind(id: string): Promise<Rewind | null> {
    return transaction(() => {
        const entry = state.rewinds[id];
        if (!entry) return null;
        const rewinds = { ...state.rewinds }; delete rewinds[id];
        save({ ...state, rewinds });
        return entry;
    }).catch(() => null);
}
