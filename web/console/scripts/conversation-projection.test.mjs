import assert from "node:assert/strict";
import test from "node:test";
import { createConversationProjection, conversationReadToken, acceptsConversationRead, reduceConversation } from "../src/lib/conversation-projection.ts";
import { conversationContextRevision } from "../src/lib/conversation-context-revision.ts";

const A = "console:a", B = "console:b";
const at = (n) => `2026-09-19T00:00:${String(n).padStart(2, "0")}Z`;
const event = (kind, n, extra = {}) => ({ kind: `console.${kind}`, conversation: A, at: at(n), n, ...extra });
const events = (state, ...batch) => reduceConversation(state, { type: "events", events: batch });
function read(state, resource, request = 1) {
    const token = conversationReadToken(state, resource, request);
    return [reduceConversation(state, { type: "read-started", token }), token];
}
const reply = (id, exchange_id, n) => ({ id, exchange_id, conversation: A, at: at(n), kind: "reply", text: id });
const exchange = (id, state = "running") => ({ id, conversation: A, state, input: id, enqueued_at: at(1), started_at: at(2) });

test("the real projection folds transcript, live turn and delegations without mutating its input", () => {
    const original = createConversationProjection(A);
    let state = events(original, event("sent", 1, { exchange_id: "a", reply_id: "sent-a", text: "question" }));
    state = events(state, event("progress", 2, { exchange_id: "a", progress: { answer: "partial" } }),
        { ...event("step", 3), task_id: "child", step: { state: "running" } });
    assert.equal(original.entries.length, 0);
    assert.equal(original.live, null);
    assert.equal(state.entries[0].input, "question");
    assert.equal(state.live.turn.answer, "partial");
    assert.equal(state.delegations["#child"].step.state, "running");
    state = events(state, event("reply", 4, { exchange_id: "a", reply_id: "reply-a", text: "finished" }));
    assert.equal(state.live, null);
    assert.equal(state.entries[1].text, "finished");
    assert.equal(state.delegations["#child"].step.state, "running");
});

test("duplicates, reordered batches and late sent/progress cannot reopen a completed exchange", () => {
    const sent = event("sent", 1, { exchange_id: "a", reply_id: "sent-a", text: "question" });
    const done = event("reply", 4, { exchange_id: "a", reply_id: "reply-a", text: "done" });
    let state = events(createConversationProjection(A), done, sent, sent);
    assert.deepEqual(state.entries.map((r) => r.id), ["sent-a", "reply-a"]);
    assert.equal(state.live, null);
    const before = state;
    state = events(state, done, sent);
    assert.equal(state, before, "replaying the same arrivals is a no-op");
    state = events(state, { ...sent, n: 5 }, event("progress", 6, { exchange_id: "a", progress: { answer: "obsolete" } }));
    assert.equal(state.live, null);
    assert.equal(state.entries.length, 2);
});

test("out-of-order progress is ordered per exchange/step, not by a global event cursor", () => {
    let state = events(createConversationProjection(A), event("sent", 1, { exchange_id: "a" }),
        event("progress", 5, { exchange_id: "a", progress: { answer: "latest" } }));
    state = events(state, event("progress", 3, { exchange_id: "a", progress: { answer: "old" } }),
        { ...event("step", 2), task_id: "independent", step: { state: "running" } });
    assert.equal(state.live.turn.answer, "latest");
    assert.ok(state.delegations["#independent"]);
    state = events(state, event("sent", 8, { exchange_id: "b" }), event("reply", 7, { exchange_id: "a", text: "old completion" }));
    assert.equal(state.live.exchangeID, "b");
});

