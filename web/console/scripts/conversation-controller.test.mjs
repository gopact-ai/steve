import assert from "node:assert/strict";
import test from "node:test";
import { ConversationController } from "../src/lib/conversation-controller.ts";

const A = "console:a";
const at = "2026-09-19T00:00:00Z";
const sent = (id) => ({ kind: "console.sent", conversation: A, at, exchange_id: id, reply_id: `sent-${id}` });
const done = (id) => ({ kind: "console.reply", conversation: A, at, exchange_id: id, reply_id: `reply-${id}`, text: "finished" });
const progress = (id) => ({ kind: "console.progress", conversation: A, at, exchange_id: id, progress: { answer: id } });
const queue = (id, state = "running") => ({ queue: [{ id, state, conversation: A, input: id, enqueued_at: at, started_at: at }] });
const deferred = () => Promise.withResolvers();
function fixture(overrides = {}) {
    return new ConversationController(A, {
        fetchReplies: async () => ({ enabled: true, replies: [] }),
        fetchQueue: async () => ({ queue: [] }),
        reconcileSubmission: async () => {},
        ...overrides,
    });
}

for (const phase of ["HTTP", "reconciliation"]) test(`terminal SSE wins over a queue read held in ${phase}`, async () => {
    const held = deferred(), entered = deferred();
    let first = true;
    const controller = fixture({
        fetchQueue: async () => {
            if (phase === "HTTP" && first) { first = false; entered.resolve(); await held.promise; return queue("a"); }
            return first ? queue("a") : { queue: [] };
        },
        reconcileSubmission: async () => {
            if (phase === "reconciliation" && first) { first = false; entered.resolve(); await held.promise; }
        },
        fetchReplies: async () => ({ enabled: true, replies: [{ id: "reply-a", kind: "reply", conversation: A, exchange_id: "a", at, text: "finished" }] }),
    });
    const load = controller.loadQueue();
    await entered.promise;
    controller.receive([done("a")]);
    held.resolve();
    await load;
    await controller.reload();
    controller.receive([progress("a")]);
    assert.equal(controller.getSnapshot().live, null);
    assert.equal(controller.getSnapshot().queueError, null);
    controller.dispose();
});

test("an older concurrent HTTP completion cannot overwrite the latest read", async () => {
    const old = deferred();
    let count = 0;
    const controller = fixture({ fetchReplies: () => ++count === 1 ? old.promise : Promise.resolve({ enabled: true, replies: [{ id: "new", kind: "notice", conversation: A, at, text: "new" }] }) });
    const first = controller.loadReplies();
    await controller.loadReplies();
    old.resolve({ enabled: true, replies: [] });
    await first;
    assert.equal(controller.getSnapshot().entries[0].id, "new");
    controller.dispose();
});

test("disposing during a switch ignores both success and error, even after reactivation", async () => {
    const held = deferred(), errors = deferred();
    let signal;
    const controller = fixture({
        fetchQueue: (_id, s) => { signal = s; return held.promise; },
        fetchReplies: () => errors.promise,
    });
    const reads = Promise.all([controller.loadQueue(), controller.loadReplies()]);
    controller.dispose();
    assert.equal(signal.aborted, true);
    controller.activate();
    const current = controller.getSnapshot();
    held.resolve(queue("old"));
    errors.reject(new Error("old failure"));
    await reads;
    assert.equal(controller.getSnapshot(), current);
    controller.dispose();
});

test("only domain-changing events request HTTP; progress does not cause polling per fragment", async () => {
    let queues = 0, replies = 0;
    const controller = fixture({
        fetchQueue: async () => { queues++; return queue("a"); },
        fetchReplies: async () => { replies++; return { enabled: true, replies: [] }; },
    });
    controller.receive([sent("a")]);
    await controller.reload();
    const reads = [queues, replies];
    for (let n = 1; n <= 12; n++) controller.receive([{ ...progress("a"), n, progress: { answer: String(n) } }]);
    assert.deepEqual([queues, replies], reads);
    assert.equal(controller.getSnapshot().live.turn.answer, "12");
    controller.receive([{ ...done("b"), conversation: "console:b" }]);
    assert.deepEqual([queues, replies], reads);
    controller.dispose();
});

test("retired replay repairs from the owner instead of resurrecting or hiding independent late state", async () => {
    let queues = 0, replies = 0;
    const owner = { enabled: true, replies: [] };
    const controller = fixture({
        fetchQueue: async () => { queues++; return { queue: [] }; },
        fetchReplies: async () => { replies++; return owner; },
    });
    const initial = { kind: "console.notice", conversation: A, reply_id: "recalled", text: "gone", at };
    controller.receive([initial, { ...initial, kind: "console.recalled" }]);
    for (let i = 1; i <= 1000; i++) controller.receive([{
        ...initial, reply_id: `n${i}`, at: new Date(Date.UTC(2026, 8, 19) + i).toISOString(),
    }]);
    await controller.reload();
    const before = [queues, replies];
    owner.replies = [{ id: "current", conversation: A, kind: "reply", at, text: "owner current",
        process: { steps: [{ id: "#independent", kind: "delegate", state: "done", answer: "late independent result" }] } }];
    controller.receive([{ ...initial, n: 2000 }, {
        kind: "console.step", conversation: A, at, n: 2001, task_id: "independent", step: { state: "done" },
    }]);
    await new Promise((resolve) => setImmediate(resolve));
    assert.ok(queues > before[0] && replies > before[1], "outside retained evidence must reread both owner resources");
    assert.equal(controller.getSnapshot().entries.some((r) => r.id === "recalled"), false);
    assert.equal(controller.getSnapshot().entries[0].text, "owner current");
    assert.equal(controller.getSnapshot().delegations["#independent"].step.answer, "late independent result");
    controller.dispose();
});

test("retry completion comes from a new owner queue observation, not an old reply SSE or HTTP", async () => {
    const old = { id: "failed-stop", kind: "reply", conversation: A, exchange_id: "stop", at, text: "failed" };
    let row = { ...queue("stop", "failed").queue[0], reply_id: old.id, key: "client:stop", input: "/cancel" };
    const controller = fixture({
        fetchQueue: async () => ({ queue: [row] }),
        fetchReplies: async () => ({ enabled: true, replies: [old] }),
    });
    await controller.reload();
    row = { ...row, state: "running" };
    await controller.reload();
    controller.receive([{ ...old, kind: "console.reply" }]);
    await controller.reload();
    assert.equal(controller.getSnapshot().live?.exchangeID, "stop");
    row = { ...row, state: "done", reply_id: "new-receipt" };
    controller.receive([{ kind: "console.reply", conversation: A, at, exchange_id: "stop", reply_id: row.reply_id }]);
    await controller.reload();
    assert.equal(controller.getSnapshot().live, null);
    controller.dispose();
});
