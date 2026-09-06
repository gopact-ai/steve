// Build web/console first. Uses an existing Playwright installation;
// PLAYWRIGHT_MODULE accepts its absolute module path. CHECK selects comma-
// separated scenarios. Every API is mocked; no request reaches a real hub.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { preview } from "../childcard/preview.mjs";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || "playwright");
const output = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-console-interactions");
await mkdir(output, { recursive: true });
const app = await preview();
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-06T10:00:00Z";
const A = "console:interaction-a", B = "console:interaction-b";
const project = (id) => ({ id, node: "test-node", path: `/test/${id}`, repo: "inplace", level: "public", home: id === "home", agents: [], workspaces: [] });
const task = (id, channel, projectID) => ({ id, channel, project_id: projectID, goal: `Task ${id}`, state: "running", lifecycle: "running", execution: "running", lane: "running", attention: 0, turns: 1, max_turns: 10, member: "test-agent", node: "test-node", updated_at: at });
const gate = () => { let release; const promise = new Promise((resolve) => { release = resolve; }); return { promise, release }; };
async function eventually(predicate, message) {
    for (let i = 0; i < 80; i++) { if (await predicate()) return; await delay(25); }
    assert.fail(message);
}

async function fixture({ history = false, running = false } = {}) {
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(2500);
    await page.clock.install();
    const f = { page, context, calls: [], errors: [], releases: [], binding: null, enqueue: null, cancel: null, failBinding: false, failEnqueue: false, replyReads: 0 };
    page.on("pageerror", (e) => f.errors.push(String(e)));
    const conversations = [{ id: A, title: "Conversation A", project: "scratch" }, { id: B, title: "Conversation B", project: "home" }];
    const projects = [project("scratch"), project("home")];
    const replies = Object.fromEntries(conversations.map((c) => [c.id, Array.from({ length: history && c.id === A ? 35 : 1 }, (_, i) => ({ id: `${c.id}-${i}`, kind: "reply", conversation: c.id, at, text: `${c.title} history ${i}\n\nA retained answer with enough detail to occupy its own row.` }))]));
    const queue = running ? [{ id: "running-a", conversation: A, state: "running", input: "Running task", started_at: at, enqueued_at: at }] : [];
    const emit = (event) => page.evaluate((event) => window.emit(event), { at, conversation: A, ...event });
    f.emit = emit;
    f.startRunning = async () => {
        queue.push({ id: "design-running", conversation: A, state: "running", input: "Running layout check", started_at: at, enqueued_at: at });
        await emit({ kind: "console.queue" });
    };
    f.hold = (kind) => { const g = gate(); f[kind] = g.promise; f.releases.push(g.release); return g.release; };
    await page.route("**/*", async (route) => {
        const req = route.request(), url = new URL(req.url()), pathname = url.pathname;
        if (url.origin !== app.url) { f.errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
        if (!["/state", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        const input = req.postDataJSON();
        const call = { method: req.method(), path: pathname, ...input };
        if (req.method() !== "GET") f.calls.push(call);
        const conversation = input?.conversation || url.searchParams.get("conversation") || A;
        let current = conversations.find((c) => c.id === conversation);
        if (pathname === "/state") return route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks: [task("11", A, "scratch"), task("22", B, "home")], plans: [], projects, attempts: [], landings: [] } });
        if (pathname === "/console/send" && input?.input.startsWith("/project use ")) {
            if (f.binding) await f.binding;
            if (f.failBinding) return route.fulfill({ status: 503, json: { error: "Project binding unavailable" } });
            if (!current) { current = { id: conversation, title: "New project conversation", project: "home" }; conversations.push(current); }
            current.project = input.input.slice("/project use ".length);
            return route.fulfill({ json: { reply: { kind: "reply", conversation, at, text: `Bound ${current.project}` } } });
        }
        if (pathname === "/console/send") {
            if (input.input === "/cancel" && f.cancel) await f.cancel;
            return route.fulfill({ json: { reply: { kind: "reply", conversation, at, text: "Accepted" } } });
        }
        if (pathname === "/console/queue" && req.method() === "POST") {
            call.projectAtEnqueue = current?.project || "home";
            if (f.enqueue) await f.enqueue;
            if (f.failEnqueue) return route.fulfill({ status: 503, body: "Enqueue unavailable" });
            return route.fulfill({ json: { id: `queued-${f.calls.length}`, conversation, input: input.input, state: "queued", enqueued_at: at } });
        }
        if (pathname === "/console/replies") { f.replyReads++; return route.fulfill({ json: { enabled: true, replies: replies[conversation] || [] } }); }
        if (pathname === "/console/queue") return route.fulfill({ json: { queue: queue.filter((q) => q.conversation === conversation) } });
        if (pathname === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation, agents: [], project: { ...project(current?.project || "home"), bound: !!current } } } });
        if (pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: conversations.map((c) => ({ ...c, count: replies[c.id]?.length || 0, running: running && c.id === A, last_at: at })) } });
        if (pathname.startsWith("/console/conversations/") && req.method() === "PUT") {
            const changed = conversations.find((c) => c.id === decodeURIComponent(pathname.slice("/console/conversations/".length)));
            if (changed) Object.assign(changed, input);
            return route.fulfill({ json: { ok: true } });
        }
        if (pathname === "/console/verbs" || pathname === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
        f.errors.push(`Unhandled API: ${req.method()} ${pathname}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.addInitScript((conversation) => {
        if (!sessionStorage.getItem("steve.conversation")) sessionStorage.setItem("steve.conversation", conversation);
        window.sources = [];
        window.EventSource = class {
            constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
            close() { window.sources = window.sources.filter((s) => s !== this); }
        };
        window.emit = (ev) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(ev) }); };
    }, A);
    await page.goto(`${app.url}/#/console`);
    await page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
    await page.locator("main header").getByText("scratch · test-node", { exact: true }).waitFor();
    f.box = page.getByRole("textbox", { name: "Message", exact: true });
    f.queued = () => f.calls.filter((c) => c.path === "/console/queue" && c.method === "POST");
    f.pick = async (name) => { await page.getByRole("button", { name: new RegExp(`^Conversation ${name}`) }).click(); await page.locator("main header").getByText(`Conversation ${name}`, { exact: true }).waitFor(); };
    return f;
}

const checks = {
    async "project-inheritance"(f) {
        await f.page.getByRole("button", { name: "新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Generic new conversation must bind the current project");
        await f.box.fill("First work");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "First work must be accepted after binding");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "binding-pending"(f) {
        const release = f.hold("binding");
        await f.page.getByRole("button", { name: "在 scratch 下新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Project binding must begin");
        if (await f.box.isEnabled()) { await f.box.fill("First work during binding"); await f.box.press("Enter"); }
        await delay(150);
        assert.equal(f.queued().length, 0, "Work must not be submitted before project binding returns");
        release();
        await eventually(() => f.box.isEnabled(), "Composer must recover when binding completes");
        if (!f.queued().length) { await f.box.fill("First work after binding"); await f.box.press("Enter"); }
        await eventually(() => f.queued().length === 1, "Work should be accepted after binding");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "binding-failure"(f) {
        f.failBinding = true;
        await f.box.fill("Keep this existing draft");
        await f.page.getByRole("button", { name: "在 scratch 下新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Project binding must begin");
        await delay(150);
        assert.equal(await f.box.inputValue(), "Keep this existing draft", "Binding failure must preserve the original draft");
        assert.equal(await f.page.locator("main header").getByText("Conversation A", { exact: true }).count(), 1, "Binding failure must retain the original conversation");
        await f.page.reload();
        await f.box.waitFor();
        await f.page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Original draft remains usable after failed creation and reload");
        assert.equal(f.queued()[0].conversation, A, "Reload must not activate an unbound new conversation");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "draft-switch"(f) {
        await f.box.fill("Draft for A");
        await f.pick("B");
        assert.equal(await f.box.inputValue(), "", "A draft must not leak into B");
        await f.box.fill("Draft for B");
        await f.pick("A");
        assert.equal(await f.box.inputValue(), "Draft for A");
        await f.pick("B");
        assert.equal(await f.box.inputValue(), "Draft for B");
    },
    async "draft-persistence"(f) {
        await f.box.fill("Unsent work survives navigation");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await f.box.inputValue(), "Unsent work survives navigation", "Leaving the console must preserve its draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.equal(await f.box.inputValue(), "Unsent work survives navigation", "Reload must preserve the unsent draft");
    },
    async "send-continuation"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("First instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.pressSequentially("Next draft");
        assert.equal(await f.box.inputValue(), "Next draft", "Typing during a slow enqueue must begin a fresh draft");
        release();
        await delay(150);
        assert.equal(await f.box.inputValue(), "Next draft", "The delayed receipt must not clear the new draft");
        assert.equal(f.queued()[0].input, "First instruction");
    },
    async "send-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.box.fill("Unsent first instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.pressSequentially("New draft typed during submission");
        release();
        await f.page.getByText("Enqueue unavailable", { exact: true }).waitFor();
        assert.match(await f.box.inputValue(), /^Unsent first instruction\n+New draft typed during submission$/, "Failed submission must restore the first instruction without losing newer typing");
        assert.equal(f.queued().length, 1, "Failed submission must not retry automatically");
    },
    async "send-switch"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("A instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.pick("B");
        await f.box.fill("B unsent draft");
        release();
        await delay(150);
        assert.equal(await f.box.inputValue(), "B unsent draft", "A's receipt must not alter B's draft");
        await f.pick("A");
        assert.equal(await f.box.inputValue(), "", "A's submitted instruction must not reappear as a draft");
    },
    async "send-unmount-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.box.fill("Original instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        await f.box.fill("New draft after remount");
        release();
        await delay(200);
        assert.match(await f.box.inputValue(), /New draft after remount/, "An old failure must not overwrite the new mounted draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.match(await f.box.inputValue(), /New draft after remount/, "Newer typing must remain persisted after an unmounted request fails");
        assert.match(await f.box.inputValue(), /Original instruction/, "The failed instruction must remain recoverable");
        assert.equal(f.queued().length, 1, "Remounting must not automatically resend the failed instruction");
    },
    async "send-reload-pending"(f) {
        f.hold("enqueue");
        await f.box.fill("Instruction before network delivery");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.fill("Newer unsent draft");
        await f.page.reload();
        await f.box.waitFor();
        await f.page.getByText("发送结果尚未确认，请先查看会话记录", { exact: false }).waitFor();
        assert.equal(await f.page.getByText("Instruction before network delivery", { exact: true }).count(), 1, "Recovery must show the original unacknowledged instruction");
        assert.equal(await f.box.inputValue(), "Newer unsent draft", "Reload must keep new typing separate from an uncertain submission");
        assert.equal(f.queued().length, 1, "Reload must not resend an instruction that may already have been accepted");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 1, "Enter must not bypass unresolved submission recovery");
        assert.equal(await f.box.inputValue(), "Newer unsent draft");
        await f.page.getByRole("button", { name: "恢复为草稿", exact: true }).click();
        assert.match(await f.box.inputValue(), /Instruction before network delivery/, "Explicit recovery must restore the original instruction");
        assert.match(await f.box.inputValue(), /Newer unsent draft/, "Explicit recovery must also preserve the newer draft");
        assert.equal(f.queued().length, 1, "Recovering a draft must not send it automatically");
    },
    async "recovery-storage-failure"(f) {
        f.hold("enqueue");
        await f.box.fill("Original uncertain instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.fill("Newer persisted draft");
        await f.page.reload();
        const recover = f.page.getByRole("button", { name: "恢复为草稿", exact: true });
        await recover.waitFor();
        await f.page.evaluate(() => {
            Storage.prototype.setItem = () => { throw new DOMException("Quota exceeded", "QuotaExceededError"); };
        });
        await recover.click();
        await f.page.reload();
        await f.box.waitFor();
        await recover.waitFor();
        assert.equal(await f.page.getByText("Original uncertain instruction", { exact: true }).count(), 1, "Failed draft persistence must retain the original pending submission");
        assert.equal(await f.box.inputValue(), "Newer persisted draft", "Failed recovery must preserve the previously saved draft");
        assert.equal(f.queued().length, 1, "Failed recovery must not submit work");
    },
    async "quotes-unmount-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.page.getByRole("button", { name: "引用", exact: true }).click();
        await f.box.fill("Question about this quote");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        assert.deepEqual(f.queued()[0].quotes, [{ conversation: A, reply_id: `${A}-0` }]);
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        await f.box.waitFor();
        release();
        await eventually(() => f.page.getByRole("button", { name: "去掉引用", exact: true }).count(), "An unmounted failed submission must restore its quote in the current composer");
        assert.equal(await f.box.inputValue(), "Question about this quote");
        assert.equal(f.queued().length, 1, "Restoring the quoted draft must not submit it automatically");
    },
    async "repeat-enter"(f) {
        f.hold("enqueue");
        await f.box.fill("Only once");
        await f.box.press("Enter");
        await f.box.press("Enter");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 1, "Repeated Enter must enqueue the instruction only once");
    },
    async "stop-double-click"(f) {
        f.hold("cancel");
        const stop = f.page.getByRole("button", { name: "停止", exact: true });
        await stop.dblclick();
        await delay(150);
        assert.equal(f.calls.filter((c) => c.input === "/cancel").length, 1, "A double click must issue only one cancellation");
    },
    async "stop-with-draft"(f) {
        await f.box.fill("Draft written while a task is running");
        assert.equal(await f.page.getByRole("button", { name: "停止", exact: true }).count(), 1, "Stop must remain reachable while a draft is present");
    },
    async "stop-enter"(f) {
        f.hold("cancel");
        await f.page.getByRole("button", { name: "停止", exact: true }).click();
        await f.box.fill("New instruction during cancellation");
        assert.equal(await f.page.locator('button[aria-label="排队"]').isDisabled(), true, "Send must be disabled while cancellation is pending");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 0, "Enter must obey the same pending-cancel guard as the send button");
        assert.equal(await f.box.inputValue(), "New instruction during cancellation", "A blocked Enter must preserve the draft");
    },
    async "scroll-poll"(f) {
        const scroller = f.page.locator("main .overflow-y-auto.overflow-x-hidden");
        await scroller.hover();
        await f.page.mouse.wheel(0, -20000);
        await eventually(() => scroller.evaluate((el) => el.scrollTop < 20), "Mouse wheel must reach older history");
        const reads = f.replyReads;
        await f.page.clock.runFor(10500);
        await eventually(() => f.replyReads > reads, "Real console polling timer must fetch replies");
        await delay(150);
        assert.ok(await scroller.evaluate((el) => el.scrollTop < 20), "Unchanged polling must not pull the reader to the bottom");
    },
    async "scroll-stream"(f) {
        const scroller = f.page.locator("main .overflow-y-auto.overflow-x-hidden");
        await scroller.hover();
        await f.page.mouse.wheel(0, -20000);
        await eventually(() => scroller.evaluate((el) => el.scrollTop < 20), "Mouse wheel must reach older history");
        await f.emit({ kind: "console.progress", exchange_id: "running-a", progress: { reasoning: "Another streamed reasoning update" } });
        await delay(150);
        assert.ok(await scroller.evaluate((el) => el.scrollTop < 20), "Stream updates must preserve the manual reading position");
    },
    async "task-navigation"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "在工作台里看", exact: true }).click();
        await eventually(() => f.page.locator("main header").getByText("Conversation B", { exact: true }).isVisible(), "Opening B's task must navigate to conversation B");
        assert.equal(f.queued().length, 0, "Viewing a task must not submit /tasks into the current conversation");
    },
    async "task-action"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "暂停", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/tasks pause 22"), "Task pause command must be submitted");
        assert.equal(f.calls.find((c) => c.input === "/tasks pause 22").conversation, B, "Task action must target its owning conversation");
    },
    async "task-logical-failure"(f) {
        const rejection = "任务 #22 现在是「失败」，这一步做不了。";
        await f.page.route("**/console/send", (route) => route.fulfill({ json: { reply: { kind: "reply", conversation: B, at, text: rejection } } }));
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "暂停", exact: true }).click();
        await f.page.getByRole("dialog").getByText(rejection, { exact: true }).waitFor();
    },
    async "intent-remount-draft"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: /^(新计划|新建计划)$/ }).click();
        await eventually(async () => (await f.box.inputValue()).startsWith("/plan"), "New plan must fill the composer");
        await f.box.fill("Plan carefully drafted after fill");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await f.box.inputValue(), "Plan carefully drafted after fill", "An already-consumed fill intent must not overwrite the persisted draft");
    },
    async "conversation-rename-once"(f) {
        const edit = async (title) => {
            await f.page.getByRole("button", { name: new RegExp(`^${title}`) }).locator("..").getByRole("button", { name: "更多", exact: true }).click();
            await f.page.getByRole("menuitem", { name: "重命名", exact: true }).click();
            return f.page.getByRole("textbox", { name: "会话名称", exact: true });
        };
        const name = await edit("Conversation A");
        await name.fill("Renamed with Enter");
        await name.press("Enter");
        await f.box.click();
        await f.page.locator("main header").getByText("Renamed with Enter", { exact: true }).waitFor();
        const writes = () => f.calls.filter((c) => c.method === "PUT" && c.path.startsWith("/console/conversations/"));
        assert.equal(writes().length, 1, "Enter followed by blur must rename only once");
        const blurred = await edit("Renamed with Enter");
        await blurred.fill("Renamed with blur");
        await f.box.click();
        await f.page.locator("main header").getByText("Renamed with blur", { exact: true }).waitFor();
        assert.equal(writes().length, 2, "A blur-only edit must issue exactly one additional rename");
    },
    async "preferences-save-retry"(f) {
        let model = "model-one", reject = false;
        await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "test", model, ready: true, usable: true } } } }));
        await f.page.route("**/console/selectors?*", (route) => route.fulfill({ json: { model, preferred: { model }, models: [{ Value: "model-one", Label: "Model One" }, { Value: "model-two", Label: "Model Two" }], options: [] } }));
        await f.page.route("**/console/preferences", (route) => {
            const input = route.request().postDataJSON();
            f.calls.push({ method: "PUT", path: "/console/preferences", ...input });
            if (reject) return route.fulfill({ status: 503, body: "Preferences unavailable" });
            model = input.patch.model;
            return route.fulfill({ json: { ok: true, note: "下一轮以新会话开始" } });
        });
        await f.page.reload();
        const chip = f.page.getByRole("button", { name: "模型", exact: true });
        const choose = async (name) => { await chip.click(); await f.page.getByRole("menuitem", { name, exact: true }).click(); };
        await choose("Model Two");
        await eventually(async () => await chip.innerText() === "model-two", "Successful preference save must update the model chip");
        reject = true;
        await choose("Model One");
        await f.page.getByText("Preferences unavailable", { exact: true }).first().waitFor();
        assert.equal(await chip.innerText(), "model-two", "A rejected save must retain the last successful model");
        reject = false;
        await choose("Model One");
        await eventually(async () => await chip.innerText() === "model-one", "Reopening after an error must allow a successful retry");
        const writes = f.calls.filter((c) => c.path === "/console/preferences");
        assert.equal(writes.length, 3);
        assert.ok(writes.every((c) => c.conversation === A && c.agent === "test-agent"), "Preference changes must target the current conversation and agent");
    },
};