test("a late sent event with the same timestamp cannot displace or reopen a newer turn", () => {
    const newer = event("sent", 5, { exchange_id: "b", reply_id: "sent-b" });
    const late = event("sent", 2, { at: newer.at, exchange_id: "a", reply_id: "sent-a" });
    let state = events(createConversationProjection(A), newer,
        event("progress", 6, { exchange_id: "b", progress: { answer: "newer" } }), late);
    assert.equal(state.live.exchangeID, "b");
    assert.equal(state.live.turn.answer, "newer");
    state = events(state, event("reply", 7, { exchange_id: "b", text: "done" }),
        event("sent", 1, { exchange_id: "older", reply_id: "sent-older" }));
    assert.equal(state.live, null);
    assert.equal(state.exchanges.some((item) => item.id === "older" && item.state === "running"), false);
});

test("delegation events use the same child identity across console.step and delegate.progress", () => {
    const complete = { ...event("step", 5), task_id: "child", step: { state: "done", answer: "finished" } };
    let state = events(createConversationProjection(A), complete);
    state = events(state, { ...event("step", 3), kind: "delegate.progress", step_id: "#child", step: { state: "running" } });
    assert.equal(state.delegations["#child"].step.state, "done");
});

test("recalled replies are tombstoned against late replay", () => {
    const line = event("notice", 2, { reply_id: "notice", text: "removed" });
    let state = events(createConversationProjection(A), line, event("recalled", 4, { reply_id: "notice" }));
    state = events(state, line, { ...line, n: 5 });
    assert.equal(state.entries.length, 0);
});

test("SSE invalidates only its actual HTTP domain and stale HTTP/error results cannot win", () => {
    let state = createConversationProjection(A), queueToken, repliesToken;
    [state, queueToken] = read(state, "queue");
    [state, repliesToken] = read(state, "replies");
    state = events(state, event("sent", 1, { exchange_id: "a", reply_id: "sent-a" }));
    assert.equal(acceptsConversationRead(state, queueToken), false);
    assert.equal(acceptsConversationRead(state, repliesToken), false);
    const newer = state;
    state = reduceConversation(state, { type: "queue", token: queueToken, exchanges: [] });
    state = reduceConversation(state, { type: "replies", token: repliesToken, enabled: true, entries: [] });
    state = reduceConversation(state, { type: "read-failed", token: queueToken, error: new Error("late") });
    assert.equal(state, newer);
    [state, queueToken] = read(state, "queue", 2);
    [state, repliesToken] = read(state, "replies", 2);
    state = events(state, event("progress", 2, { exchange_id: "a", progress: { answer: "retained" } }));
    assert.equal(acceptsConversationRead(state, queueToken), true);
    assert.equal(acceptsConversationRead(state, repliesToken), true);
    state = reduceConversation(state, { type: "queue", token: queueToken, exchanges: [exchange("a")] });
    assert.equal(state.live.turn.answer, "retained", "polling the same running exchange keeps its streamed answer");
});

test("latest requests win, including A→B→A switches with old HTTP and foreign events", () => {
    let state = createConversationProjection(A), old, current;
    [state, old] = read(state, "replies");
    [state, current] = read(state, "replies", 2);
    assert.equal(acceptsConversationRead(state, old), false);
    state = reduceConversation(state, { type: "select", conversation: B });
    assert.equal(state.loadingReplies, true);
    assert.equal(events(state, event("sent", 1, { exchange_id: "a" })), state);
    state = reduceConversation(state, { type: "select", conversation: A });
    [state] = read(state, "replies", 2);
    assert.equal(acceptsConversationRead(state, current), false);
    state = reduceConversation(state, { type: "replies", token: current, enabled: true, entries: [reply("late", "a", 1)] });
    assert.deepEqual(state.entries, []);
});

test("HTTP restores running turns, closes missed completions and preserves newer delegation progress", () => {
    let state = createConversationProjection(A), token;
    [state, token] = read(state, "queue");
    state = reduceConversation(state, { type: "queue", token, exchanges: [exchange("a")] });
    assert.equal(state.live.exchangeID, "a");
    [state, token] = read(state, "replies");
    state = events(state, { ...event("step", 3), task_id: "child", step: { state: "done", answer: "new" } });
    state = reduceConversation(state, { type: "replies", token, enabled: true, entries: [{
        ...reply("r", "a", 4), process: { steps: [{ id: "#child", kind: "delegate", state: "running" }] },
    }] });
    assert.equal(state.delegations["#child"].step.state, "done");
    assert.equal(state.live, null);
    assert.equal(state.exchanges[0].state, "done", "a recovered reply also releases busy/stop controls");
    [state, token] = read(state, "queue", 2);
    state = reduceConversation(state, { type: "queue", token, exchanges: [exchange("a")] });
    assert.equal(state.live, null, "a completed transcript beats an older queue read");
});

