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