async function noHorizontalOverflow(page) {
    const size = await page.evaluate(() => ({ document: document.documentElement.scrollWidth, viewport: window.innerWidth }));
    assert.ok(size.document <= size.viewport + 1, `Document overflows horizontally: ${JSON.stringify(size)}`);
}

async function visibleControl(locator, label) {
    assert.ok(await locator.isVisible(), `${label} must be visible`);
    const rect = await locator.boundingBox();
    const viewport = locator.page().viewportSize();
    assert.ok(rect && rect.width > 20 && rect.height > 20 && rect.x >= -1 && rect.y >= -1 && rect.x + rect.width <= viewport.width + 1 && rect.y + rect.height <= viewport.height + 1, `${label} must fit in the viewport: ${JSON.stringify(rect)}`);
    assert.ok(await locator.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const top = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2);
        return top === element || element.contains(top);
    }), `${label} must not be covered by navigation or inspector`);
    return rect;
}

const inspector = (page) => page.getByRole("complementary").filter({ has: page.getByRole("tab", { name: "会话", exact: true }) });
async function composerGeometry(f) {
    await noHorizontalOverflow(f.page);
    const message = await visibleControl(f.box, "Message");
    const send = await visibleControl(f.page.locator('button[aria-label="发送"], button[aria-label="排队"]'), "Send");
    const stop = f.page.getByRole("button", { name: "停止", exact: true });
    if (await stop.isVisible()) await visibleControl(stop, "Stop");
    assert.ok(send.x >= message.x - 12 && send.x + send.width <= message.x + message.width + 12, `Send must remain in the composer column: ${JSON.stringify({ message, send })}`);
    const detail = inspector(f.page);
    if (await detail.isVisible()) {
        const panel = await detail.boundingBox();
        assert.ok(message.x + message.width <= panel.x + 1 || message.x >= panel.x + panel.width - 1, "Message must not extend into a docked inspector");
        assert.ok(send.x + send.width <= panel.x + 1 || send.x >= panel.x + panel.width - 1, "Send must not extend into a docked inspector");
    }
}

