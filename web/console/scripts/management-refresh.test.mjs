import assert from "node:assert/strict";
import test from "node:test";
import { managementDependencyKey, managementEventAffects } from "../src/lib/management-refresh.ts";

const snapshot = () => ({
    at: "first", hub: { node: "hub", started: "boot-1" },
    nodes: [{ name: "node", up: true, features: ["mcp-probe", "skills"], health: { at: "first" },
        snapshot: { sequence: 1, generated_at: "first", offers: [
            { kind: "mcp", id: "search", availability: "available", evidence: [{ ok: true, at: "first" }] },
            { kind: "skill", id: "review", availability: "available" },
        ] } }],
    agents: [{ id: "writer", node: "node", harness: "test", mcp_servers: ["search"] }],
    projects: [{ id: "p", node: "node", path: "/fixture" }],
});

test("management dependencies ignore observation clocks, traffic and unrelated capabilities", () => {
    const before = snapshot(), after = structuredClone(before);
    after.at = "later";
    after.nodes[0].health.at = "later";
    Object.assign(after.nodes[0].snapshot, { sequence: 99, generated_at: "later", digest: "unrelated" });
    after.nodes[0].snapshot.offers[0].evidence[0].at = "later";
    after.nodes[0].snapshot.offers.push({ kind: "tool", id: "git", availability: "available" });
    after.agents[0].busy = 1;
    after.agents[0].activities = [{ at: "later" }];
    after.tasks = [{ id: "new-task" }];
    for (const domain of ["skills", "mcp", "plugins"]) {
        assert.equal(managementDependencyKey(domain, after), managementDependencyKey(domain, before), domain);
    }
});

test("actual domain changes invalidate only their consumers", () => {
    const before = snapshot(), after = structuredClone(before);
    after.nodes[0].snapshot.offers[0].availability = "unavailable";
    assert.notEqual(managementDependencyKey("mcp", after), managementDependencyKey("mcp", before));
    assert.equal(managementDependencyKey("skills", after), managementDependencyKey("skills", before));
    assert.equal(managementDependencyKey("plugins", after), managementDependencyKey("plugins", before));
    after.nodes[0].up = false;
    for (const domain of ["skills", "mcp", "plugins"]) assert.notEqual(managementDependencyKey(domain, after), managementDependencyKey(domain, before));
    const reboot = structuredClone(before);
    reboot.hub.started = "boot-2";
    assert.notEqual(managementDependencyKey("plugins", reboot), managementDependencyKey("plugins", before));
});

test("unordered domain collections do not create false revisions", () => {
    const before = snapshot(), after = structuredClone(before);
    after.nodes[0].features.reverse();
    after.nodes[0].snapshot.offers.reverse();
    for (const domain of ["skills", "mcp", "plugins"]) assert.equal(managementDependencyKey(domain, after), managementDependencyKey(domain, before));
});

test("only existing owner events invalidate management reads", () => {
    assert.equal(managementEventAffects("skills", { kind: "observe.node.skills" }), true);
    assert.equal(managementEventAffects("mcp", { kind: "observe.node.skills" }), false);
    const manifest = { kind: "observe.node.manifest", data: { changes: "mcp:search\tavailable\tunavailable" } };
    assert.equal(managementEventAffects("mcp", manifest), true);
    assert.equal(managementEventAffects("skills", manifest), false);
    assert.equal(managementEventAffects("plugins", manifest), false);
    for (const kind of ["console.progress", "observe.task.idle", "task.done", "observe.node.manifest"]) {
        assert.equal(managementEventAffects("skills", { kind, data: { changes: "tool:git\t\tavailable" } }), false);
    }
});
