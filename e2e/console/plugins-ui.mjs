import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } }); await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(7000);
const at = "2026-09-09T00:00:00Z", digest = "a".repeat(64);
const manifest = { schema: 1, api: "steve.plugins.v1", id: "test/review", version: "1.0.0", description: "A review package with a fixed skill and node-local tool.", skills: { review: "skills/review" }, settings: { token: { description: "Node-local credential", secret: true, required: true } }, mcp: { api: { transport: "http", url: { text: "https://example.invalid/mcp" } } }, agents: { reviewer: { harness: "mock", model: "base", system_prompt: "Review carefully.", skills: ["review"], mcp_servers: ["api"] } } };
const f = { view: { revision: "r1", agents: [{ id: "existing", node: "", harness: "mock", skills: ["/legacy/review"], mcp_servers: ["old-api"] }], packages: [], installations: [], operations: [] }, calls: [], errors: [], resetImport: true, failRead: false, stale: false, holdPrepare: false, release: null };
const runtimeRef = { id: "c".repeat(64), selection: { project: "p", node: "", harness: "mock", deployments: ["d".repeat(64)] } };
f.usage = { references: [{ kind: "archive", owner: "chat/reviewer", runtime: runtimeRef }], runtimes: [{ ref: runtimeRef, command_id: "runtime", created_at: at, retired: false, uses: [{ id: "host", kind: "host", stopped: false }], packages: [{ id: manifest.id, version: "0.9.0", digest: "e".repeat(64) }] }], errors: {} };
page.on("pageerror", (error) => f.errors.push(String(error)));
page.on("dialog", (dialog) => dialog.accept());
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} }; });
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "laptop", version: "test" }, nodes: [], agents: [], projects: [{ id: "p", node: "", path: "/work", workspaces: [], agents: [], level: "internal" }], tasks: [], plans: [], attempts: [], landings: [] } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/coordination" || p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    if (p === "/console/plugins") return f.failRead ? route.fulfill({ status: 503, json: { error: "Cannot read current plugin state" } }) : route.fulfill({ json: f.view });
    if (p === "/console/plugins/preview") { f.calls.push({ kind: "preview", body: req.postDataJSON() }); return route.fulfill({ json: { manifest, digest, source: req.postDataJSON() } }); }
    if (p === "/console/plugins/import") {
        const body = req.postDataJSON(); f.calls.push({ kind: "import", body });
        if (!f.view.packages.length) f.view.packages.push({ project: "p", digest, manifest });
        if (f.resetImport) { f.resetImport = false; return route.abort("connectionreset"); }
        return route.fulfill({ json: f.view.packages[0] });
    }
    if (p.endsWith("/secrets")) return route.fulfill({ json: [{ reference: { name: "local-token", revision: "b".repeat(32) }, created_at: at }] });
    if (p.endsWith("/usage")) return route.fulfill({ json: f.usage });
    if (p.includes("/runtimes/") && p.endsWith("/close")) { f.calls.push({ kind: "close-runtime" }); f.usage.references = []; f.usage.runtimes[0].uses[0].stopped = true; return route.fulfill({ json: f.usage }); }
    if (p.startsWith("/console/plugins/installations/") && req.method() === "DELETE") { assert.equal(f.usage.references.length, 0); assert.equal(f.usage.runtimes[0].uses[0].stopped, true); f.calls.push({ kind: "remove", body: req.postDataJSON() }); f.view.installations = []; return route.fulfill({ json: f.view }); }
    if (p.includes("/presets/")) {
        const body = req.postDataJSON(); f.calls.push({ kind: p.endsWith("preview") ? "preset-preview" : "preset-apply", body });
        return route.fulfill({ json: { revision: "agent-r1", ...(body.adopt ? { existing: f.view.agents[0] } : {}), proposed: { id: body.agent_id, node: body.node, harness: "mock", model: "base", system_prompt: "Review carefully." }, preserved: [] } });
    }
    if (p.endsWith("/prepare")) {
        f.calls.push({ kind: "prepare" }); if (f.holdPrepare) await new Promise((resolve) => { f.release = resolve; });
        const item = f.view.installations[0]; item.targets = [{ node: "", state: "unavailable", error: "Interpreter is missing; register it on this machine." }];
        return route.fulfill({ json: item });
    }
    if (p.startsWith("/console/plugins/installations/") && req.method() === "PUT") {
        const body = req.postDataJSON(); f.calls.push({ kind: "save", body });
        if (f.stale || body.base_revision !== f.view.revision) return route.fulfill({ status: 409, json: { error: "Plugin settings changed. Refresh and review the latest configuration." } });
        const id = decodeURIComponent(p.split("/").at(-1)); f.view.revision = "r" + (Number(f.view.revision.slice(1)) + 1);
        f.view.installations = [{ id, installation: body.installation, targets: Object.keys(body.installation.targets).map((node) => ({ node, state: "pending" })) }]; return route.fulfill({ json: f.view });
    }
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
async function waitFor(check, message) { for (let n = 0; n < 150; n++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function screenshot(name) { if (!process.env.PLUGIN_SCREENSHOTS) return; await mkdir(process.env.PLUGIN_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.PLUGIN_SCREENSHOTS, name + ".png"), animations: "disabled", fullPage: true }); }
try {
    await page.goto(url + "#/plugins"); await page.getByRole("heading", { name: "Plugins", exact: true }).waitFor();
    await page.getByText("No Capability Packages", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Import Package", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Import Package", exact: true });
    await dialog.getByRole("textbox", { name: /Directory or Repository URL/ }).fill("/packages/review");
    await dialog.getByRole("button", { name: "Preview Contents", exact: true }).click();
    await dialog.getByText(digest, { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "Confirm Import", exact: true }).click();
    await dialog.getByRole("button", { name: "Retry Original Request", exact: true }).waitFor();
    await page.reload(); await page.getByRole("dialog", { name: "Import Package", exact: true }).waitFor();
    await page.getByRole("button", { name: "Retry Original Request", exact: true }).click();
    await page.getByRole("dialog").waitFor({ state: "hidden" });
    const imports = f.calls.filter((call) => call.kind === "import"); assert.equal(imports.length, 2); assert.deepEqual(imports[0].body, imports[1].body);
    console.log("PASS reviewed imports retain command identity across response loss and reload");

    await page.getByRole("button", { name: "Configure Installation", exact: true }).click();
    const configure = page.getByRole("dialog", { name: "Configure Installation", exact: true });
    await configure.getByRole("checkbox", { name: "This Machine · laptop", exact: true }).focus(); await page.keyboard.press("Space");
    await configure.getByRole("button", { name: /token/ }).first().click();
    await page.getByRole("option", { name: /local-token/ }).click();
    await configure.getByRole("button", { name: "Save Installation", exact: true }).click();
    await configure.waitFor({ state: "hidden" });
    assert.equal(f.view.installations[0].installation.enabled, false);
    assert.deepEqual(f.view.installations[0].installation.targets[""].secrets.token, { name: "local-token", revision: "b".repeat(32) });
    assert.ok(!JSON.stringify(f.calls).includes("upstream-secret-value"));
    await page.getByRole("button", { name: "Enable for New Sessions", exact: true }).click();
    await page.getByRole("button", { name: "Stop New Bindings", exact: true }).waitFor();
    f.holdPrepare = true; await page.getByRole("button", { name: "Prepare Machines", exact: true }).click();
    await waitFor(() => f.release, "preparation started");
    assert.equal(await page.getByRole("button", { name: "Edit Configuration", exact: true }).isDisabled(), true);
    f.holdPrepare = false; f.release();
    await page.getByText("Interpreter is missing; register it on this machine.").waitFor();
    console.log("PASS configuration selects credential references and separates enablement from node readiness");

    await page.getByRole("button", { name: "Edit Configuration", exact: true }).click();
    const edit = page.getByRole("dialog", { name: "Configure Installation", exact: true });
    const originalRevision = f.view.revision; f.view.revision = "r100"; f.view.installations[0].installation.enabled = false; f.stale = true;
    await edit.getByRole("button", { name: "Save Installation", exact: true }).click();
    await edit.getByText("Plugin settings changed. Refresh and review the latest configuration.").waitFor();
    assert.equal(f.calls.filter((call) => call.kind === "save").at(-1).body.base_revision, originalRevision);
    assert.equal(await edit.getByRole("textbox", { name: /Installation Name/ }).inputValue(), "test-review");
    await edit.getByRole("button", { name: "Close Plugin Details", exact: true }).click(); f.stale = false;
    await page.getByRole("button", { name: "Use Agent Preset", exact: true }).click();
    const preset = page.getByRole("dialog", { name: "Use Agent Preset", exact: true });
    await preset.getByRole("textbox", { name: /Agent Name/ }).fill("reviewer");
    await preset.getByRole("button", { name: "Preview Agent", exact: true }).click();
    await preset.getByText("Review carefully.", { exact: true }).waitFor();
    await preset.getByRole("button", { name: "Save Agent", exact: true }).click(); await preset.waitFor({ state: "hidden" });
    assert.equal(f.calls.find((call) => call.kind === "preset-apply").body.base_revision, "agent-r1");
    console.log("PASS stale updates retain drafts and agent presets are reviewed before applying");

    await page.getByRole("button", { name: "Use Agent Preset", exact: true }).click();
    await preset.getByRole("textbox", { name: /Agent Name/ }).fill("existing");
    await preset.getByRole("checkbox", { name: "Adopt This Existing Agent" }).focus(); await page.keyboard.press("Space");
    await preset.getByRole("button", { name: /Skills · \/legacy\/review/ }).click();
    await page.getByRole("option", { name: "review", exact: true }).click();
    await preset.getByRole("button", { name: /MCP Tools · old-api/ }).click();
    await page.getByRole("option", { name: "api", exact: true }).click();
    await preset.getByRole("button", { name: "Preview Agent", exact: true }).click();
    await preset.getByText(/Replace 1 skill attachments and 1 MCP attachments/).waitFor();
    await screenshot("adoption");
    await preset.getByRole("button", { name: "Save Agent", exact: true }).click();
    await preset.waitFor({ state: "hidden" });
    assert.deepEqual(f.calls.filter((call) => call.kind === "preset-apply").at(-1).body.adopt, { skills: { "/legacy/review": "review" }, mcp: { "old-api": "api" } });
    console.log("PASS explicit adoption reviews each legacy attachment before saving");
    await screenshot("desktop"); await page.setViewportSize({ width: 390, height: 844 });
    await screenshot("narrow-light"); await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" }); await screenshot("narrow-dark");
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), "narrow page does not overflow");
    await page.getByRole("button", { name: "Edit Configuration", exact: true }).click();
    await page.getByRole("dialog", { name: "Configure Installation", exact: true }).waitFor(); await page.getByRole("dialog").getByRole("button", { name: "Close Plugin Details", exact: true }).focus(); await page.keyboard.press("Tab"); assert.equal(await page.evaluate(() => !!document.activeElement.closest('[role="dialog"]')), true, "keyboard stays in dialog");
    await page.keyboard.press("Escape"); await page.getByRole("dialog").waitFor({ state: "hidden" });
    f.failRead = true; await page.getByRole("button", { name: "Refresh Status", exact: true }).click();
    await page.getByText(/Cannot read current plugin state/).waitFor();
    assert.equal(await page.getByRole("button", { name: "Edit Configuration", exact: true }).isDisabled(), true);
    f.failRead = false; await page.getByRole("button", { name: "Refresh Status", exact: true }).click();
    await page.getByRole("button", { name: "Session References & Removal", exact: true }).click();
    const usage = page.getByRole("dialog", { name: "Session References & Removal", exact: true });
    await usage.getByText("archive · chat/reviewer", { exact: true }).waitFor();
    assert.equal(await usage.getByRole("button", { name: "Remove Installation", exact: true }).isDisabled(), true);
    await screenshot("runtime-references");
    await usage.getByRole("button", { name: "Close & Release Session", exact: true }).click();
    await waitFor(async () => !(await usage.getByRole("button", { name: "Remove Installation", exact: true }).isDisabled()), "positive stop enables removal");
    await usage.getByRole("button", { name: "Remove Installation", exact: true }).click(); await usage.waitFor({ state: "hidden" });
    assert.equal(f.calls.filter((call) => call.kind === "remove").length, 1);
    console.log("PASS retained versions block removal until sessions are closed and stop evidence is positive");
    assert.deepEqual(f.errors, []);
    console.log("PASS narrow layouts, themes, keyboard focus and stale-read protection");
} catch (error) { console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
