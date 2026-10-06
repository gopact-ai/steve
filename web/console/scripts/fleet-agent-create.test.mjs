import assert from "node:assert/strict";
import test from "node:test";
import { configuredHarnesses, createFleetAgent, bindFleetAgent, inspectFleetAgent, launchFingerprint, fleetWriteRejected } from "../src/lib/api/fleet-agent-create.ts";
const launch = { command: "/fixture/未知 agent", args: ["acp", "", " 含 空格 ", '"quote"', "$HOME", "a|b"], env: ["MODE=ordinary", "EMPTY=", "UNICODE=中文 空格"] };
function fixture() {
    const f = { settings: { revision: "r1", harnesses: { catalog: { adapter: "pinned-acp", command: "/fixture/pinned" }, unbound: launch }, tools: ["git"], declares: [], capabilities: ["test"], mcp_servers: {} }, agents: [], saved: [], posts: [], receipts: [], saveError: null, bindError: null, missingOK: false };
    const ports = {
        readSettings: async () => ({ settings: f.settings }),
        readAgents: async () => ({ hub: { node: "local" }, agents: f.agents }),
        saveSettings: async (node, settings) => { f.saved.push({ node, settings }); if (f.saveError) throw f.saveError; f.settings = { ...settings, revision: "r2" }; return { settings: f.settings }; },
        bind: async binding => { f.posts.push(binding); if (f.bindError) throw f.bindError; if (f.missingOK) return {}; f.agents.push(binding); return { ok: true }; },
        rejected: error => error.rejected === true,
    };
    const remember = receipt => { f.receipts.push(receipt); f.receipt = receipt; };
    return { f, ports, remember };
}
const binding = { id: "reviewer", harness: "my-acp", node: "remote" };
test("all configured harnesses, including unbound and catalog entries, are choices", () => {
    const { f } = fixture(); assert.deepEqual(configuredHarnesses(f.settings), ["catalog", "unbound"]);
});
test("custom launch is saved node-locally before binding without changing argv or unrelated settings", async () => {
    const { f, ports, remember } = fixture(); const before = structuredClone(f.settings);
    const result = await createFleetAgent(binding, f.settings, launch, ports, remember);
    assert.equal(result.phase, "bound"); assert.equal(result.revision, "r2");
    assert.deepEqual(f.saved, [{ node: "remote", settings: { ...before, harnesses: { ...before.harnesses, "my-acp": launch } } }]);
    assert.deepEqual(f.posts, [binding]);
    assert.deepEqual(f.receipts.map(r => r.phase), ["save-unknown", "saved", "bind-unknown", "bound"]);
    assert.equal(JSON.stringify(f.receipts).includes("UNICODE="), false, "receipts contain no raw env");
});
test("configured catalog launch binds without rewriting or editing its pinned arguments", async () => {
    const { f, ports, remember } = fixture(); await createFleetAgent({ ...binding, harness: "catalog" }, f.settings, undefined, ports, remember);
    assert.equal(f.saved.length, 0); assert.deepEqual(f.posts, [{ ...binding, harness: "catalog" }]);
    await assert.rejects(createFleetAgent({ ...binding, harness: "catalog" }, f.settings, launch, ports, remember), { code: "harnessExists" });
    assert.equal(f.posts.length, 1);
});
test("unknown settings transport never binds or repeats PUT; readback uses the exact launch fingerprint", async () => {
    const { f, ports, remember } = fixture(); f.saveError = new Error("connection reset");
    await assert.rejects(createFleetAgent(binding, f.settings, launch, ports, remember));
    assert.equal(f.receipt.phase, "save-unknown"); assert.equal(f.posts.length, 0);
    assert.equal((await inspectFleetAgent(f.receipt, ports)).phase, "save-unknown");
    f.settings = { ...f.settings, revision: "r2", harnesses: { ...f.settings.harnesses, "my-acp": launch } };
    const observed = await inspectFleetAgent(f.receipt, ports); assert.equal(observed.phase, "saved");
    assert.equal(f.saved.length, 1); assert.equal(f.posts.length, 0, "inspection alone never binds");
    await bindFleetAgent(observed, ports, remember); assert.deepEqual(f.posts, [binding]);
});
test("unknown binding retains original identity across readback and absence never permits a second POST", async () => {
    const { f, ports, remember } = fixture(); f.bindError = new Error("lost reply");
    await assert.rejects(createFleetAgent(binding, f.settings, launch, ports, remember));
    const pending = structuredClone(f.receipt); assert.equal(pending.phase, "bind-unknown");
    const observed = await inspectFleetAgent(pending, ports); assert.equal(observed.phase, "bind-unknown");
    await assert.rejects(bindFleetAgent(observed, ports, remember), { code: "invalidReceipt" });
    assert.equal(f.posts.length, 1); assert.equal(f.saved.length, 1);
    f.agents.push(binding); assert.equal((await inspectFleetAgent(pending, ports)).phase, "bound"); assert.equal(f.posts.length, 1);
});
test("a rejected bind retains the saved harness and explicitly retries only the same binding", async () => {
    const { f, ports, remember } = fixture(); f.bindError = Object.assign(new Error("bind rejected"), { rejected: true });
    await assert.rejects(createFleetAgent(binding, f.settings, launch, ports, remember));
    assert.equal(f.receipt.phase, "bind-rejected"); assert.equal(f.receipt.revision, "r2");
    f.settings = { ...f.settings, revision: "r3", capabilities: ["other-user-setting"] }; f.bindError = null;
    const observed = await inspectFleetAgent(f.receipt, ports); await bindFleetAgent(observed, ports, remember);
    assert.deepEqual(f.posts, [binding, binding]); assert.equal(f.saved.length, 1); assert.deepEqual(f.settings.capabilities, ["other-user-setting"]);
});
test("server rejection of duplicate env leaves revision and values intact until explicit readback", async () => {
    const { f, ports, remember } = fixture(); const duplicate = { ...launch, env: ["MODE=one", "MODE=two"] };
    f.saveError = Object.assign(new Error('duplicate environment key "MODE"'), { rejected: true });
    await assert.rejects(createFleetAgent(binding, f.settings, duplicate, ports, remember), /duplicate/);
    assert.deepEqual(f.saved[0].settings.harnesses["my-acp"].env, duplicate.env);
    assert.equal(f.receipt.revision, "r1"); assert.equal(await inspectFleetAgent(f.receipt, ports), null); assert.equal(f.posts.length, 0);
});
test("changed launch or conflicting binding never authorizes another mutation", async () => {
    const { f, ports, remember } = fixture(); f.bindError = Object.assign(new Error("rejected"), { rejected: true });
    await assert.rejects(createFleetAgent(binding, f.settings, launch, ports, remember));
    f.settings.harnesses["my-acp"] = { ...launch, args: ["changed"] };
    await assert.rejects(inspectFleetAgent(f.receipt, ports), { code: "configurationChanged" });
    f.agents.push({ ...binding, node: "somewhere-else" }); await assert.rejects(inspectFleetAgent(f.receipt, ports), { code: "bindingConflict" });
    assert.equal(f.saved.length, 1); assert.equal(f.posts.length, 1);
});
test("malformed success is unknown and storage failure prevents dispatch", async () => {
    const { f, ports, remember } = fixture(); f.missingOK = true;
    await assert.rejects(createFleetAgent(binding, f.settings, launch, ports, remember), { code: "invalidReceipt" }); assert.equal(f.receipt.phase, "bind-unknown");
    const next = fixture(); await assert.rejects(createFleetAgent(binding, next.f.settings, launch, next.ports, () => { throw new Error("storage unavailable"); }), /storage/);
    assert.equal(next.f.saved.length, 0); assert.equal(next.f.posts.length, 0);
});
test("fingerprints distinguish exact Unicode, spaces and empty args; missing arrays equal empty ones", async () => {
    assert.notEqual(await launchFingerprint(launch), await launchFingerprint({ ...launch, args: launch.args.filter(Boolean) }));
    assert.notEqual(await launchFingerprint(launch), await launchFingerprint({ ...launch, args: launch.args.map(s => s.trim()) }));
    assert.equal(await launchFingerprint({ command: "fixture" }), await launchFingerprint({ command: "fixture", args: [], env: [], adapter: "", process_dir: "" }));
});