test("sent without an exchange ID still opens a turn and early progress is retained until sent arrives", () => {
    let state = events(createConversationProjection(A), event("sent", 1, { text: "command" }));
    assert.ok(state.live);
    state = events(createConversationProjection(A), event("progress", 3, { exchange_id: "early", progress: { answer: "arrived first" } }));
    state = events(state, event("sent", 2, { exchange_id: "early", text: "question" }));
    assert.equal(state.live.turn.answer, "arrived first");
});

test("context revision follows eligibility and project placement, not snapshot time or activity", () => {
    const snapshot = { at: at(1), hub: { node: "test" }, nodes: [{ name: "test", up: true }], agents: [{ id: "a", node: "test", harness: "test", eligible: true }], projects: [{ id: "p", node: "test", path: "/test", level: "public", repo: "inplace", agents: [], workspaces: [] }] };
    const revision = conversationContextRevision(snapshot);
    assert.equal(conversationContextRevision({ ...snapshot, at: at(9), tasks: [{ state: "running" }], nodes: [{ ...snapshot.nodes[0], health: { at: at(9), load1: 5 } }], agents: [{ ...snapshot.agents[0], busy: 1, activities: [{ at: at(9) }] }] }), revision);
    assert.notEqual(conversationContextRevision({ ...snapshot, agents: [{ ...snapshot.agents[0], eligible: false }] }), revision);
    assert.notEqual(conversationContextRevision({ ...snapshot, projects: [{ ...snapshot.projects[0], workspaces: [{ id: "w", node: "other", kind: "copy" }] }] }), revision);
});

function observe(state, resource, value) {
    let token;
    [state, token] = read(state, resource, state.requests[resource] + 1);
    return reduceConversation(state, { type: resource, token, ...value });
}

test("an owner-authorized same-ID retry stays live despite its retained old terminal receipt", () => {
    const failed = { ...exchange("stop", "failed"), input: "/cancel", key: "client:stop", reply_id: "failed-stop" };
    const receipt = reply("failed-stop", "stop", 3);
    let state = observe(createConversationProjection(A), "queue", { exchanges: [failed] });
    state = observe(state, "replies", { entries: [receipt], enabled: true });
    state = observe(state, "queue", { exchanges: [{ ...failed, state: "running" }] });
    assert.equal(state.exchanges[0].state, "running");
    assert.equal(state.live?.exchangeID, "stop");
    // A retry retains the old reply_id and started_at on the actual Go owner.
    // Neither a replayed receipt nor polling that history closes the new work.
    state = events(state, event("reply", 3, { exchange_id: "stop", reply_id: receipt.id, text: receipt.text }));
    state = observe(state, "replies", { entries: [receipt], enabled: true });
    assert.equal(state.exchanges[0].state, "running");
    assert.equal(state.live?.exchangeID, "stop");
    state = observe(state, "queue", { exchanges: [{ ...failed, state: "done", reply_id: "retry-done" }] });
    assert.equal(state.live, null);
    assert.equal(state.exchanges[0].state, "done");
});

