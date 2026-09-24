import { workState } from "./work-fixture.mjs";
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
async function fixture(enabled = true, step = "identity", done = false) {
    const context = await browser.newContext({ viewport: { width: 1360, height: 980 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const f = { context, page, status: { enabled, node_id: enabled ? "my-desktop" : "", setup_required: enabled && !done, agent_count: 0, workspace_path: "/Users/me/Library/Application Support/Steve/workspace", workspace_managed: true, setup: { step, done } }, agents: candidates(), posts: [], setups: [], workspaces: [], renames: [], labelSaves: [], settingsPatches: [], name: "My Desktop", labels: [], revision: 3, queue: [], errors: [], reads: 0, discoveryReads: 0, discoveryError: false, reset: false, reject: false, hold: false, release: null };
    const coordination = () => ({ enabled: true, cluster_id: "cluster-test", node_id: "my-desktop", coordinator_id: "my-desktop", epoch: 1, revision: f.revision, authoritative: true, observed_at: at, auto_failover: false, ready: true, nodes: [{ id: "my-desktop", name: f.name, local: true, online: true, voter: true, auto_eligible: true, ready: true }], events: [] });
    const nodeSettings = () => ({ settings: { revision: "r" + f.labelSaves.length, harnesses: {}, tools: [], mcp_servers: {}, declares: [], capabilities: f.labels } });
    const hubSettings = () => ({ revision: "s" + f.settingsPatches.length, desired: { gateway: { locale: f.settingsPatches.at(-1)?.settings?.gateway?.locale ?? "zh" } }, effective: { gateway: { locale: "zh" } }, pending_restart: false, apply_mode: "restart", fields: [] });
    page.on("pageerror", (error) => f.errors.push(String(error)));
    await page.addInitScript((id) => {
        localStorage.setItem("steve.ui.locale", "en"); sessionStorage.setItem("steve.conversation", id);
        window.sources = [];
        window.EventSource = class { addEventListener() {} constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((item) => item !== this); } };
    }, conversation);
    await page.route("**/*", async (route) => {
        const req = route.request(), u = new URL(req.url()), p = u.pathname;
        if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
        if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
        if (p === "/console/coordination") return route.fulfill({ json: coordination() });
        if (p === "/console/coordination/name" && req.method() === "PUT") { const body = req.postDataJSON(); f.renames.push(body); if (body.expected_revision !== f.revision) return route.fulfill({ status: 409, json: { error: "revision conflict" } }); f.name = body.name; f.revision++; return route.fulfill({ json: coordination() }); }
        if (p === "/console/nodes/my-desktop/settings") { if (req.method() === "PUT") { const body = req.postDataJSON(); f.labelSaves.push(body); f.labels = body.capabilities; } return route.fulfill({ json: nodeSettings() }); }
        if (p === "/console/settings") { if (req.method() === "PATCH") f.settingsPatches.push(req.postDataJSON()); return route.fulfill({ json: hubSettings() }); }
        if (p === "/console/desktop") { f.reads++; return route.fulfill({ json: f.status }); }
        if (p === "/console/desktop/setup" && req.method() === "PUT") { const body = req.postDataJSON(); f.setups.push(body); const done = !!body.done || f.status.setup.done; f.status = { ...f.status, setup: { step: body.step, done }, setup_required: !done }; return route.fulfill({ json: f.status }); }
        if (p === "/console/desktop/workspace" && req.method() === "PUT") { const body = req.postDataJSON(); f.workspaces.push(body); if (body.path.startsWith("/etc")) return route.fulfill({ status: 400, json: { error: "/etc 属于系统目录，请选择个人目录下的文件夹" } }); f.status = { ...f.status, workspace_path: body.path.replace(/^~/, "/Users/me"), workspace_managed: false }; return route.fulfill({ json: f.status }); }
        if (p === "/console/desktop/agents") {
            if (req.method() === "GET") { f.discoveryReads++; return f.discoveryError ? route.fulfill({ status: 503, json: { error: "Tool discovery is temporarily unavailable" } }) : route.fulfill({ json: { agents: f.agents } }); }
            const input = req.postDataJSON(); f.posts.push(input);
            if (f.hold) await new Promise((resolve) => { f.release = resolve; });
            if (f.reject) { f.reject = false; return route.fulfill({ status: 400, json: { error: "Codex is no longer installed. Refresh and select an available tool." } }); }
            const chosen = input.agents || (input.agent_ids || []).map((id) => ({ candidate_id: id, agent_id: id }));
            for (const item of f.agents) if (chosen.some((pick) => pick.candidate_id === item.id)) item.registered = true;
            f.status = { ...f.status, agent_count: f.agents.filter((item) => item.registered).length, default_agent: (chosen.find((pick) => pick.default) || chosen[0])?.agent_id };
            if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
            return route.fulfill({ json: f.status });
        }
        if (p === "/state") return route.fulfill({ json: workState({ at, hub: { node: "my-desktop", version: "test" }, nodes: [{ name: "my-desktop", role: "hub", up: true }], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
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
    const dialog = page.getByRole("dialog", { name: "First-time setup" });
    await dialog.waitFor();
    await dialog.getByRole("heading", { name: "This computer", exact: true }).waitFor();
    await dialog.getByText("Step 1 of 5 · This computer", { exact: true }).waitFor();
    const progress = dialog.getByRole("progressbar");
    assert.equal(await progress.getAttribute("aria-valuenow"), "0");
    const nameInput = dialog.getByRole("textbox", { name: "Name", exact: true });
    await nameInput.waitFor();
    assert.equal(await nameInput.inputValue(), "My Desktop", "the current display name is proposed");
    await dialog.getByText("my-desktop", { exact: true }).waitFor();
    await nameInput.fill("   ");
    await dialog.getByRole("button", { name: "Next", exact: true }).click();
    await dialog.getByText("Give this computer a name.", { exact: true }).waitFor();
    assert.deepEqual(f.renames, []);
    await nameInput.fill("Work laptop");
    await dialog.getByRole("textbox", { name: "Labels", exact: true }).fill("gpu, lab, gpu");
    await dialog.getByRole("list", { name: "Labels" }).getByText("lab", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "Next", exact: true }).click();
    await dialog.getByRole("heading", { name: "Working directory", exact: true }).waitFor();
    assert.equal(f.renames.length, 1); assert.equal(f.renames[0].node_id, "my-desktop"); assert.equal(f.renames[0].name, "Work laptop"); assert.equal(f.renames[0].expected_revision, 3);
    assert.deepEqual(f.labelSaves.map((item) => item.capabilities), [["gpu", "lab"]], "labels are saved once, without duplicates");
    assert.deepEqual(f.setups, [{ step: "workspace" }], "progress is recorded as the guide moves");
    assert.equal(await progress.getAttribute("aria-valuenow"), "1");

    const workspace = dialog.getByRole("textbox", { name: "Working directory", exact: true });
    assert.equal(await workspace.inputValue(), "~/Steve", "a directory inside Application Support is not proposed");
    assert.equal(await dialog.getByRole("button", { name: "Choose folder…", exact: true }).count(), 0, "browsers have no native chooser");
    await workspace.fill("/etc");
    await dialog.getByRole("button", { name: "Next", exact: true }).click();
    await dialog.getByText("/etc 属于系统目录，请选择个人目录下的文件夹", { exact: true }).waitFor();
    await dialog.getByRole("heading", { name: "Working directory", exact: true }).waitFor();
    await workspace.fill("~/Steve");
    await dialog.getByRole("button", { name: "Next", exact: true }).click();
    await dialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
    assert.deepEqual(f.workspaces, [{ path: "/etc" }, { path: "~/Steve" }]);
    assert.equal(f.status.workspace_path, "/Users/me/Steve");

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
    const codexName = dialog.getByRole("textbox", { name: "Name", exact: true });
    assert.equal(await codexName.inputValue(), "codex", "the tool's own id is proposed as the agent name");
    await codexName.fill("Coder");
    assert.equal(await dialog.getByRole("radiogroup", { name: "Default agent", exact: true }).count(), 0, "a single agent needs no default chooser");
    await dialog.getByText("coder will be the default after registration.", { exact: true }).waitFor();
    f.hold = true;
    await dialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await waitFor(() => f.posts.length === 1, "one explicit enrollment");
    assert.deepEqual(f.posts[0], { agents: [{ candidate_id: "codex", agent_id: "coder", default: true }] }, "the name the owner typed is what is registered");
    assert.equal(await dialog.getByRole("button", { name: "Finish later", exact: true }).isDisabled(), true);
    assert.equal(await dialog.getByRole("heading", { name: "Local agents", exact: true }).count(), 1, "the page waits for the registration result");
    f.hold = false; f.release();
    await dialog.getByRole("heading", { name: "Other machines", exact: true }).waitFor();
    await dialog.getByText("No other machines yet", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "Connect a machine over SSH", exact: true }).waitFor();
    assert.equal(await dialog.getByRole("button", { name: "Next", exact: true }).count(), 0, "without machines the only way on is to skip");
    await dialog.getByRole("button", { name: "Skip for now", exact: true }).click();
    await dialog.getByRole("heading", { name: "Preferences", exact: true }).waitFor();
    await dialog.getByText("Dark", { exact: true }).click();
    // Local preferences commit under a cross-window lock, then React paints.
    // No reload or backend operation should be needed to apply the choice.
    await page.waitForFunction(() => document.documentElement.classList.contains("dark-mode"), null, { timeout: 2000 });
    assert.equal(await page.evaluate(() => document.documentElement.classList.contains("dark-mode")), true, "appearance applies at once");
    await dialog.getByText("English", { exact: true }).click();
    await dialog.getByRole("button", { name: "Next", exact: true }).click();
    await dialog.getByRole("heading", { name: "All set", exact: true }).waitFor();
    assert.deepEqual(f.settingsPatches.map((item) => item.settings), [{ gateway: { locale: "en" } }], "the backend locale follows the chosen language");
    await dialog.getByText("/Users/me/Steve", { exact: true }).waitFor();
    await dialog.getByText(/^1 agent( ·|$)/).waitFor();
    assert.deepEqual(f.setups.map((item) => item.step), ["workspace", "agents", "machines", "preferences", "finished"]);
    await dialog.getByRole("button", { name: "Open workbench", exact: true }).click();
    await dialog.waitFor({ state: "hidden" });
    assert.deepEqual(f.setups.at(-1), { step: "finished", done: true });
    await page.reload();
    await page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await page.getByRole("dialog").count(), 0, "a finished guide stays closed");
    await screenshot(page, "onboarding-finished");
    await f.close();
    console.log("PASS the guide names the computer, sets the workspace, registers an agent, skips machines, applies preferences and finishes once");

    const resume = await fixture(true, "machines");
    await resume.page.goto(url + "#/console");
    const resumeDialog = resume.page.getByRole("dialog", { name: "First-time setup" });
    await resumeDialog.getByRole("heading", { name: "Other machines", exact: true }).waitFor();
    await resumeDialog.getByRole("button", { name: "Back", exact: true }).click();
    await resumeDialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
    assert.deepEqual(resume.setups, [{ step: "agents" }]);
    await resumeDialog.getByRole("button", { name: "Finish later", exact: true }).click();
    await resumeDialog.waitFor({ state: "hidden" });
    await resume.page.reload();
    await resume.page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await resume.page.getByRole("dialog").count(), 0, "closing keeps the guide away for this visit");
    await resume.page.goto(url + "?token=test-desktop-route#/fleet");
    await resume.page.evaluate(() => { window.navigationSentinel = "same-document"; });
    const enrollmentLink = resume.page.getByRole("link", { name: "Register local agents", exact: true });
    await enrollmentLink.waitFor();
    assert.equal(await enrollmentLink.getAttribute("href"), "#/console?setup=agents", "native link actions use a hash-router URL");
    await enrollmentLink.click();
    await resumeDialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
    assert.equal(new URL(resume.page.url()).hash, "#/console?setup=agents");
    assert.equal(new URL(resume.page.url()).search, "?token=test-desktop-route");
    assert.equal(await resume.page.evaluate(() => window.navigationSentinel), "same-document", "route links preserve the running document");
    await resumeDialog.getByRole("button", { name: "Finish later", exact: true }).click();
    await resume.page.getByRole("link", { name: "Resources", exact: true }).click();
    await resume.page.getByRole("tab", { name: /^Machines/ }).click();
    await resume.page.getByRole("row", { name: /my-desktop/ }).click();
    const machineEnrollment = resume.page.getByRole("link", { name: "Register agents on this machine", exact: true });
    await machineEnrollment.focus();
    await resume.page.keyboard.press("Enter");
    await resumeDialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
    assert.equal(new URL(resume.page.url()).hash, "#/console?setup=agents");
    assert.deepEqual(resume.posts, []);
    await resume.close();
    console.log("PASS the guide reopens where it stopped, goes back a page, and resource links open the agents page without reloading");

    const later = await fixture(true, "finished", true);
    await later.page.goto(url + "#/console");
    await later.page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await later.page.getByRole("dialog").count(), 0, "a finished guide does not open on its own");
    await later.page.goto(url + "#/console?setup=agents");
    const laterDialog = later.page.getByRole("dialog", { name: "First-time setup" });
    await laterDialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
    assert.equal(await laterDialog.getByRole("progressbar").count(), 0, "one page opened after the guide is done shows no step progress");
    assert.equal(await laterDialog.getByRole("button", { name: "Back", exact: true }).count(), 0);
    await laterDialog.getByText("Codex", { exact: true }).click();
    await laterDialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await laterDialog.waitFor({ state: "hidden" });
    assert.deepEqual(later.posts, [{ agents: [{ candidate_id: "codex", agent_id: "codex", default: true }] }]);
    assert.deepEqual(later.setups, [], "registering later writes no guide progress, so the guide stays finished");
    assert.equal(later.status.setup_required, false);
    await later.page.reload();
    await later.page.getByRole("heading", { name: "First conversation", exact: true }).waitFor();
    assert.equal(await later.page.getByRole("dialog").count(), 0);
    await later.close();
    console.log("PASS registering agents after the guide is done shows only that page and leaves the guide finished");

    const retry = await fixture(true, "agents"); retry.discoveryError = true;
    await retry.page.goto(url + "#/console");
    const retryDialog = retry.page.getByRole("dialog", { name: "First-time setup" });
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
    await retryDialog.getByRole("heading", { name: "Other machines", exact: true }).waitFor();
    assert.deepEqual(retry.posts, [{ agents: [{ candidate_id: "codex", agent_id: "codex", default: true }] }, { agents: [{ candidate_id: "codex", agent_id: "codex", default: true }] }]);
    await retry.close();
    console.log("PASS discovery retries preserve selection and a dropped registration reply is reconciled from current registration");

    const many = await fixture(true, "agents");
    many.agents = [
        { id: "codex", name: "Codex", harness: "codex-acp", executable: "/test/bin/codex", installed: true, requires: [], registered: false },
        { id: "claude", name: "Claude Code", harness: "claude-code", executable: "/test/bin/claude", installed: true, requires: [], registered: false },
    ];
    await many.page.goto(url + "#/console");
    const manyDialog = many.page.getByRole("dialog", { name: "First-time setup" });
    await manyDialog.getByRole("checkbox", { name: "Codex", exact: true }).waitFor();
    await manyDialog.getByText("Codex", { exact: true }).click();
    await manyDialog.getByText("Claude Code", { exact: true }).click();
    const manyNames = manyDialog.getByRole("textbox", { name: "Name", exact: true });
    assert.equal(await manyNames.count(), 2, "every chosen tool gets its own name");
    await manyNames.nth(0).fill("reviewer");
    await manyNames.nth(1).fill("reviewer");
    await manyDialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await manyDialog.getByText("An agent is already called reviewer. Pick another name.", { exact: true }).waitFor();
    assert.deepEqual(many.posts, [], "a name claimed twice is refused before anything is registered");
    await manyNames.nth(1).fill("Coder");
    await manyDialog.getByRole("textbox", { name: "Good for", exact: true }).nth(1).fill("frontend work");
    const chooser = manyDialog.getByRole("radiogroup", { name: "Default agent", exact: true });
    await chooser.getByText("coder", { exact: true }).click();
    await manyDialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
    await manyDialog.getByRole("heading", { name: "Other machines", exact: true }).waitFor();
    assert.deepEqual(many.posts, [{ agents: [{ candidate_id: "codex", agent_id: "reviewer" }, { candidate_id: "claude", agent_id: "coder", about: "frontend work", default: true }] }]);
    assert.equal(many.status.default_agent, "coder", "the chosen agent is the default, not the first one picked");
    await many.close();
    console.log("PASS each chosen tool is named, described and one of them is made the default");

    const client = await fixture(true, "agents");
    await client.page.goto(url + "#/console");
    const clientDialog = client.page.getByRole("dialog", { name: "First-time setup" });
    await clientDialog.getByRole("checkbox", { name: "Codex", exact: true }).waitFor();
    await clientDialog.getByText("No agents here, only connect remote machines", { exact: true }).click();
    assert.equal(await clientDialog.getByRole("checkbox", { name: "Codex", exact: true }).count(), 0, "the tool list is out of the way once this computer is only a console");
    await clientDialog.getByRole("button", { name: "Next: connect a machine", exact: true }).click();
    await clientDialog.getByRole("heading", { name: "Other machines", exact: true }).waitFor();
    await clientDialog.getByText("No agents are registered here yet. Connect a machine so there is someone to do the work.", { exact: true }).waitFor();
    await clientDialog.getByRole("button", { name: "Skip for now", exact: true }).click();
    await clientDialog.getByRole("heading", { name: "Preferences", exact: true }).waitFor();
    await clientDialog.getByRole("button", { name: "Next", exact: true }).click();
    await clientDialog.getByRole("heading", { name: "All set", exact: true }).waitFor();
    await clientDialog.getByText("No agents yet", { exact: true }).waitFor();
    assert.deepEqual(client.setups.map((item) => item.step), ["machines", "preferences", "finished"]);
    await clientDialog.getByRole("button", { name: "Open workbench", exact: true }).click();
    await clientDialog.waitFor({ state: "hidden" });
    assert.deepEqual(client.posts, [], "choosing to stay a console registers nothing");
    assert.equal(client.status.setup_required, false, "a console with no agents of its own is a finished setup");
    await client.close();
    console.log("PASS a computer with no agents of its own can finish the guide as a console for remote machines");

    const empty = await fixture(true, "agents"); empty.agents = [];
    await empty.page.goto(url + "#/console");
    const emptyDialog = empty.page.getByRole("dialog", { name: "First-time setup" });
    await emptyDialog.getByText("No local agents found", { exact: true }).waitFor();
    await empty.page.setViewportSize({ width: 390, height: 844 });
    await screenshot(empty.page, "onboarding-empty-narrow");
    assert.ok(await emptyDialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await empty.close();

    const narrow = await fixture();
    await narrow.page.setViewportSize({ width: 390, height: 844 });
    await narrow.page.goto(url + "#/console");
    const narrowDialog = narrow.page.getByRole("dialog", { name: "First-time setup" });
    await narrowDialog.getByRole("textbox", { name: "Name", exact: true }).waitFor();
    await screenshot(narrow.page, "onboarding-narrow-light");
    await narrow.page.evaluate(() => document.documentElement.classList.add("dark-mode"));
    await narrow.page.waitForTimeout(180);
    await screenshot(narrow.page, "onboarding-narrow-dark");
    await narrowDialog.getByRole("button", { name: "Finish later", exact: true }).focus();
    await narrow.page.keyboard.press("Tab");
    assert.equal(await narrowDialog.evaluate((el) => el.contains(document.activeElement)), true);
    assert.ok(await narrowDialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    assert.ok(await narrow.page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    await narrow.page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "zh"); localStorage.setItem("ui-theme", "dark"); });
    await narrow.page.reload();
    await narrow.page.getByRole("dialog", { name: "首次设置" }).getByText("第 1 步，共 5 步 · 本机名称", { exact: true }).waitFor();
    await screenshot(narrow.page, "onboarding-narrow-zh");
    await narrow.close();
    console.log("PASS empty discovery, narrow layouts, theme states and keyboard containment");
} finally { await browser.close(); await server.close(); }
