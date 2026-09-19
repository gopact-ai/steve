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