test("RFC3339 fractional seconds and offsets order events without millisecond truncation", () => {
    let state = events(createConversationProjection(A), event("sent", 1, { exchange_id: "nano" }),
        event("progress", 2, { at: "2026-09-19T00:00:02.123900001Z", exchange_id: "nano", progress: { answer: "newer" } }));
    state = events(state, event("progress", 3, { at: "2026-09-19T08:00:02.123900000+08:00", exchange_id: "nano", progress: { answer: "older" } }));
    assert.equal(state.live.turn.answer, "newer");
    state = events(state, event("sent", 4, { at: "2026-09-19T00:00:02.123900002Z", exchange_id: "next" }),
        event("sent", 5, { at: "2026-09-19T00:00:02.123900001Z", exchange_id: "late" }));
    assert.equal(state.live.exchangeID, "next");
});

test("long sessions retire bookkeeping without reviving recalled lines or completed turns", () => {
    let state = createConversationProjection(A);
    const stamp = (n) => new Date(Date.UTC(2026, 8, 19) + n).toISOString();
    let firstLine, firstSent, firstDone;
    for (let i = 0; i < 5000; i++) {
        const line = event("notice", i * 4 + 1, { at: stamp(i * 4), reply_id: `n-${i}`, text: "notice" });
        const sent = event("sent", i * 4 + 2, { at: stamp(i * 4 + 1), exchange_id: `e-${i}`, reply_id: `s-${i}` });
        const done = event("reply", i * 4 + 3, { at: stamp(i * 4 + 2), exchange_id: `e-${i}`, reply_id: `r-${i}`, text: "done" });
        firstLine ??= line; firstSent ??= sent; firstDone ??= done;
        state = events(state, line, sent, done,
            event("recalled", i * 4 + 4, { at: stamp(i * 4 + 3), reply_id: line.reply_id }));
        state = observe(state, "queue", { exchanges: [] });
        state = observe(state, "replies", { entries: [], enabled: true });
        if (i % 250 === 249) {
            for (const key of ["completed", "recalled", "versions", "pendingProgress"]) {
                assert.ok(state[key].size <= 400, `${key} grew with history: ${state[key].size} at ${i + 1} turns`);
            }
        }
    }
    const counts = ["completed", "recalled", "versions", "pendingProgress"].map((key) => state[key].size);
    state = events(state, { ...firstLine, n: 30000 }, { ...firstSent, n: 30001 }, { ...firstDone, n: 30002 });
    assert.equal(state.entries.length, 0, "retired/recalled history must not reappear with a new arrival cursor");
    assert.equal(state.live, null, "retired terminal exchange must not reopen");
    assert.deepEqual(["completed", "recalled", "versions", "pendingProgress"].map((key) => state[key].size), counts);
    // A delayed event for an independent retained entity must still fold.
    state = events(state, event("sent", 30003, { at: stamp(30000), exchange_id: "active" }),
        event("progress", 30004, { at: stamp(30002), exchange_id: "active", progress: { answer: "current" } }),
        { ...event("step", 30005), at: stamp(30001), task_id: "independent", step: { state: "running" } });
    assert.equal(state.live.turn.answer, "current");
    assert.equal(state.delegations["#independent"].step.state, "running");
});

test("HTTP-only completion releases orphan progress retained before its sent event", () => {
    let state = events(createConversationProjection(A),
        event("progress", 1, { exchange_id: "missed", progress: { answer: "before sent" } }));
    assert.equal(state.pendingProgress.size, 1);
    state = observe(state, "queue", { exchanges: [exchange("missed", "done")] });
    assert.equal(state.pendingProgress.size, 0);
    assert.equal(state.versions.has("console.progress:missed"), false);
    state = events(createConversationProjection(A), event("progress", 1, { exchange_id: "missed", progress: { answer: "before sent" } }));
    state = observe(state, "replies", { entries: [reply("missed-done", "missed", 2)], enabled: true });
    assert.equal(state.pendingProgress.size, 0);
    assert.equal(state.versions.has("console.progress:missed"), false);
});

