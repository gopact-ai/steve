import assert from "node:assert/strict";
import test from "node:test";
import { indexSessionWork, indexBoardWork, sessionWorkAttention } from "../src/lib/work-list-index.ts";

const task = (id, extra = {}) => ({ id, transport: "console", channel: "a", execution: "idle", attention: 0, ...extra });
const roots = (tasks, conversation) => tasks.filter((t) => t.transport === "console" && t.channel === conversation && !t.parent
    && (t.execution === "running" || (t.attention || 0) > 0 || !!t.plan_id || (t.origin || "").startsWith("schedule") || tasks.some((child) => child.parent === t.id)));

// taskAxes has already rolled local approvals/plan attention into ancestors.
// These are snapshot values, not independent counts to sum across all tasks.
for (const scenario of [
    { name: "one child approval", tasks: [task("root", { attention: 1 }), task("child", { parent: "root", attention: 1 })], expected: 1 },
    { name: "one parent and one child approval", tasks: [task("root", { attention: 2 }), task("child", { parent: "root", attention: 1 })], expected: 2 },
    { name: "three levels with local attention at every level", tasks: [
        task("root", { attention: 3 }), task("child", { parent: "root", attention: 2 }), task("grandchild", { parent: "child", attention: 1 }),
    ], expected: 3 },
    { name: "multiple roots with conversation and transport boundaries", tasks: [
        task("root", { attention: 1 }), task("child", { parent: "root", attention: 1 }),
        task("second", { attention: 2 }), task("second-child", { parent: "second", attention: 2 }),
        task("elsewhere", { channel: "b", attention: 7 }), task("external", { transport: "feishu", attention: 9 }),
        task("orphan", { parent: "missing", attention: 5 }),
    ], expected: 3 },
    { name: "rolled root without loaded descendants", tasks: [task("root", { attention: 4 })], expected: 4 },
    { name: "no work", tasks: [], expected: 0 },
    { name: "notable root without attention", tasks: [task("root", { plan_id: "p", attention: undefined })], expected: 0 },
]) {
    test(`session attention counts ${scenario.name} once`, () => {
        const before = structuredClone(scenario.tasks);
        const { rootsByConversation } = indexSessionWork(scenario.tasks);
        assert.equal(sessionWorkAttention(rootsByConversation.get("a") || []), scenario.expected);
        assert.deepEqual(scenario.tasks, before, "Counting must not mutate the snapshot");
    });
}

test("session attention clears when approvals are handled in the next snapshot", () => {
    const pending = [task("root", { attention: 2 }), task("child", { parent: "root", attention: 1 })];
    const count = (tasks) => sessionWorkAttention(indexSessionWork(tasks).rootsByConversation.get("a") || []);
    assert.equal(count(pending), 2);
    assert.equal(count(pending.map((item) => ({ ...item, attention: 0 }))), 0);
    assert.equal(count(pending), 2, "Earlier snapshots remain unchanged");
});

test("session work preserves root eligibility, channel boundaries and child order", () => {
    const tasks = [
        task("quiet"), task("running", { execution: "running" }), task("attention", { attention: 2 }),
        task("plan", { plan_id: "p" }), task("scheduled", { origin: "schedule:daily" }), task("parent"),
        task("child-first", { parent: "parent", channel: "elsewhere", transport: "slack" }),
        task("child-second", { parent: "parent" }), task("orphan", { parent: "missing", execution: "running" }),
        task("other-transport", { transport: "slack", execution: "running" }),
        task("other-channel", { channel: "b", execution: "running" }), task("empty-channel", { channel: "", execution: "running" }),
        task("absent-channel", { channel: undefined, execution: "running" }),
    ];
    const before = structuredClone(tasks);
    const index = indexSessionWork(tasks);
    for (const conversation of ["a", "b", "", "missing"]) {
        assert.deepEqual(index.rootsByConversation.get(conversation) || [], roots(tasks, conversation));
    }
    assert.deepEqual(index.childrenByParent.get("parent"), [tasks[6], tasks[7]], "A child from another transport still makes its parent notable");
    assert.deepEqual(index.childrenByParent.get("missing"), [tasks[8]]);
    assert.equal(index.rootsByConversation.get("a")[0], tasks[1], "Retain task identity");
    assert.deepEqual(tasks, before, "Indexing must not mutate the snapshot");
});

test("session indexing reads tasks linearly even when every quiet root needs a child check", () => {
    let reads = 0;
    const tasks = Array.from({ length: 1000 }, (_, i) => new Proxy(task(String(i), { channel: String(i) }), {
        get(value, key) { reads++; return value[key]; },
    }));
    const index = indexSessionWork(tasks);
    for (let i = 0; i < tasks.length; i++) assert.deepEqual(index.rootsByConversation.get(String(i)) || [], []);
    assert.ok(reads < tasks.length * 20, `Expected bounded reads per task, got ${reads}`);
});

test("session indexes match scan semantics across mixed task graphs", () => {
    let seed = 20260920;
    const random = () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed / 2 ** 32; };
    for (let sample = 0; sample < 100; sample++) {
        const tasks = Array.from({ length: 80 }, (_, i) => task(String(i), {
            transport: random() < 0.8 ? "console" : "slack", channel: String(Math.floor(random() * 10)),
            execution: random() < 0.2 ? "running" : "idle", attention: random() < 0.2 ? 1 : 0,
            plan_id: random() < 0.2 ? "plan" : undefined, origin: random() < 0.2 ? "schedule:daily" : "chat",
            parent: random() < 0.5 ? String(Math.floor(random() * 90)) : undefined,
        }));
        const index = indexSessionWork(tasks);
        for (let i = 0; i < 10; i++) assert.deepEqual(index.rootsByConversation.get(String(i)) || [], roots(tasks, String(i)));
        for (const task of tasks) assert.deepEqual(index.childrenByParent.get(task.id) || [], tasks.filter((child) => child.parent === task.id));
    }
});

test("board indexes retain the first plan and activity in snapshot order", () => {
    const plans = [{ id: "first", task_id: "t" }, { id: "second", task_id: "t" }, { id: "other", task_id: "u" }];
    const first = { task_id: "t", tool: "first" }, second = { task_id: "t", tool: "second" }, other = { task_id: "u", tool: "other" };
    const agents = [{ activities: [first, second, { tool: "unassigned" }] }, {}, { activities: [other, { task_id: "t", tool: "last" }] }];
    const before = structuredClone({ plans, agents });
    const index = indexBoardWork(plans, agents);
    for (const id of ["t", "u", "missing"]) {
        assert.equal(index.plansByTask.get(id), plans.find((plan) => plan.task_id === id));
        assert.equal(index.activitiesByTask.get(id), agents.flatMap((agent) => agent.activities || []).find((activity) => activity.task_id === id));
    }
    assert.deepEqual({ plans, agents }, before);
});

test("board indexes do not rescan plans or activities for each task", () => {
    let reads = 0;
    const counted = (value) => new Proxy(value, { get(item, key) { reads++; return item[key]; } });
    const plans = Array.from({ length: 1000 }, (_, i) => counted({ task_id: String(i) }));
    const activities = Array.from({ length: 1000 }, (_, i) => counted({ task_id: String(i) }));
    const index = indexBoardWork(plans, [{ activities }]);
    for (let i = 0; i < 1000; i++) {
        assert.equal(index.plansByTask.get(String(i)), plans[i]);
        assert.equal(index.activitiesByTask.get(String(i)), activities[i]);
    }
    assert.ok(reads < 10000, `Expected bounded index construction, got ${reads} reads`);
});