for (const width of [1280, 1024, 390]) {
    checks[`design-layout-${width}`] = async (f) => {
        await f.page.setViewportSize({ width, height: 900 });
        await f.box.fill("A draft that keeps the composer actions visible");
        await composerGeometry(f);
        await f.startRunning();
        await f.page.getByRole("button", { name: "停止", exact: true }).waitFor();
        await composerGeometry(f);
        await f.page.screenshot({ path: path.join(output, `design-layout-${width}.png`) });
    };
}

checks["design-mobile-sessions"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Mobile draft A");
    const open = f.page.getByRole("button", { name: /^(会话列表|打开会话列表)$/ });
    await visibleControl(open, "Open conversation navigation");
    await open.click();
    await visibleControl(f.page.getByRole("button", { name: /^Conversation B/ }), "Conversation B");
    await f.pick("B");
    await visibleControl(f.box, "Message after selecting B");
    assert.equal(await f.box.inputValue(), "", "Mobile selection must switch to B's own draft");
    await open.click();
    await f.pick("A");
    assert.equal(await f.box.inputValue(), "Mobile draft A");
    await open.click();
    await f.page.getByRole("button", { name: /^(关闭会话列表|关闭会话导航)$/ }).click();
    await visibleControl(f.box, "Message after dismissing conversation navigation");
    await noHorizontalOverflow(f.page);
};

