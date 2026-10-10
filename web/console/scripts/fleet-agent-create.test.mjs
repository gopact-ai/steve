import assert from "node:assert/strict";
import test from "node:test";
import { permissionConfirmationRejected } from "../src/lib/agent-permission.ts";
import { canCancelFleetAgentBinding, configuredHarnesses, createFleetAgent as prepareFleetAgent, bindFleetAgent as submitFleetAgent, readFleetAgentPermission, inspectFleetAgent, launchFingerprint, fleetWriteRejected } from "../src/lib/api/fleet-agent-create.ts";
const policy = (node, harness, permission = "read", source = "default_read", revision = "policy-fact-r1") => ({ node, harness, permission, source, revision });
const assertion = { expected_permission: "read", expected_permission_revision: "policy-fact-r1" };
async function bindFleetAgent(receipt, ports, remember) { return submitFleetAgent(receipt, ports, remember, await readFleetAgentPermission(receipt, ports)); }
async function createFleetAgent(binding, settings, custom, ports, remember) { return bindFleetAgent(await prepareFleetAgent(binding, settings, custom, ports, remember), ports, remember); }
const launch = { command: "/fixture/未知 agent", args: ["acp", "", " 含 空格 ", '"quote"', "$HOME", "a|b"], env: ["MODE=ordinary", "EMPTY=", "UNICODE=中文 空格"] };
function fixture() {
    const f = { settings: { revision: "r1", harnesses: { catalog: { adapter: "pinned-acp", command: "/fixture/pinned" }, unbound: launch }, tools: ["git"], declares: [], capabilities: ["test"], mcp_servers: {} }, agents: [], saved: [], posts: [], receipts: [], saveError: null, bindError: null, missingOK: false };
    const ports = {
        readSettings: async () => ({ settings: f.settings }),
        readAgents: async () => ({ hub: { node: "local" }, agents: f.agents }),
        saveSettings: async (node, settings) => { f.saved.push({ node, settings }); if (f.saveError) throw f.saveError; f.settings = { ...settings, revision: "r2" }; return { settings: f.settings }; },
        readPermission: async (node, harness) => policy(node, harness),
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
    assert.deepEqual(f.posts, [{ ...binding, ...assertion }]);
    assert.deepEqual(f.receipts.map(r => r.phase), ["save-unknown", "saved", "bind-unknown", "bound"]);
    assert.equal(JSON.stringify(f.receipts).includes("UNICODE="), false, "receipts contain no raw env");
});
test("configured catalog launch binds without rewriting or editing its pinned arguments", async () => {
    const { f, ports, remember } = fixture(); await createFleetAgent({ ...binding, harness: "catalog" }, f.settings, undefined, ports, remember);
    assert.equal(f.saved.length, 0); assert.deepEqual(f.posts, [{ ...binding, harness: "catalog", ...assertion }]);
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
    await bindFleetAgent(observed, ports, remember); assert.deepEqual(f.posts, [{ ...binding, ...assertion }]);
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
    assert.deepEqual(f.posts, [{ ...binding, ...assertion }, { ...binding, ...assertion }]); assert.equal(f.saved.length, 1); assert.deepEqual(f.settings.capabilities, ["other-user-setting"]);
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

for (const scenario of ["matching launch", "changed launch", "missing harness", "save unknown with old binding"]) {
    test(`read-only matching Agent reconciliation verifies the original launch: ${scenario}`, async () => {
        const { f, ports } = fixture();
        const receipt = { ...binding, revision: "r1", launch: await launchFingerprint(launch), phase: scenario === "save unknown with old binding" ? "save-unknown" : "bind-unknown" };
        const original = structuredClone(receipt);
        f.agents.push({ ...binding });
        f.settings = { ...f.settings, revision: "r2", harnesses: { ...f.settings.harnesses, [binding.harness]: launch } };
        if (scenario === "changed launch") f.settings.harnesses[binding.harness] = { ...launch, args: ["replaced"] };
        if (scenario === "missing harness") delete f.settings.harnesses[binding.harness];
        if (scenario === "save unknown with old binding") f.settings.harnesses[binding.harness] = { command: "/fixture/old-agent", args: ["old-acp"] };
        const calls = [];
        const readOnly = { ...ports,
            readSettings: async node => { calls.push(["settings", node]); return ports.readSettings(node); },
            readAgents: async () => { calls.push(["agents"]); return ports.readAgents(); },
            saveSettings: async () => { assert.fail("read-only inspection must not PUT"); },
            bind: async () => { assert.fail("read-only inspection must not POST"); },
        };
        if (scenario === "matching launch") {
            const observed = await inspectFleetAgent(receipt, readOnly);
            assert.equal(observed.phase, "bound"); assert.equal(observed.revision, "r2"); assert.equal(observed.launch, original.launch);
        } else {
            await assert.rejects(inspectFleetAgent(receipt, readOnly), { code: "configurationChanged" });
        }
        assert.deepEqual(receipt, original, "inspection retains the original identity, launch, phase and revision");
        assert.deepEqual(calls, [["settings", binding.node], ["agents"]]);
        assert.equal(f.saved.length, 0); assert.equal(f.posts.length, 0); assert.equal(f.receipts.length, 0);
    });
}

test("command registration must stop before POST until effective permission is explicitly confirmed", async () => {
    const { f, ports, remember } = fixture();
    const prepared = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
    assert.equal(prepared.phase, "saved", "launch save is not permission confirmation or binding");
    assert.equal(f.saved.length, 1);
    assert.equal(f.posts.length, 0, "saving a new node-local command must not silently inherit policy and bind");
});

test("permission facts are separate from launch / settings revisions and bind requires explicit consent", async () => {
    const { f, ports, remember } = fixture();
    const receipt = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
    await assert.rejects(submitFleetAgent(receipt, ports, remember), { code: "permissionRequired" });
    assert.equal(f.posts.length, 0);
    ports.readPermission = async (node, harness) => policy(node, harness, "auto", "hub_harness", "opaque-policy-revision");
    const fact = await readFleetAgentPermission(receipt, ports);
    assert.notEqual(fact.revision, receipt.revision); assert.notEqual(fact.revision, receipt.launch);
    assert.equal(f.posts.length, 0, "GET is not consent or binding");
    await submitFleetAgent(receipt, ports, remember, fact);
    assert.deepEqual(f.posts, [{ ...binding, expected_permission: "auto", expected_permission_revision: "opaque-policy-revision" }]);
    assert.deepEqual(f.receipts.at(-1).confirmation, fact);
});
test("unknown or wrong-scope permission facts cannot authorize a binding", async () => {
    for (const invalid of [null, {}, policy("other-node", binding.harness), policy(binding.node, "other-harness"), policy(binding.node, binding.harness, "unsafe-default"), policy(binding.node, binding.harness, "read", "guessed_namespace_policy"), policy(binding.node, binding.harness, "read", "default_read", "")]) {
        const { f, ports, remember } = fixture(); const receipt = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
        ports.readPermission = async () => invalid;
        await assert.rejects(readFleetAgentPermission(receipt, ports), { code: "invalidPermission" });
        assert.equal(f.posts.length, 0); assert.equal(f.receipt.phase, "saved");
    }
});
test("typed permission rejection invalidates the assertion but never silently resubmits a changed policy", async () => {
    const { f, ports, remember } = fixture(); const receipt = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
    const fact = await readFleetAgentPermission(receipt, ports);
    f.bindError = Object.assign(new Error("permission changed"), { rejected: true });
    await assert.rejects(submitFleetAgent(receipt, ports, remember, fact));
    assert.equal(f.receipt.phase, "bind-rejected"); assert.equal(f.receipt.confirmation, undefined);
    ports.readPermission = async (node, harness) => policy(node, harness, "always_allow", "shared_remote_permissions", "new-policy-revision");
    const refreshed = await readFleetAgentPermission(f.receipt, ports);
    assert.equal(refreshed.permission, "always_allow"); assert.equal(f.posts.length, 1);
    await assert.rejects(submitFleetAgent(f.receipt, ports, remember), { code: "permissionRequired" });
    assert.equal(f.posts.length, 1, "a new GET still needs a new explicit confirmation");
});

test("only exact typed permission errors authorize confirmation refresh; generic 400/409 stay unknown", () => {
    assert.equal(permissionConfirmationRejected(409, "agent_permission_conflict"), true);
    assert.equal(permissionConfirmationRejected(400, "agent_permission_confirmation_invalid"), true);
    assert.equal(permissionConfirmationRejected(400), false);
    assert.equal(permissionConfirmationRejected(409), false);
    assert.equal(permissionConfirmationRejected(400, "agent_permission_conflict"), false);
    assert.equal(permissionConfirmationRejected(409, "some_other_conflict"), false);
});

test("all five coordinator policies can be read and explicitly asserted without a settings policy write", async () => {
    for (const permission of ["read", "write", "deny", "auto", "always_allow"]) {
        const { f, ports, remember } = fixture(); const receipt = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
        ports.readPermission = async (node, harness) => policy(node, harness, permission, "hub_harness", `opaque-${permission}`);
        const fact = await readFleetAgentPermission(receipt, ports);
        assert.equal(f.posts.length, 0);
        await submitFleetAgent(receipt, ports, remember, fact);
        assert.equal(f.posts[0].expected_permission, permission); assert.equal(f.posts[0].expected_permission_revision, fact.revision);
        assert.equal(f.saved[0].settings.harnesses[binding.harness].permission, undefined, "assertion is never a node settings policy setter");
    }
});

test("the actual HTTP client preserves typed rejection codes and permission GET is scoped and no-store", async () => {
    const { readFile } = await import("node:fs/promises");
    const ts = (await import("typescript")).default;
    const previous = { window: globalThis.window, sessionStorage: globalThis.sessionStorage, fetch: globalThis.fetch };
    globalThis.window = { location: { search: "?token=fixture-owner-token" } };
    globalThis.sessionStorage = { getItem: () => "fixture-owner-token", setItem() {} };
    let response = { status: 200, value: policy(binding.node, binding.harness) };
    const requests = [];
    globalThis.fetch = async (url, init) => { requests.push({ url, init }); return new Response(JSON.stringify(response.value), { status: response.status }); };
    try {
        const compile = source => ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext } }).outputText;
        const httpURL = `data:text/javascript;base64,${Buffer.from(compile(await readFile(new URL("../src/lib/http.ts", import.meta.url), "utf8"))).toString("base64")}`;
        let fleet = compile(await readFile(new URL("../src/lib/api/fleet.ts", import.meta.url), "utf8"));
        fleet = fleet.replace('from "../http"', `from ${JSON.stringify(httpURL)}`).replace('from "../i18n"', `from ${JSON.stringify(new URL("../src/lib/i18n.ts", import.meta.url).href)}`);
        const api = await import(`data:text/javascript;base64,${Buffer.from(fleet).toString("base64")}`);
        const fact = await api.fetchAgentPermission(binding.node, binding.harness);
        assert.deepEqual(fact, policy(binding.node, binding.harness));
        assert.equal(requests[0].url, "./console/agents/permission?node=remote&harness=my-acp");
        assert.equal(requests[0].init.cache, "no-store"); assert.equal(requests[0].init.headers.get("Authorization"), "Bearer fixture-owner-token");
        for (const [status, code] of [[409, "agent_permission_conflict"], [400, "agent_permission_confirmation_invalid"], [400, undefined]]) {
            response = { status, value: { error: "fixture rejection", ...(code ? { code } : {}) } };
            await assert.rejects(api.addAgent({ ...binding, expected_permission: "read", expected_permission_revision: "opaque-original" }), error => error.status === status && error.code === code);
            const sent = JSON.parse(requests.at(-1).init.body);
            assert.equal(sent.expected_permission, "read"); assert.equal(sent.expected_permission_revision, "opaque-original");
        }
    } finally { for (const [key, value] of Object.entries(previous)) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; } }
});

test("a matching existing Agent cannot skip confirmation of a newly saved launch", async () => {
    const { f, ports, remember } = fixture(); const receipt = await prepareFleetAgent(binding, f.settings, launch, ports, remember);
    f.agents.push({ ...binding });
    assert.equal((await inspectFleetAgent(receipt, ports)).phase, "saved");
    assert.equal((await inspectFleetAgent({ ...receipt, phase: "save-unknown" }, ports)).phase, "saved");
    assert.equal((await inspectFleetAgent({ ...receipt, phase: "bind-rejected" }, ports)).phase, "bind-rejected");
    assert.equal(f.posts.length, 0, "checking a matching old binding is not an asserted POST");
});

test("only known undispatched or rejected bindings can be canceled; unknown receipts remain protected", () => {
    assert.equal(canCancelFleetAgentBinding(null), false);
    for (const phase of ["save-unknown", "save-rejected", "saved", "bind-unknown", "bind-rejected", "bound"]) {
        assert.equal(canCancelFleetAgentBinding({ ...binding, phase, launch: "digest", revision: "r1" }), ["saved", "bind-rejected"].includes(phase), phase);
    }
});
