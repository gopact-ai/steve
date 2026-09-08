import assert from "node:assert/strict";
import test from "node:test";
import { applyActivity, withLiveActivity } from "../../web/console/src/lib/live.ts";

const at = (n) => `2026-09-08T00:00:${String(n).padStart(2, "0")}Z`;

test("streamed progress becomes the agent's live activity, last tool call first", () => {
    let live = {};
    live = applyActivity(live, { kind: "console.progress", at: at(1), task_id: "12", progress: { agent: "codex", tools: [{ id: "t1", kind: "read", name: "Read go.mod", status: "completed" }, { id: "t2", kind: "shell", name: "go test ./...", status: "running" }] } });
    assert.deepEqual(live.codex, { agent: "codex", taskID: "12", tool: "shell", detail: "go test ./...", at: at(1) });
    // Events without an agent, or of other kinds, do not count.
    assert.equal(applyActivity(live, { kind: "console.reply", at: at(2), progress: { agent: "codex" } }), live);
    assert.equal(applyActivity(live, { kind: "console.progress", at: at(2), progress: {} }), live);
});

test("the live activity overlays the snapshot's only when it is newer", () => {
    const recorded = [{ agent: "codex", attempt_id: "att-1", kind: "turn", task_id: "12", tool: "read", detail: "Read go.mod", since: at(0), at: at(1) }];
    const newer = { agent: "codex", taskID: "12", tool: "shell", detail: "go test ./...", at: at(5) };
    assert.deepEqual(withLiveActivity(recorded, newer), [{ ...recorded[0], tool: "shell", detail: "go test ./...", at: at(5) }]);
    const older = { ...newer, at: at(0) };
    assert.deepEqual(withLiveActivity(recorded, older), recorded);
    assert.deepEqual(withLiveActivity(recorded, undefined), recorded);
    // A task the snapshot has not caught up with is shown, not "idle".
    const [shown] = withLiveActivity([], newer);
    assert.equal(shown.task_id, "12");
    assert.equal(shown.tool, "shell");
    // Another task's recorded activity is left alone.
    const other = [{ ...recorded[0], task_id: "13" }];
    assert.deepEqual(withLiveActivity(other, newer), other);
});