test("orphan overflow requests owner repair while preserving the current active progress", () => {
    let state = events(createConversationProjection(A), event("sent", 1, { exchange_id: "active" }),
        event("progress", 2, { exchange_id: "active", progress: { answer: "still current" } }));
    for (let n = 3; n < 1003; n++) state = events(state, event("progress", n, {
        at: new Date(Date.UTC(2026, 8, 19) + n * 1000).toISOString(), exchange_id: `orphan-${n}`, progress: { answer: "orphan" },
    }));
    assert.ok(state.repairs > 0, "eviction must be reconciled, not silently forgotten");
    assert.ok(state.pendingProgress.size <= 400);
    assert.equal(state.live.turn.answer, "still current");
});

test("SSE-only operation keeps bounded terminal bookkeeping even while owner reads fail", () => {
    let state = createConversationProjection(A);
    const stamp = (n) => new Date(Date.UTC(2026, 8, 19) + n).toISOString();
    for (let i = 0; i < 1000; i++) {
        state = events(state,
            event("sent", 2 * i + 1, { at: stamp(i * 2), exchange_id: `offline-${i}`, reply_id: `sent-${i}` }),
            event("reply", 2 * i + 2, { at: stamp(i * 2 + 1), exchange_id: `offline-${i}`, reply_id: `done-${i}`, text: `done ${i}` }));
    }
    let token;
    [state, token] = read(state, "queue");
    const entries = state.entries;
    state = reduceConversation(state, { type: "read-failed", token, error: new Error("owner offline") });
    assert.equal(state.entries, entries, "read error preserves the visible transcript");
    assert.equal(state.entries.length, 200);
    assert.equal(state.exchanges.length, 200, "match the owner's 200 terminal receipts; never trim queued/running rows");
    assert.ok(state.completed.size <= 400);
    assert.ok(state.versions.size <= 400);
    state = events(state, event("sent", 2001, { at: stamp(0), exchange_id: "offline-0", reply_id: "sent-0" }));
    assert.equal(state.live, null);
});

test("retiring HTTP-only transcript evidence cannot reopen the completed turn via old SSE", () => {
    let state = observe(createConversationProjection(A), "replies", { entries: [reply("http-done", "http-turn", 5)], enabled: true });
    state = observe(state, "replies", { entries: [], enabled: true });
    state = events(state, event("sent", 10, { at: at(2), exchange_id: "http-turn", reply_id: "http-sent" }));
    assert.equal(state.live, null);
    assert.equal(state.entries.length, 0);
    assert.ok(state.repairs > 0);
});

test("an active exchange does not authorize replay of its retired recalled lines", () => {
    const line = event("notice", 2, { exchange_id: "active", reply_id: "removed", text: "must stay removed" });
    let state = events(createConversationProjection(A), event("sent", 1, { exchange_id: "active" }),
        line, event("recalled", 3, { reply_id: line.reply_id }));
    for (let i = 0; i < 500; i++) state = events(state, event("notice", i + 4, {
        at: new Date(Date.UTC(2026, 8, 19, 0, 1) + i).toISOString(), reply_id: `other-${i}`, text: "other",
    }));
    state = observe(state, "replies", { entries: [], enabled: true });
    state = events(state, { ...line, n: 1000 });
    assert.equal(state.entries.some((r) => r.id === line.reply_id), false);
    assert.equal(state.live.exchangeID, "active");
});

test("HTTP retirement also retires version keys of old milestone payloads", () => {
    const latest = event("milestone", 1, { reply_id: "milestone", text: "last payload" });
    let state = events(createConversationProjection(A), latest);
    state = observe(state, "replies", { entries: [], enabled: true });
    state = events(state, { ...latest, n: 10, text: "older payload, same original timestamp" });
    assert.equal(state.entries.length, 0);
    assert.ok(state.repairs > 0);
});

test("streaming progress preserves unrelated projection identities", () => {
    const previous = events(createConversationProjection(A), event("sent", 1, { exchange_id: "a" }));
    const next = events(previous, event("progress", 2, { exchange_id: "a", progress: { answer: "stream" } }));
    assert.equal(next.entries, previous.entries);
    assert.equal(next.exchanges, previous.exchanges, "composer queue memo must not be invalidated per fragment");
    assert.equal(next.delegations, previous.delegations);
});