checks["design-mobile-current-session"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Current mobile conversation draft");
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    const sheet = f.page.getByRole("dialog", { name: "会话列表", exact: true });
    await sheet.getByRole("button", { name: /^Conversation A/ }).click();
    await eventually(async () => await sheet.count() === 0, "Selecting the current conversation must dismiss the mobile navigation sheet");
    await visibleControl(f.box, "Message after reselecting the current conversation");
    assert.equal(await f.box.inputValue(), "Current mobile conversation draft");
};

checks["design-mobile-child"] = async (f) => {
    const tasks = [task("11", A, "scratch"), { ...task("33", A, "scratch"), parent: "11" }];
    await f.page.route("**/state", (route) => route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] } }));
    await f.page.route("**/console/replies?*", (route) => route.fulfill({ json: { enabled: true, replies: [{ id: "parent-reply", kind: "reply", conversation: A, at, text: "Parent reply", process: { steps: [{ id: "#33", kind: "delegate", goal: "Delegated work", state: "done", answer: "Child answer" }] } }] } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft before viewing a child");
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    const sheet = f.page.getByRole("dialog", { name: "会话列表", exact: true });
    await sheet.getByRole("button", { name: /^2 在跑$/ }).click();
    await sheet.getByRole("button", { name: /#33.*Task 33/ }).click();
    await eventually(async () => await sheet.count() === 0, "Opening a child task must dismiss mobile conversation navigation");
    await f.page.getByText("Child answer", { exact: true }).waitFor();
    await f.page.getByRole("button", { name: /回到对话/ }).click();
    await visibleControl(f.box, "Message after returning from a child task");
    assert.equal(await f.box.inputValue(), "Draft before viewing a child");
};

checks["design-connection-compact"] = async (f) => {
    await f.page.setViewportSize({ width: 1024, height: 900 });
    const connected = f.page.getByRole("status", { name: /^(已连接|实时)$/ });
    await connected.waitFor();
    assert.ok(await connected.isVisible(), "Compact navigation must show the connected state");
    await f.page.evaluate(() => window.sources.forEach((source) => source.onerror?.()));
    const reconnecting = f.page.getByRole("status", { name: /^(正在连接|重连中)$/ });
    await reconnecting.waitFor();
    const rect = await reconnecting.boundingBox();
    assert.ok(rect && rect.width > 0 && rect.height > 0 && rect.x >= 0 && rect.x + rect.width <= 1024 && rect.y + rect.height <= 900, "Connection changes must remain visible inside compact navigation");
};

checks["design-mobile-new-failure"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft kept after mobile creation fails");
    f.failBinding = true;
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    await f.page.getByRole("dialog", { name: "会话列表", exact: true }).getByRole("button", { name: "新会话", exact: true }).click();
    await eventually(() => f.calls.some((call) => call.input === "/project use scratch"), "Mobile project binding must begin");
    await f.page.getByRole("status").filter({ hasText: "Project binding unavailable" }).waitFor();
    await visibleControl(f.box, "Message after mobile creation failure");
    assert.equal(await f.box.inputValue(), "Draft kept after mobile creation fails");
    await f.page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
};

checks["design-active-workspace"] = async (f) => {
    await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, agents: [], project: { ...project("scratch"), bound: true }, agent: { id: "test-agent", node: "worker-west", harness: "test", model: "model-one", ready: true, usable: true, place: { node: "worker-west", kind: "copy", workspace: "scratch@worker-west" } } } } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.locator("main header").getByText(/scratch.*worker-west/).waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    const detail = inspector(f.page);
    await detail.getByText("当前工作区", { exact: true }).waitFor();
    await detail.getByText("副本 · worker-west", { exact: true }).waitFor();
    await detail.getByText("项目主机", { exact: true }).waitFor();
    assert.equal(await detail.getByText("test-node", { exact: true }).count(), 1, "Inspector must distinguish the project's host from the selected agent's workspace");
};

checks["design-inspector-toggle"] = async (f) => {
    const panel = inspector(f.page);
    const show = f.page.getByRole("button", { name: /^(显示详情|打开详情)$/ });
    const hide = f.page.getByRole("button", { name: /^(隐藏详情|关闭详情)$/ });
    if (await panel.isVisible()) await hide.first().click();
    await visibleControl(show, "Show inspector");
    await f.box.fill("Draft while inspecting");
    await show.click();
    await panel.waitFor();
    await composerGeometry(f);
    await hide.first().click();
    assert.equal(await panel.isVisible(), false, "Inspector must be explicitly dismissible");
    await f.page.setViewportSize({ width: 390, height: 844 });
    const before = await visibleControl(f.box, "Mobile message before inspector");
    const underlyingMessage = await f.box.elementHandle();
    await show.click();
    await panel.waitFor();
    await noHorizontalOverflow(f.page);
    const during = await underlyingMessage.boundingBox();
    assert.ok(Math.abs(during.width - before.width) <= 1, "Narrow-screen inspector must overlay instead of shrinking the conversation");
    const close = f.page.getByRole("button", { name: /^(关闭详情|隐藏详情)$/ });
    await visibleControl(close.last(), "Close mobile inspector");
    await close.last().click();
    await visibleControl(f.box, "Mobile message after inspector closes");
    assert.equal(await f.box.inputValue(), "Draft while inspecting");
};

checks["mcp-tool-details"] = async (f) => {
    const description = "Read the current workspace. " + "A long explanation of the available context. ".repeat(20) + "Final detail remains readable.";
    const externalName = "external_workspace_".repeat(6);
    await f.page.route("**/console/mcp", (route) => route.fulfill({ json: {
        platform: [{ name: "steve", description: "Session tools", tools: [{ name: "steve_context", description }] }],
        deployments: [{ node: "test-node", name: "example-service", type: "http", url: "https://example.test/mcp", agents: [], probe: { at, ok: true, tools: [{ name: externalName, description, input_schema: { type: "object", properties: { workspace: { type: "string" } } } }] } }],
        machines: [],
    } }));
    await f.page.getByRole("link", { name: "MCP", exact: true }).click();
    const summary = f.page.locator("summary").filter({ hasText: "steve_context" });
    await summary.waitFor();
    assert.match(await summary.innerText(), /查看当前会话/);
    const fullDescription = f.page.getByText(description, { exact: true });
    assert.equal(await fullDescription.isVisible(), false, "Long tool instructions should not crowd the initial list");
    await summary.click();
    assert.equal(await fullDescription.isVisible(), true, "Expanding a tool must expose its complete description");
    await summary.press("Enter");
    assert.equal(await fullDescription.isVisible(), false, "Keyboard must collapse the tool details");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await summary.click();
    await noHorizontalOverflow(f.page);
    await f.page.getByRole("row").filter({ hasText: "example-service" }).click();
    const drawer = f.page.getByRole("dialog", { name: "详细信息", exact: true });
    await drawer.waitFor();
    await drawer.locator("summary").filter({ hasText: externalName }).click();
    assert.equal(await drawer.getByRole("paragraph").filter({ hasText: description }).isVisible(), true, "Installed tools must also expose the full description beyond the preview");
    await drawer.getByText("输入参数", { exact: true }).waitFor();
    assert.match(await drawer.locator("pre").innerText(), /"workspace"/);
    assert.ok(await drawer.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "Long tool names must not overflow the mobile drawer");
    assert.equal(f.calls.length, 0, "Reading tool descriptions must not invoke, probe, or install services");
};

const selected = process.env.CHECK ? process.env.CHECK.split(",") : Object.keys(checks);
let failed = 0;
try {
    for (const name of selected) {
        assert.ok(checks[name], `Unknown CHECK ${name}; choices: ${Object.keys(checks).join(",")}`);
        const f = await fixture({ history: name.startsWith("scroll-"), running: name.startsWith("stop-") || name === "scroll-stream" });
        try {
            await checks[name](f);
            assert.deepEqual(f.errors, [], "Browser and mocked API errors");
            console.log(`PASS ${name}`);
        } catch (error) {
            failed++;
            await f.page.screenshot({ path: path.join(output, `${name}.png`) });
            console.error(`FAIL ${name}: ${error.message}`);
        } finally {
            for (const release of f.releases) release();
            await f.context.close();
        }
    }
} finally {
    await browser.close();
    app.close();
}
console.log(`${selected.length - failed}/${selected.length} passed; screenshots: ${output}`);
if (failed) process.exitCode = 1;
