import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState } from "./work-fixture.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(6000);
const baseSettings = harnesses => ({ revision: "r1", harnesses, tools: ["git"], mcp_servers: {}, declares: ["network:fixture"], capabilities: ["keep-this"] });
const f = {
    settings: {
        "fixture-hub": baseSettings({ "unbound-acp": { command: "/fixture/未知 agent", args: ["acp", "", "含 空格"] }, pinned: { command: "/fixture/pinned", adapter: "codex-acp" } }),
        "fixture-remote": baseSettings({ "remote-alone": { command: "/remote/fixture-acp", args: ["--stdio"] }, "remote-pinned": { command: "/remote/pinned", adapter: "claude-agent-acp" } }),
    }, agents: [], writes: [], reads: [], errors: [], permissionReads: 0, policy: "read", policySource: "default_read", policyRevision: "opaque-permission-r1", conflictPermission: false, rejectBind: false, resetBind: false, resetSave: false, commitSave: false, readError: false, holdBind: false, release: null,
};
const unverified = "ACP not verified. Saving or binding is not an ACP handshake.";
page.on("pageerror", error => f.errors.push(String(error)));
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.EventSource = class { addEventListener() {} constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} }; });
function bind(body) { f.agents.push({ ...body, node: body.node || "fixture-hub", eligible: true, requires: [], models: [], selectors: [], options: {} }); }
await page.route("**/*", async route => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    assert.equal(url.origin, new URL(origin).origin, "external traffic prohibited");
    if (!["/state", "/events"].includes(p) && !p.startsWith("/console/")) return route.continue();
    if (req.method() === "GET") f.reads.push(p);
    if (p === "/state") return route.fulfill({ json: workState({ at: "2026-10-07T01:00:00Z", hub: { node: "fixture-hub", version: "test" }, nodes: [{ name: "fixture-remote", up: true, role: "worker", version: "test", os: "linux", arch: "amd64" }], agents: f.agents, tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
    const match = p.match(/^\/console\/nodes\/([^/]+)\/settings$/);
    if (match) {
        const node = decodeURIComponent(match[1]); assert.ok(f.settings[node]);
        if (req.method() === "GET") return f.readError ? route.fulfill({ status: 503, json: { error: "Fixture node settings unavailable" } }) : route.fulfill({ json: { settings: f.settings[node] } });
        assert.equal(req.method(), "PUT"); const body = req.postDataJSON(); f.writes.push({ p, body });
        assert.equal(body.revision, f.settings[node].revision, "node-local CAS revision");
        // The Go validator's detailed contract is tested separately. This fixture
        // only reproduces its explicit duplicate-env rejection for the UI.
        for (const harness of Object.values(body.harnesses)) {
            const keys = (harness.env || []).map(entry => entry.slice(0, entry.indexOf("=")));
            if (new Set(keys).size !== keys.length) return route.fulfill({ status: 400, json: { error: 'duplicate environment key "MODE"' } });
        }
        const saved = { ...body, revision: `r${Number(body.revision.slice(1)) + 1}` };
        if (f.resetSave) { f.resetSave = false; if (f.commitSave) f.settings[node] = saved; return route.abort("connectionreset"); }
        f.settings[node] = saved;
        return route.fulfill({ json: { settings: saved } });
    }
    if (p === "/console/agents/permission") {
        assert.equal(req.method(), "GET"); f.permissionReads++;
        return route.fulfill({ json: { node: url.searchParams.get("node"), harness: url.searchParams.get("harness"), permission: f.policy, source: f.policySource, revision: f.policyRevision } });
    }
    if (p === "/console/agents" && req.method() === "POST") {
        const body = req.postDataJSON(); f.writes.push({ p, body });
        assert.equal(typeof body.expected_permission, "string");
        assert.equal(typeof body.expected_permission_revision, "string");
        if (f.conflictPermission) { f.conflictPermission = false; f.policy = "auto"; f.policySource = "shared_remote_permissions"; f.policyRevision = "opaque-permission-r2"; return route.fulfill({ status: 409, json: { error: "Policy changed", code: "agent_permission_conflict" } }); }
        assert.equal(body.expected_permission, f.policy); assert.equal(body.expected_permission_revision, f.policyRevision);
        if (f.holdBind) await new Promise(resolve => { f.release = resolve; });
        if (f.rejectBind) { f.rejectBind = false; return route.fulfill({ status: 403, json: { error: "Fixture binding rejected" } }); }
        if (f.resetBind) { f.resetBind = false; return route.abort("connectionreset"); }
        bind(body); return route.fulfill({ json: { ok: true } });
    }
    assert.equal(req.method(), "GET", `unexpected mutation ${p}; probes, installation and prompts are prohibited`);
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0 } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    return route.fulfill({ json: {} });
});
async function waitFor(check, reason) { for (let i = 0; i < 100; i++) { if (await check()) return; await new Promise(resolve => setTimeout(resolve, 25)); } assert.fail(reason); }
async function open(viaResource = false) {
    await page.getByRole("button", { name: viaResource ? "Add machine / agent" : "Add agent", exact: true }).first().click();
    const dialog = page.getByRole("dialog", { name: /Add (agent|machine)$/, exact: true });
    if (viaResource) await dialog.getByRole("tab", { name: "Agent", exact: true }).click();
    await dialog.getByRole("button", { name: /Harness \/ command$/ }).waitFor();
    await dialog.getByText("Reading launch configuration from this machine…", { exact: true }).waitFor({ state: "hidden" });
    return dialog;
}
async function choose(dialog, label, value) { await dialog.getByRole("button", { name: new RegExp(label + "$") }).click(); await page.getByRole("option", { name: value, exact: true }).click(); }
async function confirmPermission(dialog) {
    const checkbox = dialog.getByRole("checkbox", { name: "I confirm this execution permission and its source", exact: true });
    await checkbox.waitFor();
    const submit = dialog.getByRole("button", { name: "Bind Agent with confirmed permission", exact: true });
    assert.equal(await submit.isDisabled(), true, "policy read is not consent");
    await dialog.getByText("I confirm this execution permission and its source", { exact: true }).click();
    assert.equal(await checkbox.isChecked(), true); await submit.click();
}
async function close(dialog) { await dialog.getByRole("button", { name: "Done", exact: true }).click(); await dialog.waitFor({ state: "hidden" }); }
async function custom(dialog, id, harness, args = [], env = []) {
    await choose(dialog, "Machine", "fixture-remote");
    await waitFor(async () => !await dialog.getByRole("button", { name: /Harness \/ command$/ }).isDisabled(), "remote settings");
    await choose(dialog, "Harness \/ command", "Custom command");
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill(id);
    await dialog.getByRole("textbox", { name: "Harness ID", exact: true }).fill(harness);
    await dialog.getByRole("textbox", { name: "Executable", exact: true }).fill("/fixture/未知 agent with spaces");
    for (let i = 0; i < args.length; i++) { await dialog.getByRole("button", { name: "Add argument", exact: true }).click(); await dialog.getByRole("textbox", { name: `Argument ${i + 1}`, exact: true }).fill(args[i]); }
    if (env.length) { await dialog.getByText("Advanced: ordinary environment variables", { exact: true }).click(); for (let i = 0; i < env.length; i++) { await dialog.getByRole("button", { name: "Add environment entry", exact: true }).click(); await dialog.getByRole("textbox", { name: `Environment entry ${i + 1}`, exact: true }).fill(env[i]); } }
}
async function shot(name) { if (!process.env.COMMAND_FIRST_SCREENSHOTS) return; await mkdir(process.env.COMMAND_FIRST_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.COMMAND_FIRST_SCREENSHOTS, name + ".png"), animations: "disabled" }); }
try {
    await page.goto(origin + "#/fleet?tab=agents");
    let dialog = await open(true);
    await dialog.getByRole("button", { name: /Harness \/ command$/ }).click();
    await page.getByRole("option", { name: "unbound-acp", exact: true }).waitFor();
    assert.equal(await page.getByRole("option", { name: "grok", exact: true }).count(), 0);
    await page.getByRole("option", { name: "unbound-acp", exact: true }).click();
    const preview = dialog.getByRole("region", { name: "Machine and final command / argv", exact: true });
    assert.deepEqual(JSON.parse(await preview.locator("pre").innerText()), ["/fixture/未知 agent", "acp", "", "含 空格"]);
    assert.equal(f.writes.length, 0, "reading configured choices never launches/registers anything");
    await choose(dialog, "Harness \/ command", "pinned");
    await dialog.getByText(/Pinned catalog adapter: codex-acp/).waitFor();
    assert.equal(await dialog.getByRole("button", { name: "Add argument", exact: true }).count(), 0, "pinned launch has no argv editor");
    await choose(dialog, "Harness \/ command", "unbound-acp");
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill("local-reviewer");
    await dialog.getByRole("button", { name: "Review execution permission", exact: true }).click();
    await confirmPermission(dialog);
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    await dialog.getByText(unverified, { exact: true }).waitFor();
    assert.deepEqual(f.writes, [{ p: "/console/agents", body: { id: "local-reviewer", harness: "unbound-acp", expected_permission: "read", expected_permission_revision: "opaque-permission-r1" } }]);
    await close(dialog);
    console.log("PASS local unbound choices, pinned adapter read-only launch, final argv and unverified binding");

    dialog = await open();
    await choose(dialog, "Machine", "fixture-remote");
    await waitFor(async () => !await dialog.getByRole("button", { name: /Harness \/ command$/ }).isDisabled(), "node settings");
    await dialog.getByRole("button", { name: /Harness \/ command$/ }).click();
    await page.getByRole("option", { name: "remote-alone", exact: true }).waitFor();
    assert.equal(await page.getByRole("option", { name: "unbound-acp", exact: true }).count(), 0, "choices cannot bleed between nodes");
    await page.getByRole("option", { name: "remote-alone", exact: true }).click();
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill("remote-reviewer");
    await dialog.getByRole("button", { name: "Review execution permission", exact: true }).click();
    await confirmPermission(dialog);
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    assert.deepEqual(f.writes.at(-1).body, { id: "remote-reviewer", harness: "remote-alone", node: "fixture-remote", expected_permission: "read", expected_permission_revision: "opaque-permission-r1" });
    await close(dialog);
    console.log("PASS remote machine uses its complete configured harnesses, not the hub roster");

    const argv = ["acp", "", " 含 空格 ", '"quoted"', "$HOME", "--unknown", "a|b"];
    dialog = await open(); await custom(dialog, "custom-reviewer", "new-acp", argv, ["MODE=ordinary", "MODE=duplicate"]);
    const before = structuredClone(f.settings["fixture-remote"]);
    await dialog.getByText(/Non-secret values only/).waitFor();
    await dialog.getByRole("button", { name: "Save command and review permission", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: 'duplicate environment key "MODE"' }).waitFor();
    assert.equal(f.writes.at(-1).p, "/console/nodes/fixture-remote/settings");
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).click();
    await waitFor(async () => !await dialog.getByRole("textbox", { name: "Executable", exact: true }).isDisabled(), "rejection outcome readback");
    assert.equal(await dialog.getByRole("textbox", { name: "Argument 2", exact: true }).inputValue(), "");
    await dialog.getByRole("button", { name: "Remove environment entry 2", exact: true }).click();
    f.rejectBind = true;
    await dialog.getByRole("button", { name: "Save command and review permission", exact: true }).click();
    await confirmPermission(dialog);
    await dialog.getByRole("alert").filter({ hasText: "Fixture binding rejected" }).waitFor();
    const put = f.writes.at(-2);
    assert.deepEqual(put.body, { ...before, harnesses: { ...before.harnesses, "new-acp": { command: "/fixture/未知 agent with spaces", args: argv, env: ["MODE=ordinary"] } } });
    await dialog.getByText("Configuration revision: r2", { exact: true }).waitFor();
    const heldBinding = f.writes.at(-1).body;
    f.settings["fixture-remote"].capabilities.push("another-users-setting");
    const beforeRetry = f.writes.length;
    await page.setViewportSize({ width: 390, height: 844 });
    assert.ok(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth), "narrow dialog has no horizontal overflow");
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).focus(); await page.keyboard.press("Tab");
    assert.equal(await dialog.evaluate(element => element.contains(document.activeElement)), true);
    await shot("custom-partial-narrow-light");
    await page.evaluate(() => document.documentElement.classList.add("dark-mode")); await shot("custom-partial-narrow-dark");
    await confirmPermission(dialog);
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    assert.equal(f.writes.length, beforeRetry + 1); assert.deepEqual(f.writes.at(-1).body, heldBinding);
    assert.ok(f.settings["fixture-remote"].capabilities.includes("another-users-setting"), "no rollback overwrites another setting");
    assert.equal(await page.evaluate(() => Object.entries(localStorage).some(([key, value]) => key.includes("fleet-agent.receipt") && value.includes("MODE=ordinary"))), false);
    await close(dialog); await page.setViewportSize({ width: 1440, height: 1000 });
    console.log("PASS exact custom argv, duplicate env feedback, saved revision, explicit same-scope bind retry and narrow themes/focus");

    dialog = await open(); await choose(dialog, "Harness \/ command", "unbound-acp");
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill("permission-drift");
    await dialog.getByRole("button", { name: "Review execution permission", exact: true }).click();
    await dialog.getByText("Policy: read", { exact: true }).waitFor();
    const beforeConflict = f.writes.length, permissionReads = f.permissionReads;
    f.conflictPermission = true;
    await confirmPermission(dialog);
    await dialog.getByText("Policy: auto", { exact: true }).waitFor();
    await dialog.getByText("Source: Coordinator shared remote policy", { exact: true }).waitFor();
    await dialog.getByText(/auto \/ always_allow may approve/).waitFor();
    assert.equal(await dialog.getByRole("checkbox", { name: "I confirm this execution permission and its source", exact: true }).isChecked(), false);
    assert.equal(await dialog.getByRole("button", { name: "Bind Agent with confirmed permission", exact: true }).isDisabled(), true);
    assert.equal(f.writes.length, beforeConflict + 1, "409 cannot silently submit the new auto policy");
    assert.ok(f.permissionReads > permissionReads, "typed conflict rereads permission");
    assert.deepEqual(f.writes.at(-1).body, { id: "permission-drift", harness: "unbound-acp", expected_permission: "read", expected_permission_revision: "opaque-permission-r1" });
    await confirmPermission(dialog);
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    assert.deepEqual(f.writes.at(-1).body, { id: "permission-drift", harness: "unbound-acp", expected_permission: "auto", expected_permission_revision: "opaque-permission-r2" });
    assert.equal(f.writes.length, beforeConflict + 2); await close(dialog);
    f.policy = "read"; f.policySource = "default_read"; f.policyRevision = "opaque-permission-r1";
    console.log("PASS typed permission conflict rereads source/revision and requires a new explicit broad-policy confirmation");

    dialog = await open(); await choose(dialog, "Harness \/ command", "unbound-acp");
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill("unknown-binding");
    f.resetBind = true; f.holdBind = true;
    const beforeUnknown = f.writes.length;
    await dialog.getByRole("button", { name: "Review execution permission", exact: true }).click();
    await dialog.getByText("I confirm this execution permission and its source", { exact: true }).click();
    await dialog.getByRole("button", { name: "Bind Agent with confirmed permission", exact: true }).focus(); await page.keyboard.press("Enter");
    await waitFor(() => f.writes.length === beforeUnknown + 1, "one held POST");
    await page.keyboard.press("Enter"); await page.keyboard.press("Escape"); assert.equal(f.writes.length, beforeUnknown + 1);
    assert.equal(await dialog.isVisible(), true, "in-flight write cannot be dismissed");
    f.holdBind = false; f.release();
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).waitFor();
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).click();
    assert.equal(f.writes.length, beforeUnknown + 1);
    assert.equal(await dialog.getByRole("button", { name: "Bind Agent with confirmed permission", exact: true }).count(), 0, "unknown POST is not retryable");
    await page.reload(); dialog = await open();
    assert.equal(await dialog.getByRole("textbox", { name: "Name", exact: true }).inputValue(), "unknown-binding");
    assert.equal(await dialog.getByRole("textbox", { name: "Name", exact: true }).isDisabled(), true);
    bind({ id: "unknown-binding", harness: "unbound-acp" });
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).click();
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    assert.equal(f.writes.length, beforeUnknown + 1); await close(dialog);
    console.log("PASS transport-unknown retains original receipt across reload; readback never repeats POST");

    dialog = await open(); await custom(dialog, "save-unknown", "saved-after-loss", ["", "acp"]);
    const beforeSaveLoss = f.writes.length;
    f.resetSave = true; f.commitSave = true;
    await dialog.getByRole("button", { name: "Save command and review permission", exact: true }).click();
    await dialog.getByText("Harness save outcome unconfirmed", { exact: true }).waitFor();
    assert.equal(f.writes.length, beforeSaveLoss + 1);
    await page.reload(); dialog = await open();
    await dialog.getByRole("button", { name: "Check saved outcome (read-only)", exact: true }).click();
    await dialog.getByRole("button", { name: "Bind Agent with confirmed permission", exact: true }).waitFor();
    assert.equal(f.writes.length, beforeSaveLoss + 1, "checking lost PUT does not auto-bind or rewrite");
    await confirmPermission(dialog);
    await dialog.getByText("Agent binding confirmed", { exact: true }).waitFor();
    assert.equal(f.writes.length, beforeSaveLoss + 2); assert.equal(f.writes.at(-1).p, "/console/agents"); await close(dialog);
    console.log("PASS lost save reply reconciles exact node-local launch and revision without a second PUT");

    f.readError = true;
    await page.getByRole("button", { name: "Add agent", exact: true }).first().click(); dialog = page.getByRole("dialog", { name: /Add (agent|machine)$/, exact: true });
    await dialog.getByText("Fixture node settings unavailable", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("button", { name: /Harness \/ command$/ }).isDisabled(), true);
    f.readError = false; await dialog.getByRole("button", { name: "Reload machine configuration", exact: true }).click();
    await waitFor(async () => !await dialog.getByRole("button", { name: /Harness \/ command$/ }).isDisabled(), "read recovery");
    await choose(dialog, "Harness \/ command", "Custom command"); await page.setViewportSize({ width: 390, height: 844 });
    await dialog.getByRole("textbox", { name: "Executable", exact: true }).fill("/a/very/long/Unicode/未知 with spaces/agent/".repeat(8));
    assert.ok(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)); await shot("custom-form-long-narrow");
    await close(dialog); await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(origin + "#/fleet?tab=machines");
    await page.getByRole("row", { name: /fixture-remote/ }).click();
    const drawer = page.getByRole("dialog", { name: "fixture-remote", exact: true });
    await drawer.getByRole("button", { name: "Add agent", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "Add agent", exact: true });
    await dialog.waitFor();
    await dialog.getByText("Reading launch configuration from this machine…", { exact: true }).waitFor({ state: "hidden" });
    assert.match(await dialog.getByRole("button", { name: /Machine$/ }).innerText(), /fixture-remote/, "machine drawer entry preselects its real node");
    assert.equal(await dialog.evaluate(element => { const box = element.getBoundingClientRect(); return element.contains(document.elementFromPoint(box.x + box.width / 2, box.y + 30)); }), true, "add dialog is painted above the machine drawer");
    await choose(dialog, "Harness \/ command", "remote-alone");
    await shot("machine-entry-desktop");
    await close(dialog); await drawer.getByRole("button", { name: "Register agents on this machine", exact: true }).waitFor();
    console.log("PASS machine-scoped entry is preselected, painted above the drawer, and keeps known-tool discovery available");
    assert.deepEqual(f.errors, []);
    assert.equal(f.reads.some(p => /probe|\/models|\/install/.test(p)), false);
    console.log("PASS loading/error recovery, long command, no framework redesign and no probe/model/install calls");
} catch (error) { console.error("DEBUG", f.errors, await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
