import assert from "node:assert/strict";
import test from "node:test";
import { mergeChannelHistory, revalidateChannelHistory } from "../src/lib/channel-history.ts";
import { conversationExecution, conversationKey, conversationURL } from "../src/lib/conversation-identity.ts";
import { taskConversationAddress, consoleTaskConversation } from "../src/lib/task-transport.ts";

const r = (id, text = id) => ({ id, text, kind: "reply", at: "2026-09-21T00:00:00Z", conversation: "raw" });
test("transport is identity, never an ID prefix, and URL preserves raw ID", () => {
    const id = "console:opaque/a ?中";
    assert.notEqual(conversationKey({ id, transport: "feishu" }), conversationKey({ id }));
    assert.equal(conversationKey({ id }), conversationKey({ id, transport: "console" }));
    assert.equal(conversationURL(id, "feishu"), `/console?conversation=${encodeURIComponent(id)}&transport=feishu`);
    assert.equal(consoleTaskConversation({ channel: id, transport: "feishu" }), null);
    assert.deepEqual(taskConversationAddress({ channel: id, transport: "feishu" }), { id, transport: "feishu" });
    assert.equal(taskConversationAddress({ channel: id }), null);
});
test("old pages are deduplicated; refresh preserves older history and latest receipts", () => {
    const held = [r("b"), r("c", "pending")];
    const old = mergeChannelHistory(held, [r("a"), r("b")], true);
    assert.deepEqual(old.replies.map(x => x.id), ["a", "b", "c"]);
    const next = mergeChannelHistory(old.replies, [r("c", "final"), r("d")]);
    assert.deepEqual(next, { replies: [r("a"), r("b"), r("c", "final"), r("d")], reset: false });
});
test("late answer is inserted in server order even when timestamps tie", () => {
    const next = mergeChannelHistory([r("old"), r("sent1"), r("sent2")], [r("sent1"), r("answer1"), r("sent2"), r("answer2")]);
    assert.deepEqual(next.replies.map(x => x.id), ["old", "sent1", "answer1", "sent2", "answer2"]);
});
test("an advanced latest window resets explicitly instead of hiding a history gap", () => {
    assert.deepEqual(mergeChannelHistory([r("a")], [r("c")]), { replies: [r("c")], reset: true });
    assert.deepEqual(mergeChannelHistory([r("a")], []), { replies: [], reset: true });
});

test("late receipt stays inside the authoritative complete turn before the next input", () => {
    const a = { ...r("a-sent"), exchange_id: "a", kind: "sent" };
    const b = { ...r("b-sent"), exchange_id: "b", kind: "sent" };
    const answer = { ...r("a-reply"), exchange_id: "a" };
    const old = { ...r("old"), exchange_id: "old" };
    assert.deepEqual(mergeChannelHistory([old, a, b], [a, answer, b]), { replies: [old, a, answer, b], reset: false });
});

test("channel unknown execution outranks the compatibility running flag", () => {
    assert.equal(conversationExecution({ execution: "unknown", running: true }), "unknown");
    assert.equal(conversationExecution({ running: true }), "running");
    assert.equal(conversationExecution({ running: false }), "idle");
});

const turn = (id, text, delivery = "unconfirmed") => [
    { ...r(`${id}-sent`), exchange_id: id, kind: "sent" },
    ...(text === undefined ? [] : [{ ...r(`${id}-reply`, text), exchange_id: id, delivery }]),
];

test("a newer earlier page replaces overlapping complete turns, including late answers", () => {
    const held = [...turn("b"), ...turn("c", "latest")];
    const page = [...turn("a", "older"), ...turn("b", "late", "confirmed")];
    assert.deepEqual(mergeChannelHistory(held, page, true).replies, [...page, ...turn("c", "latest")]);
});

test("revalidation updates loaded turns in place without importing unrequested older history", () => {
    const held = [...turn("b"), ...turn("c", "pending"), ...turn("d", "latest")];
    const page = [...turn("a", "not loaded"), ...turn("b", "late"), ...turn("c", "final", "confirmed")];
    assert.deepEqual(revalidateChannelHistory(held, page), [
        ...turn("b", "late"), ...turn("c", "final", "confirmed"), ...turn("d", "latest"),
    ]);
    assert.deepEqual(revalidateChannelHistory(held, []), held);
});

test("revalidation uses complete server turns, not timestamps or individual reply append order", () => {
    const held = [...turn("b", "obsolete"), ...turn("c", "pending"), ...turn("d", "latest")];
    assert.deepEqual(revalidateChannelHistory(held, [...turn("b"), ...turn("c", "final", "suppressed")]), [
        ...turn("b"), ...turn("c", "final", "suppressed"), ...turn("d", "latest"),
    ]);
});
