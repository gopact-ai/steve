import { workState, workDetail, nativeHistory } from "./work-fixture.mjs";
import assert from "node:assert/strict";
import path from "node:path";
import os from "node:os";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const artifacts = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-task-completion");
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-13T01:00:00Z";
const eventually = async (predicate, message) => {
    for (let i = 0; i < 100; i++) {
        if (await predicate()) return;
        await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.fail(message);
};
const conversation = "console:completion-test";
const root = () => ({ id: "148", goal: "Accepted root task with a long title 验收完成的主任务，保留会话与原生上下文 ".repeat(3), transport: "console", channel: conversation, member: "worker", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, pending_results: 0, uncertain_results: 0, can_complete: true, turns: 2, max_turns: 0, updated_at: at });
const fixture = { task: root(), calls: [], errors: [], mode: "success", release: null, onHold: null, expect: "/tasks complete 148" };

try {
    await mkdir(artifacts, { recursive: true });
    for (const locale of ["en", "zh"]) {
        const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
        const page = await context.newPage();
        page.setDefaultTimeout(10000);
        page.on("pageerror", (error) => fixture.errors.push(String(error)));
        await page.addInitScript((language) => {
            localStorage.setItem("steve.ui.locale", language);
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        }, locale);
        await page.route("**/*", async (route) => {
            const request = route.request();
            const requestURL = new URL(request.url());
            if (requestURL.origin !== new URL(url).origin) { fixture.errors.push(`external ${requestURL.origin}`); return route.abort(); }
            const pathname = requestURL.pathname;
            if (pathname === `/console/tasks/${fixture.task.id}`) return route.fulfill({ json: workDetail(fixture.task) });
            if (pathname === "/state") return route.fulfill({ json: workState({ at, hub: { node: "local", version: "test" }, tasks: [fixture.task], nodes: [], agents: [], projects: [], plans: [], attempts: [], landings: [] }) });
            if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 1 } });
            if (pathname === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
            if (pathname === "/console/send") {
                const body = request.postDataJSON();
                fixture.calls.push(body);
                assert.equal(body.conversation, conversation);
                assert.equal(body.input, fixture.expect);
                assert.ok(body.command_id);
                if (fixture.mode === "hold") await new Promise((resolve) => { fixture.release = resolve; fixture.onHold?.(); });
                if (fixture.mode === "http-error") return route.fulfill({ status: 409, json: { error: "Execution is reserved; wait for settlement." } });
                if (fixture.mode === "reply-error") return route.fulfill({ json: { reply: { kind: "reply", text: "Results await a parent-processing receipt.", error: "Results await a parent-processing receipt." } } });
                fixture.task = { ...fixture.task, state: "done", lifecycle: "done", lane: "ended", can_complete: false };
                return route.fulfill({ json: { reply: { kind: "reply", conversation, text: locale === "en" ? "Task #148 is completed. Context preserved." : "任务 #148 已完成。上下文保留。" } } });
            }
            if (pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [] } });
            if (pathname.startsWith("/console/")) return route.fulfill({ json: { enabled: true, replies: [], verbs: [], suggestions: [], questions: [] } });
            return route.continue();
        });
        const open = async () => {
            await page.goto(`${url}console`);
            await page.getByRole("button", { name: locale === "en" ? "Board" : "看板", exact: true }).click();
            try { await page.getByRole("button", { name: /148.*Accepted root task/ }).first().click(); }
            catch (error) { await page.screenshot({ path: path.join(artifacts, "fixture-error.png") }); console.error(await page.locator("body").innerText(), fixture.errors); throw error; }
            return page.getByRole("dialog");
        };
        const completeName = locale === "en" ? "Complete task" : "完成任务";
        for (const width of [1440, 390]) {
            fixture.task = root();
            await page.setViewportSize({ width, height: 1000 });
            const dialog = await open();
            const complete = dialog.getByRole("button", { name: completeName, exact: true });
            await complete.waitFor();
            assert.equal(await complete.isEnabled(), true);
            assert.equal(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth), true);
            await page.keyboard.press("Tab");
            await complete.focus();
            assert.equal(await complete.evaluate((element) => element.matches(":focus-visible")), true);
            fixture.mode = "reply-error";
            await page.keyboard.press("Enter");
            await dialog.getByRole("alert").filter({ hasText: "parent-processing receipt" }).waitFor();
            assert.equal(await dialog.getByRole("status").count(), 0);
            fixture.mode = "http-error";
            await complete.click();
            await dialog.getByRole("alert").filter({ hasText: "reserved" }).waitFor();
            await page.screenshot({ path: path.join(artifacts, `error-${locale}-${width}.png`) });
            fixture.mode = "hold";
            fixture.release = null;
            const held = new Promise((resolve, reject) => {
                const timeout = setTimeout(() => reject(new Error("Completion request did not reach the server")), 10000);
                fixture.onHold = () => { clearTimeout(timeout); resolve(); };
            });
            const before = fixture.calls.length;
            await complete.click();
            await page.waitForFunction(() => [...document.querySelectorAll("[role=dialog] button")].some((button) => button.disabled));
            assert.equal(await complete.isEnabled(), false);
            await complete.evaluate((element) => { element.click(); element.click(); });
            await held;
            assert.equal(fixture.calls.length, before + 1);
            // Detail and Board summary are independent resources. Observe
            // each post-mutation image without requiring either to win.
            const ownerUpdated = page.waitForResponse(async (response) =>
                new URL(response.url()).pathname === "/console/tasks/148" && response.status() === 200 &&
                (await response.json()).task.lifecycle === "done");
            const summaryUpdated = page.waitForResponse(async (response) =>
                new URL(response.url()).pathname === "/state" && response.status() === 200 &&
                (await response.json()).task_coverage.completed_roots === 1);
            fixture.release();
            await Promise.all([ownerUpdated, summaryUpdated]);
            await dialog.getByRole("status").filter({ hasText: locale === "en" ? "completed" : "已完成" }).waitFor();
            await complete.waitFor({ state: "hidden" });
            assert.equal(await dialog.getByRole("alert").count(), 0);
            await page.screenshot({ path: path.join(artifacts, `completed-${locale}-${width}.png`) });
            await page.keyboard.press("Escape");
            const summary = page.getByRole("region", { name: locale === "en" ? "Root task summary" : "主任务统计" });
            await summary.filter({ hasText: locale === "en" ? /Completed\s*1/ : /已完成\s*1/ }).waitFor();
            assert.match(await summary.innerText(), locale === "en" ? /Completed\s*1/ : /已完成\s*1/);
        }
        // A failed task is usually already dealt with by the time anyone
        // looks at it. Saying so has to be possible without cancelling,
        // which would take the failure off the record, and it has to be
        // reversible.
        const settleName = { handled: locale === "en" ? "I handled it" : "我已处理", ignored: locale === "en" ? "Not worth attention" : "无需关注", reopen: locale === "en" ? "Reopen" : "重新打开", retry: locale === "en" ? "Retry" : "重试", cancel: locale === "en" ? "Cancel" : "取消" };
        for (const [command, settlement, shown] of [["handled", "handled", locale === "en" ? "Handled by hand" : "人工已处理"], ["ignore", "ignored", locale === "en" ? "Not worth attention" : "无需关注"]]) {
            const failed = { ...root(), state: "failed", lifecycle: "failed", lane: "needs_you", can_complete: false };
            fixture.task = failed;
            fixture.mode = "success";
            const dialog = await open();
            for (const name of [settleName.retry, settleName.handled, settleName.ignored, settleName.cancel]) {
                await dialog.getByRole("button", { name, exact: true }).waitFor();
            }
            assert.equal(await dialog.getByRole("button", { name: settleName.reopen, exact: true }).count(), 0);
            fixture.expect = `/tasks ${command} 148`;
            const settleCalls = fixture.calls.length;
            await dialog.getByRole("button", { name: command === "handled" ? settleName.handled : settleName.ignored, exact: true }).click();
            await eventually(() => fixture.calls.length === settleCalls + 1, "Settling must reach the server");
            fixture.task = { ...failed, settlement, lane: "ended" };
            // The record still says it failed; it just stops asking.
            await dialog.getByText(shown, { exact: true }).first().waitFor();
            await dialog.getByRole("button", { name: settleName.reopen, exact: true }).waitFor();
            for (const name of [settleName.handled, settleName.ignored, settleName.cancel]) {
                assert.equal(await dialog.getByRole("button", { name, exact: true }).count(), 0, `${name} must not be offered once settled`);
            }
            fixture.expect = "/tasks reopen 148";
            const reopenCalls = fixture.calls.length;
            await dialog.getByRole("button", { name: settleName.reopen, exact: true }).click();
            await eventually(() => fixture.calls.length === reopenCalls + 1, "Reopening must reach the server");
            fixture.task = failed;
            await dialog.getByRole("button", { name: settleName.retry, exact: true }).waitFor();
            await page.screenshot({ path: path.join(artifacts, `settled-${settlement}-${locale}.png`) });
            await page.keyboard.press("Escape");
        }
        // A task opened by a conversation never ends on its own. The board
        // card closes it by hand: completed when its results are ready to
        // accept, called off when they are not, and the dialog says which.
        const endName = locale === "en" ? "End task" : "结束任务";
        const endTitle = locale === "en" ? "End task #148?" : "结束任务 #148？";
        for (const [accepted, command] of [[true, "complete"], [false, "cancel"]]) {
            fixture.task = { ...root(), can_complete: accepted };
            fixture.mode = "success";
            fixture.expect = `/tasks ${command} 148`;
            await page.goto(`${url}console`);
            await page.getByRole("button", { name: locale === "en" ? "Board" : "看板", exact: true }).click();
            await page.getByRole("button", { name: locale === "en" ? "More actions for task #148" : "任务 #148 更多操作" }).first().click();
            await page.getByRole("menuitem", { name: endName, exact: true }).click();
            const asking = page.getByRole("dialog", { name: endTitle, exact: true });
            await asking.getByRole("button", { name: endName, exact: true }).waitFor();
            assert.match(await asking.innerText(), accepted ? /completed|已完成/ : /cancelled|已取消/, "the dialog says how the task will be recorded");
            await page.screenshot({ path: path.join(artifacts, `ending-${command}-${locale}.png`) });
            const ending = fixture.calls.length;
            await asking.getByRole("button", { name: endName, exact: true }).click();
            await eventually(() => fixture.calls.length === ending + 1, "Ending a task must reach the server");
            await eventually(async () => await asking.count() === 0, "the dialog closes once the task is ended");
            await page.screenshot({ path: path.join(artifacts, `ended-${command}-${locale}.png`) });
        }
        // A task that has already ended offers no second ending.
        fixture.task = { ...root(), state: "cancelled", lifecycle: "cancelled", lane: "ended", can_complete: false };
        await page.goto(`${url}console`);
        await page.getByRole("button", { name: locale === "en" ? "Board" : "看板", exact: true }).click();
        await page.getByRole("button", { name: locale === "en" ? "More actions for task #148" : "任务 #148 更多操作" }).first().click();
        assert.equal(await page.getByRole("menuitem", { name: endName, exact: true }).count(), 0);
        await page.keyboard.press("Escape");

        fixture.expect = "/tasks complete 148";
        for (const overrides of [{ can_complete: false }, { parent: "147" }, { execution: "running" }, { execution: "unknown" }, { attention: 1 }, { pending_results: 1 }, { uncertain_results: 1 }, { origin: "plan" }, { plan_id: "p1" }, { lifecycle: "paused" }, { lifecycle: "failed" }, { lifecycle: "cancelled" }]) {
            fixture.task = { ...root(), ...overrides };
            const dialog = await open();
            assert.equal(await dialog.getByRole("button", { name: completeName, exact: true }).count(), 0, JSON.stringify(overrides));
        }
        await context.close();
    }
    assert.deepEqual(fixture.errors, []);
    console.log("TASK COMPLETION UI PASS: en/zh, wide/narrow, keyboard, pending, retries, rejection, completion, ending a task by hand and settling a failure");
} finally {
    await browser.close();
    await server.close();
}
