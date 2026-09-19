import assert from "node:assert/strict";
import test from "node:test";
import { arrangeProjects, movedProjects } from "../../web/console/src/lib/project-order.ts";

const project = (id, extra = {}) => ({ id, node: "n", path: `/${id}`, level: "public", repo: "inplace", agents: [], workspaces: [], ...extra });

test("an unarranged list keeps the default project first and the rest by name", () => {
    const list = [project("redis"), project("env"), project("workspace", { default: true }), project("daily")];
    assert.deepEqual(arrangeProjects(list, []).map((p) => p.id), ["workspace", "daily", "env", "redis"]);
});

test("a project the reader moved leads, and one they never touched follows in its old order", () => {
    const list = [project("redis"), project("env"), project("workspace", { default: true }), project("daily")];
    assert.deepEqual(arrangeProjects(list, ["redis", "env"]).map((p) => p.id), ["redis", "env", "workspace", "daily"]);
    // A project that joins later is appended rather than sorted into the
    // arrangement, and an order naming a project that is gone is harmless.
    assert.deepEqual(arrangeProjects(list, ["daily", "gone", "redis"]).map((p) => p.id), ["daily", "redis", "workspace", "env"]);
    assert.deepEqual(arrangeProjects([], ["daily"]), []);
});

test("moving is the same step whether it came from a drag or a key", () => {
    const ids = ["a", "b", "c", "d"];
    assert.deepEqual(movedProjects(ids, "d", 0), ["d", "a", "b", "c"]);
    assert.deepEqual(movedProjects(ids, "a", 3), ["b", "c", "d", "a"]);
    assert.deepEqual(movedProjects(ids, "b", 2), ["a", "c", "b", "d"]);
    // Out of range, unknown and no-op moves leave the list as it was.
    assert.deepEqual(movedProjects(ids, "a", -1), ids);
    assert.deepEqual(movedProjects(ids, "a", 4), ids);
    assert.deepEqual(movedProjects(ids, "z", 1), ids);
    assert.deepEqual(movedProjects(ids, "c", 2), ids);
    assert.deepEqual(ids, ["a", "b", "c", "d"], "Reordering must not touch the list it was given");
});
