import assert from "node:assert/strict";
import test from "node:test";
import { eventCursor } from "../src/lib/event-cursor.ts";

test("a reconnect resumes after the last event delivered", () => {
    const cursor = eventCursor();
    assert.equal(cursor.last(), "");
    assert.equal(cursor.accept("run1.1"), true);
    assert.equal(cursor.accept("run1.2"), true);
    assert.equal(cursor.last(), "run1.2");
});

test("an event replayed after a reconnect is delivered once", () => {
    const cursor = eventCursor();
    for (const id of ["run1.1", "run1.2", "run1.3"]) assert.equal(cursor.accept(id), true);
    // A server that could not resume replays its recent events again.
    assert.deepEqual(["run1.2", "run1.3", "run1.4"].map((id) => cursor.accept(id)), [false, false, true]);
    assert.equal(cursor.last(), "run1.4");
});

test("a restarted hub's events are new even with smaller numbers", () => {
    const cursor = eventCursor();
    cursor.accept("run1.40");
    assert.equal(cursor.accept("run2.1"), true);
    assert.equal(cursor.accept("run2.2"), true);
    assert.equal(cursor.accept("run1.41"), true);
    assert.equal(cursor.last(), "run1.41");
});

test("an event without an id is delivered and leaves the cursor alone", () => {
    const cursor = eventCursor();
    cursor.accept("run1.3");
    assert.equal(cursor.accept(""), true);
    assert.equal(cursor.accept(undefined), true);
    assert.equal(cursor.accept("garbage"), true);
    assert.equal(cursor.last(), "run1.3");
});
