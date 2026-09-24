// Isolated app, synthetic channel history. No network API or real writes.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import { workState, workDetail } from "../../../e2e/console/work-fixture.mjs";

const server = await createServer({ plugins: [{ name: "channel-fixture-controls", enforce: "pre", transform(source, id) {
    if (!id.endsWith("/src/app.tsx")) return;
    return 'import { useEffect as useFixtureEffect } from "react";\nimport { useIntent as useFixtureIntent } from "@/lib/fleet";\nimport { TaskDrawer as FixtureTaskDrawer } from "@/components/steve/task-drawer";\n' + source.replace("<Shell />", "<Shell /><ChannelFixtureControls />") + `
        function ChannelFixtureControls() {
            const intent = useFixtureIntent();
            const [task, setTask] = useState(null);
            useFixtureEffect(() => { window.__channelFixture = { act: intent.act, fill: intent.fill, openTask: setTask }; }, [intent.act, intent.fill]);
            return task ? <FixtureTaskDrawer t={task} tasks={[task]} onClose={()=>setTask(null)} /> : null;
        }`;
} }], root: fileURLToPath(new URL("..", import.meta.url)), logLevel: "error", server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(8000);
const ID = "console:opaque/channel ?中", OTHER = "opaque-other";
const at = (n) => `2026-09-21T00:${String(n).padStart(2, "0")}:00Z`;
const project = { id: "fixture", node: "fixture", path: "/fixture", workspaces: [] };
const channel = { id: ID, title: "Feishu original", transport: "feishu", read_only: true, project: "fixture", agent: "fixture", running: true, count: 4, last_at: at(4) };
const NORMAL = "console:normal";
const task = { id: "channel-task", goal: "Channel task fixture", transport: "feishu", channel: ID, member: "fixture", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, turns: 0, max_turns: 0, can_complete: true };
const conversations = [{ ...channel, id: NORMAL, transport: "console", read_only: false, title: "Console normal", running: false }, channel, { ...channel, transport: "console", read_only: false, title: "Console same raw ID" }, { ...channel, id: OTHER, title: "Other channel" }];
const reply = (id, n, patch = {}) => ({ id, conversation: ID, kind: "reply", text: id, at: at(n), ...patch });
let latest = [reply("new-input", 3, { kind: "sent", input: "New channel question", text: "" }), reply("new-answer", 4, { text: "New channel answer", delivery: "unconfirmed", project_id: "fixture", revision: "r1" })];
let older = [reply("old-input", 1, { kind: "sent", input: "Old channel question", text: "" }), reply("old-answer", 2, { text: "Old channel answer", delivery: "confirmed" }), latest[0]];
let failLatest = false, failOlder = false, failList = false, cursor = "older cursor/+", reads = 0, hold;
const errors = [], requests = [], writes = [];
const externalRuns = [];
let expectedExternalRun;
page.on("pageerror", e => errors.push(String(e)));
page.on("console", m => { if (m.type() === "error" && /Encountered two children|Each child.*unique/.test(m.text())) errors.push(m.text()); });
await context.route("**/*", async route => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
    requests.push([req.method(), p, url.search]);
    if (req.method() !== "GET") {
        const body = req.postDataJSON();
        if (expectedExternalRun && p === "/console/queue" && req.method() === "POST" && body.input === expectedExternalRun) {
            assert.notEqual(body.conversation, ID);
            assert.ok(body.conversation.startsWith("console:"));
            externalRuns.push(body);
            return route.fulfill({ json: { id: "external-fixture", conversation: body.conversation, key: `client:${body.command_id}`, state: "queued" } });
        }
        writes.push(p); return route.abort();
    }
    if (p === "/state") return route.fulfill({ json: workState({ at: at(4), hub: { node: "fixture", started: at(0) }, nodes: [], agents: [], tasks: [], projects: [project], plans: [], attempts: [], landings: [] }) });
    if (p === "/console/conversations" && failList) return route.fulfill({ status: 503, json: { error: "Fixture directory unavailable" } });
    if (p === `/console/tasks/${task.id}`) return route.fulfill({ json: workDetail(task) });
    if (p === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations } });
    if (p.startsWith("/console/channel-conversations/")) {
        reads++;
        const id = decodeURIComponent(p.slice("/console/channel-conversations/".length));
        assert.equal(url.searchParams.get("limit"), "50");
        if (id === OTHER) return route.fulfill({ json: { conversation: conversations.find(c => c.id === OTHER), replies: [reply("other-only", 1, { text: "Other isolated history" })] } });
        assert.equal(id, ID);
        const isOlder = url.searchParams.has("cursor");
        if (isOlder) assert.equal(url.searchParams.get("cursor"), cursor);
        const data = structuredClone({ conversation: channel, replies: isOlder ? older : latest, next_cursor: isOlder ? undefined : cursor });
        if (hold && !isOlder) { const h = hold; if (!h.all) hold = undefined; h.captured.resolve(); await h.release.promise; }
        if ((isOlder && failOlder) || (!isOlder && failLatest)) return route.fulfill({ status: 503, json: { error: "Fixture history unavailable" } }).catch(() => {});
        return route.fulfill({ json: data }).catch(() => {});
    }
    if (p === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation: url.searchParams.get("conversation"), project: { ...project, bound: true }, agents: [] } } });
    if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [reply("console-only", 1, { text: "Console normal history" })] } });
    if (p === "/console/queue") return route.fulfill({ json: { submission_keys: true, material_refs: true, queue: [] } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
    return route.fulfill({ json: { enabled: true, replies: [], verbs: [], suggestions: [], questions: [] } });
});
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { addEventListener() {} constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
});
const pane = () => page.locator("[data-channel-conversation]");
const shot = async name => { if (process.env.CHANNEL_SCREENSHOTS) { await mkdir(process.env.CHANNEL_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.CHANNEL_SCREENSHOTS, name + ".png") }); } };
try {
    const initialHold = { all: true, captured: Promise.withResolvers(), release: Promise.withResolvers() }; hold = initialHold;
    await page.goto(`${origin}/#/console?conversation=${encodeURIComponent(ID)}&transport=feishu`);
    await initialHold.captured.promise;
    await pane().getByRole("status").filter({ hasText: "Loading Feishu conversation" }).first().waitFor();
    await shot("channel-loading");
    assert.equal(await page.locator(".composer-dock").count(), 0);
    hold = undefined; initialHold.release.resolve();
    await pane().getByText("New channel answer", { exact: true }).waitFor();
    await pane().getByText("Delivery unconfirmed", { exact: true }).waitFor();
    // A fresh action originating outside the channel must reach a writable
    // Console identity, not be consumed by the previously viewed channel.
    await page.locator('a[href="#/fleet"]').first().click();
    await pane().waitFor({ state: "hidden" });
    await page.evaluate(() => window.__channelFixture.fill("@fixture external assignment"));
    await page.locator('.cm-content[contenteditable="true"]').filter({ hasText: "@fixture external assignment" }).waitFor();
    const externalID = await page.evaluate(() => sessionStorage.getItem("steve.conversation"));
    assert.notEqual(externalID, ID);
    assert.equal(await page.evaluate(() => sessionStorage.getItem("steve.conversation.transport")), "console");
    await page.getByRole("button", { name: /Feishu original/ }).click();
    await pane().getByText("New channel answer", { exact: true }).waitFor();
    assert.equal(await page.locator(".composer-dock").count(), 0);
    assert.equal(requests.some(([, p, query]) => ["/console/replies", "/console/context", "/console/queue"].includes(p) && new URLSearchParams(query).get("conversation") === ID), false, "channel must not mount console hooks");
    assert.equal(await pane().getByRole("button", { name: /edit|quote|send|cancel|retry|agent|add to/i }).count(), 0);
    assert.equal(await page.locator('.conversation-row[aria-current="page"]').count(), 1);
    assert.equal(await page.locator(".group\\/thread").filter({ hasText: "Feishu original" }).getByRole("button", { name: "More" }).count(), 0);
    await pane().getByText("Processing", { exact: true }).waitFor();
    await shot("channel-desktop");
    assert.equal(await page.getByRole("link", { name: "Workbench", exact: true }).getAttribute("aria-current"), "page");
    // Selecting source text must not reveal selection/material/side-chat actions.
    await pane().getByText("New channel answer", { exact: true }).evaluate(el => { const range = document.createRange(); range.selectNodeContents(el); const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range); document.dispatchEvent(new Event("selectionchange")); });
    await page.keyboard.press("Shift+F10");
    assert.equal(await page.locator(".selection-toolbar").count(), 0);
    await page.evaluate(() => getSelection().removeAllRanges());
    await page.evaluate(() => window.__channelFixture.act("/cancel"));
    await page.evaluate(() => window.__channelFixture.fill("must not leak into next console"));
    await page.keyboard.press("Enter"); await page.keyboard.press("Control+Enter"); await page.keyboard.press("Meta+Enter");
    assert.equal(await page.locator(".composer-dock").count(), 0);
    await page.evaluate(() => window.dispatchEvent(new Event("offline")));
    // Browser connectivity is separate from a failed HTTP read.
    await context.setOffline(true);
    await pane().getByRole("alert").filter({ hasText: "Disconnected" }).waitFor();
    await shot("channel-offline");
    await context.setOffline(false);
    failOlder = true;
    await pane().getByRole("button", { name: "Load earlier messages" }).click();
    await pane().getByRole("alert").filter({ hasText: "Fixture history unavailable" }).waitFor();
    assert.equal(await pane().getByText("New channel answer", { exact: true }).count(), 1);
    failOlder = false;
    await pane().getByRole("button", { name: "Load earlier messages" }).click();
    await pane().getByText("Old channel answer", { exact: true }).waitFor();
    assert.equal(await pane().getByText("New channel question", { exact: true }).count(), 1);
    channel.running = false;
    latest = [latest[0], { ...latest[1], text: "Final polled answer", delivery: "confirmed" }];
    await pane().getByText("Final polled answer", { exact: true }).waitFor();
    assert.equal(await pane().getByText("Old channel answer", { exact: true }).count(), 1);
    await pane().getByText("Not running", { exact: true }).waitFor();
    channel.execution = "unknown"; channel.running = true;
    await pane().getByText("Execution status unknown", { exact: true }).waitFor();
    await page.locator(".conversation-row").filter({ hasText: "Feishu original" }).getByText("Execution status unknown", { exact: true }).waitFor({ timeout: 13000 });
    await shot("channel-execution-unknown");
    delete channel.execution; channel.running = false;
    failLatest = true;
    await pane().getByRole("alert").filter({ hasText: "Fixture history unavailable" }).waitFor();
    await shot("channel-error");
    failLatest = false;
    latest = [reply("gap", 9, { text: "Long " + "unbroken".repeat(150), delivery: "suppressed" }), reply("missing", 10, { text: "", delivery: "unavailable" })];
    await pane().getByText("Not delivered", { exact: true }).waitFor();
    await pane().getByText("Reply body unavailable", { exact: true }).waitFor();
    await pane().getByText("History advanced beyond the loaded page. Showing the latest messages; load earlier messages again.", { exact: true }).waitFor();
    assert.equal(await pane().getByText("Old channel answer", { exact: true }).count(), 0);
    await page.setViewportSize({ width: 390, height: 844 });
    await shot("channel-narrow-long");
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await page.keyboard.press("Tab"); await page.keyboard.press("Enter"); await page.keyboard.press("Escape");
    assert.deepEqual(writes, []);
    await page.setViewportSize({ width: 1440, height: 1000 });
    // Native task navigation is enabled but its mutation controls remain closed.
    await page.evaluate(task => window.__channelFixture.openTask(task), task);
    const drawer = page.getByRole("dialog");
    await drawer.getByRole("button", { name: "Complete task", exact: true }).waitFor();
    assert.equal(await drawer.getByRole("button", { name: "Complete task", exact: true }).isEnabled(), false);
    await drawer.getByRole("button", { name: "Open in workbench", exact: true }).click();
    await drawer.waitFor({ state: "hidden" });
    const h = { captured: Promise.withResolvers(), release: Promise.withResolvers() }; hold = h;
    await h.captured.promise;
    await page.getByRole("button", { name: /Other channel/ }).click();
    await pane().getByText("Other isolated history", { exact: true }).waitFor();
    latest = latest.map(reply => reply.id === "gap" ? { ...reply, text: "Fresh after remount" } : reply);
    await page.getByRole("button", { name: /Feishu original/ }).click();
    await pane().getByText("Fresh after remount", { exact: true }).waitFor();
    h.release.resolve();
    await page.waitForTimeout(100);
    assert.equal(await pane().getByText("Other isolated history", { exact: true }).count(), 0);
    assert.equal(await pane().getByText("Fresh after remount", { exact: true }).count(), 1);
    for (let i = 0; i < 2; i++) {
        await page.getByRole("button", { name: /Console same raw ID/ }).click();
        await page.locator(".transcript-messages").getByText("Console normal history", { exact: true }).waitFor();
        await page.locator('.cm-content[contenteditable="true"]').fill("Console draft still editable");
        await page.getByRole("button", { name: /Feishu original/ }).click();
        await pane().getByText("Not delivered", { exact: true }).waitFor();
        assert.equal(await page.locator(".composer-dock").count(), 0);
    }
    await page.getByRole("button", { name: /^Console normal / }).click();
    const editor = page.locator('.cm-content[contenteditable="true"]');
    await editor.fill("Normal non-colliding console draft");
    assert.equal((await editor.innerText()).includes("must not leak"), false);
    await shot("console-normal-after-channel");
    await page.getByRole("button", { name: /Feishu original/ }).click();
    await pane().getByText("Not delivered", { exact: true }).waitFor();
    // A bare route uses the persisted transport, rather than treating its raw ID as Console.
    await page.goto(`${origin}/#/console`);
    await page.reload();
    await pane().getByText("Not delivered", { exact: true }).waitFor();
    assert.equal(await page.evaluate(() => sessionStorage.getItem("steve.conversation.transport")), "feishu");
    failList = true;
    await pane().getByRole("alert").filter({ hasText: "Conversation directory refresh failed" }).waitFor({ timeout: 13000 });
    assert.equal(await pane().getByRole("alert").filter({ hasText: "History and running status may be stale" }).count(), 0);
    failList = false;
    failLatest = true;
    await page.reload();
    await pane().getByRole("alert").filter({ hasText: "Fixture history unavailable" }).waitFor();
    assert.equal(await page.locator(".composer-dock").count(), 0);
    await shot("channel-initial-error");
    failLatest = false;
    latest = [];
    cursor = undefined;
    await pane().getByText("No history to display yet.", { exact: true }).waitFor();
    cursor = "older cursor/+"; latest = [reply("after-empty", 12)];
    await pane().getByText("after-empty", { exact: true }).waitFor();
    await pane().getByRole("button", { name: "Load earlier messages" }).waitFor();
    await page.locator('a[href="#/inbox"]').first().click();
    await pane().waitFor({ state: "hidden" });
    expectedExternalRun = "@fixture external run";
    const submitted = page.waitForResponse(response => response.request().method() === "POST" && new URL(response.url()).pathname === "/console/queue");
    await page.evaluate(text => window.__channelFixture.act(text), expectedExternalRun);
    assert.equal((await submitted).status(), 200);
    assert.equal(externalRuns.length, 1);
    assert.deepEqual(writes, []); assert.deepEqual(errors, []); assert.ok(reads >= 6);
    console.log("Channel read-only browser: identity, delivery, pagination, polling, errors, narrow/keyboard, switch isolation, console editing PASS");
} finally { await context.close(); await browser.close(); await server.close(); }
