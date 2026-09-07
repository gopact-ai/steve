// The desktop onboarding uses source modules and isolated API fixtures only.
import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-07T01:00:00Z", conversation = "console:desktop-ui";
const candidates = () => [
    { id: "codex", name: "Codex", harness: "codex-acp", executable: "/test/bin/codex", installed: true, requires: [], registered: false },
    { id: "claude", name: "Claude Code", harness: "claude-code", executable: "/test/bin/claude", installed: true, requires: ["node", "npm"], registered: false },
    { id: "kimi", name: "Kimi Code", harness: "kimi", installed: false, requires: [], registered: false },
];
async function waitFor(check, message) { for (let i = 0; i < 100; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function fixture(enabled = true) {
    const context = await browser.newContext({ viewport: { width: 1360, height: 980 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const f = { context, page, status: { enabled, node_id: enabled ? "my-desktop" : "", setup_required: enabled, agent_count: 0 }, agents: candidates(), posts: [], queue: [], errors: [], reads: 0, discoveryReads: 0, discoveryError: false, reset: false, reject: false, hold: false, release: null };
    page.on("pageerror", (error) => f.errors.push(String(error)));
    await page.addInitScript((id) => {
        localStorage.setItem("steve.ui.locale", "en"); sessionStorage.setItem("steve.conversation", id);
        window.sources = [];
        window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((item) => item !== this); } };
    }, conversation);
    await page.route("**/*", async (route) => {
        const req = route.request(), u = new URL(req.url()), p = u.pathname;
        if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
        if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
        if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (p === "/console/desktop") { f.reads++; return route.fulfill({ json: f.status }); }
        if (p === "/console/desktop/agents") {
            if (req.method() === "GET") { f.discoveryReads++; return f.discoveryError ? route.fulfill({ status: 503, json: { error: "Tool discovery is temporarily unavailable" } }) : route.fulfill({ json: { agents: f.agents } }); }
            const input = req.postDataJSON(); f.posts.push(input);
            if (f.hold) await new Promise((resolve) => { f.release = resolve; });
            if (f.reject) { f.reject = false; return route.fulfill({ status: 400, json: { error: "Codex is no longer installed. Refresh and select an available tool." } }); }
            for (const item of f.agents) if (input.agent_ids.includes(item.id)) item.registered = true;
            f.status = { ...f.status, setup_required: false, agent_count: f.agents.filter((item) => item.registered).length, default_agent: input.agent_ids[0] };
            if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
            return route.fulfill({ json: f.status });
        }
        if (p === "/state") return route.fulfill({ json: { at, hub: { node: "my-desktop", version: "test" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
        if (p === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation, agents: [] } } });
        if (p === "/console/conversations") return route.fulfill({ json: { conversations: [{ id: conversation, title: "First conversation", count: 1, last_at: at, running: false }] } });
        if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [] } });
        if (p === "/console/verbs" || p === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
        if (p === "/console/queue") { if (req.method() !== "GET") f.queue.push(req.postDataJSON()); return route.fulfill({ json: { queue: [], submission_keys: true, interactive_requests: true } }); }
        if (p === "/console/questions") return route.fulfill({ json: { questions: [] } });
        f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    f.close = async () => { assert.deepEqual(f.queue, []); assert.deepEqual(f.errors, []); await context.close(); };
    return f;
}
async function screenshot(page, name) { if (!process.env.ONBOARDING_SCREENSHOTS) return; await mkdir(process.env.ONBOARDING_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.ONBOARDING_SCREENSHOTS, name + ".png"), animations: "disabled" }); }
try {
    const webOnly = await fixture(false);
    await webOnly.page.goto(url + "#/console");
    await webOnly.page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    await waitFor(() => webOnly.reads > 0, "the app checks desktop availability");
    assert.equal(await webOnly.page.getByRole("dialog").count(), 0);
    assert.equal(webOnly.discoveryReads, 0); assert.deepEqual(webOnly.posts, []);
    await webOnly.close();
    console.log("PASS ordinary web use does not show desktop onboarding or discover agents");

    const f = await fixture(); const page = f.page;
    await page.goto(url + "#/console");
    const dialog = page.getByRole("dialog", { name: "This computer is ready" });
    await dialog.waitFor();
    await dialog.getByText("my-desktop", { exact: true }).waitFor();
    await dialog.getByText("Tools detected on this computer. Sign-in is checked when you use them.", { exact: true }).waitFor();
    const codex = dialog.getByRole("checkbox", { name: "Codex", exact: true });
    await codex.waitFor(); assert.equal(await codex.isChecked(), false);
    assert.equal(await dialog.getByRole("checkbox", { name: "Claude Code", exact: true }).isDisabled(), true);
    assert.equal(await dialog.getByRole("checkbox", { name: "Kimi Code", exact: true }).isDisabled(), true);
    assert.deepEqual(f.posts, []);
    await dialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await dialog.getByText("Select at least one installed tool.", { exact: true }).waitFor();
    assert.equal(await codex.evaluate((el) => document.activeElement === el), true);
    await page.keyboard.press("Space");
    assert.equal(await codex.isChecked(), true);
    f.hold = true;
    await dialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await waitFor(() => f.posts.length === 1, "one explicit enrollment");
    assert.deepEqual(f.posts[0], { agent_ids: ["codex"] });
    assert.equal(await dialog.getByText("Agents registered", { exact: true }).count(), 0);
    assert.equal(await dialog.getByRole("button", { name: "Register later", exact: true }).isDisabled(), true);
    f.hold = false; f.release();
    await dialog.getByText("Agents registered", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "Open workbench", exact: true }).click();
    await dialog.waitFor({ state: "hidden" });
    await page.reload();
    await page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await page.getByRole("dialog").count(), 0);
    await f.close();
    console.log("PASS first launch detects without enrolling, validates selection and waits for the registration result");

    const later = await fixture();
    await later.page.goto(url + "#/console");
    const laterDialog = later.page.getByRole("dialog", { name: "This computer is ready" });
    await laterDialog.getByRole("button", { name: "Register later", exact: true }).click();
    await laterDialog.waitFor({ state: "hidden" });
    await later.page.reload();
    await later.page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await later.page.getByRole("dialog").count(), 0);
    await later.page.goto(url + "#/console?setup=agents");
    await laterDialog.waitFor();
    assert.deepEqual(later.posts, []);
    await later.close();
    console.log("PASS postponing onboarding survives reload and the resources deep link opens it again");

    const retry = await fixture(); retry.discoveryError = true;
    await retry.page.goto(url + "#/console");
    const retryDialog = retry.page.getByRole("dialog", { name: "This computer is ready" });
    await retryDialog.getByText("Tool discovery is temporarily unavailable", { exact: true }).waitFor();
    retry.discoveryError = false;
    await retryDialog.getByRole("button", { name: "Check again", exact: true }).click();
    await retryDialog.getByRole("checkbox", { name: "Codex", exact: true }).waitFor();
    await retryDialog.getByText("Codex", { exact: true }).click();
    retry.reject = true;
    await retryDialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await retryDialog.getByText("Codex is no longer installed. Refresh and select an available tool.", { exact: true }).waitFor();
    assert.equal(await retryDialog.getByRole("checkbox", { name: "Codex", exact: true }).isChecked(), true);
    await screenshot(retry.page, "onboarding-error");
    retry.reset = true;
    await retryDialog.getByRole("button", { name: "Retry registration", exact: true }).click();
    await retryDialog.getByText("Agents registered", { exact: true }).waitFor();
    assert.deepEqual(retry.posts, [{ agent_ids: ["codex"] }, { agent_ids: ["codex"] }]);
    await retry.close();
    console.log("PASS discovery retries preserve selection and a dropped registration reply is reconciled from current registration");

    const empty = await fixture(); empty.agents = [];
    await empty.page.goto(url + "#/console");
    const emptyDialog = empty.page.getByRole("dialog", { name: "This computer is ready" });
    await emptyDialog.getByText("No local agents found", { exact: true }).waitFor();
    await empty.page.setViewportSize({ width: 390, height: 844 });
    await screenshot(empty.page, "onboarding-empty-narrow");
    assert.ok(await emptyDialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await empty.close();

    const narrow = await fixture();
    await narrow.page.setViewportSize({ width: 390, height: 844 });
    await narrow.page.goto(url + "#/console");
    const narrowDialog = narrow.page.getByRole("dialog", { name: "This computer is ready" });
    await narrowDialog.getByRole("checkbox", { name: "Codex", exact: true }).waitFor();
    await screenshot(narrow.page, "onboarding-narrow-light");
    await narrow.page.evaluate(() => document.documentElement.classList.add("dark-mode"));
    await narrow.page.waitForTimeout(180);
    await screenshot(narrow.page, "onboarding-narrow-dark");
    await narrowDialog.getByRole("button", { name: "Register later", exact: true }).focus();
    await narrow.page.keyboard.press("Tab");
    assert.equal(await narrowDialog.evaluate((el) => el.contains(document.activeElement)), true);
    assert.ok(await narrowDialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    assert.ok(await narrow.page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    await narrow.page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "zh"); localStorage.setItem("ui-theme", "dark"); });
    await narrow.page.reload();
    await narrow.page.getByRole("dialog", { name: "本机已就绪" }).getByRole("button", { name: "登记所选 Agent", exact: true }).waitFor();
    await screenshot(narrow.page, "onboarding-narrow-zh");
    await narrow.close();
    console.log("PASS empty discovery, narrow layouts, theme states and keyboard containment");
} finally { await browser.close(); await server.close(); }