test("legacy 400 transport and uncertain durability failures are unknown, not retry permission", () => {
    assert.equal(fleetWriteRejected(400, "node connection reset", "bind"), false);
    assert.equal(fleetWriteRejected(400, 'configuration applied; directory sync failed (durability uncertain): duplicate environment key "MODE"', "save"), false);
    assert.equal(fleetWriteRejected(400, "configuration applied; directory sync failed (durability uncertain): fixture", "save"), false);
    assert.equal(fleetWriteRejected(400, "configuration applied; directory sync failed (durability uncertain): fixture", "bind"), false);
    assert.equal(fleetWriteRejected(400, 'harness "my-acp": duplicate environment key "MODE"', "save"), true);
    assert.equal(fleetWriteRejected(409, "revision changed", "save"), true);
    assert.equal(fleetWriteRejected(409, "unknown", "bind"), false);
    assert.equal(fleetWriteRejected(403, "forbidden", "bind"), true);
    for (const status of [408, 500, 502, 503, 504]) assert.equal(fleetWriteRejected(status, "fixture", "save"), false);
});


test("configuration changes between choice / save and binding prevent POST", async () => {
    const { f, ports, remember } = fixture(); const old = structuredClone(f.settings);
    f.settings.harnesses.unbound = { command: "/fixture/replaced" };
    await assert.rejects(createFleetAgent({ ...binding, harness: "unbound" }, old, undefined, ports, remember), { code: "configurationChanged" });
    assert.equal(f.posts.length, 0); assert.equal(f.saved.length, 0); assert.equal(f.receipt.phase, "saved");
});
test("a definitive settings CAS rejection allows a fresh draft, not stale settings rollback", async () => {
    const { f, ports, remember } = fixture(); const before = f.settings;
    f.saveError = Object.assign(new Error("CAS rejected"), { rejected: true });
    await assert.rejects(createFleetAgent(binding, before, launch, ports, remember));
    f.settings = { ...before, revision: "r2", capabilities: ["another-change"] };
    assert.equal(await inspectFleetAgent(f.receipt, ports), null); assert.equal(f.posts.length, 0);
    assert.deepEqual(f.settings.capabilities, ["another-change"]);
});
