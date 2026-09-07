// Source preview with isolated node-agent discovery and registration fixtures.
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
const page = await context.newPage(); page.setDefaultTimeout(6000);
const at = "2026-09-07T01:00:00Z";
const candidates = () => [
    { id: "codex", name: "Codex", harness: "codex", executable: "/remote/bin/codex", adapter: "codex-acp", installed: true, requires: [], configured: false, registered: false },
    { id: "claude", name: "Claude Code", harness: "claude-code", executable: "/remote/bin/claude", adapter: "claude-agent-acp", installed: true, requires: ["npm"], configured: false, registered: false },
    { id: "kimi", name: "Kimi Code", harness: "kimi", installed: false, requires: [], configured: false, registered: false },
];
const f = { revision: "node-r1", agents: candidates(), reads: 0, posts: [], errors: [], hold: false, release: null, reset: false, partial: false, discoveryError: false };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} }; });
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "my-desktop", version: "test" }, nodes: [{ name: "build-node", up: true, role: "worker", version: "test", os: "linux", arch: "arm64", addr: "10.0.0.9:7701" }], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0 } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/nodes/build-node/agents") {
        if (req.method() === "GET") { f.reads++; return f.discoveryError ? route.fulfill({ status: 503, json: { error: "The node is temporarily unreachable" } }) : route.fulfill({ json: { revision: f.revision, agents: f.agents } }); }
        const body = req.postDataJSON(); f.posts.push(body);
        if (f.hold) await new Promise((resolve) => { f.release = resolve; });
        assert.equal(body.expected_revision, f.revision);
        f.agents[0].configured = true; f.revision = "node-r2";
        if (f.partial) { f.partial = false; return route.fulfill({ status: 400, json: { error: "The node tool was prepared but Agent registration could not be saved" } }); }
        f.agents[0].registered = true;
        if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
        return route.fulfill({ json: { candidate_id: body.candidate_id, agent_id: body.agent_id, harness: "codex", revision: f.revision, registered: true } });
    }
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
async function waitFor(check, message) { for (let i = 0; i < 120; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function open() {
    await page.getByRole("row", { name: /build-node/ }).click();
    await page.getByRole("button", { name: "Register agents on this machine", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Register an agent on build-node", exact: true });
    await dialog.waitFor(); return dialog;
}
async function screenshot(name) { if (!process.env.NODE_AGENT_SCREENSHOTS) return; await mkdir(process.env.NODE_AGENT_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.NODE_AGENT_SCREENSHOTS, name + ".png"), animations: "disabled" }); }
try {
    await page.goto(url + "#/fleet");
    let dialog = await open();
    await dialog.getByText("/remote/bin/codex", { exact: true }).waitFor();
    assert.equal(f.posts.length, 0);
    await dialog.getByText("Tool presence is checked on this machine. Sign-in is checked when the agent runs.", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("radio", { name: "Claude Code", exact: true }).isDisabled(), true);
    assert.equal(await dialog.getByRole("radio", { name: "Kimi Code", exact: true }).isDisabled(), true);
    await dialog.getByText("Codex", { exact: true }).click();
    await dialog.getByRole("textbox", { name: "Agent name", exact: true }).fill("remote-codex");
    await dialog.getByText("Register remote-codex on build-node using Codex. Any required adapter will be prepared on that machine.", { exact: true }).waitFor();
    f.hold = true; f.partial = true;
    await dialog.getByRole("button", { name: "Register agent", exact: true }).focus(); await page.keyboard.press("Enter");
    await waitFor(() => f.posts.length === 1, "one registration");
    await page.keyboard.press("Enter"); assert.equal(f.posts.length, 1);
    assert.equal(await dialog.getByText("Agent registered", { exact: true }).count(), 0);
    f.hold = false; f.release();
    await dialog.getByRole("button", { name: "Check and retry registration", exact: true }).waitFor();
    await dialog.getByText("Runtime configured; Agent registration still needs confirmation.", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("textbox", { name: "Agent name", exact: true }).inputValue(), "remote-codex");
    console.log("PASS node-local discovery, required tooling, explicit enrollment and no premature success");

    await page.reload(); dialog = await open();
    await dialog.getByRole("button", { name: "Check and retry registration", exact: true }).waitFor();
    assert.equal(await dialog.getByRole("textbox", { name: "Agent name", exact: true }).inputValue(), "remote-codex");
    assert.equal(await dialog.getByRole("textbox", { name: "Agent name", exact: true }).isDisabled(), true);
    f.reset = true;
    await dialog.getByRole("button", { name: "Check and retry registration", exact: true }).click();
    await waitFor(() => f.posts.length === 2, "retry after partial preparation");
    await dialog.getByRole("button", { name: "Check and retry registration", exact: true }).waitFor();
    assert.deepEqual(f.posts[1], { candidate_id: "codex", agent_id: "remote-codex", expected_revision: "node-r2" });
    assert.equal(await dialog.getByText("Agent registered", { exact: true }).count(), 0, "candidate registered flag does not acknowledge an exact agent ID");
    await dialog.getByRole("button", { name: "Check and retry registration", exact: true }).click();
    await dialog.getByText("Agent registered", { exact: true }).waitFor();
    assert.equal(f.posts.length, 3);
    assert.deepEqual(f.posts[2], f.posts[1]);
    await dialog.getByRole("button", { name: "Done", exact: true }).click();
    await dialog.waitFor({ state: "hidden" });
    await page.getByRole("button", { name: "Register agents on this machine", exact: true }).waitFor();
    console.log("PASS partial and unknown outcomes refresh the revision but preserve candidate and Agent identity");

    await page.getByRole("button", { name: "Register agents on this machine", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "Register an agent on build-node", exact: true });
    await dialog.getByText("Registered", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("radio", { name: "Codex", exact: true }).isDisabled(), true);
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await dialog.evaluate((el) => { const box = el.getBoundingClientRect(); const hit = document.elementFromPoint(box.x + box.width / 2, box.y + 30); return el.contains(hit); }), true, "the enrollment dialog must be painted above the machine drawer");
    await screenshot("node-agents-narrow-light");
    await page.evaluate(() => document.documentElement.classList.add("dark-mode")); await page.waitForTimeout(180);
    await screenshot("node-agents-narrow-dark");
    assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await dialog.getByRole("button", { name: "Close", exact: true }).focus(); await page.keyboard.press("Tab");
    assert.equal(await dialog.evaluate((el) => el.contains(document.activeElement)), true);
    await dialog.getByRole("button", { name: "Close", exact: true }).click();
    f.discoveryError = true;
    await page.getByRole("button", { name: "Register agents on this machine", exact: true }).click();
    await dialog.getByText("The node is temporarily unreachable", { exact: true }).waitFor();
    f.discoveryError = false;
    await dialog.getByRole("button", { name: "Refresh tools", exact: true }).click();
    await dialog.getByText("Registered", { exact: true }).waitFor();
    assert.deepEqual(f.errors, []);
    console.log("PASS registered state, nested-dialog focus, narrow themes and actionable read errors");
} catch (error) { console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
