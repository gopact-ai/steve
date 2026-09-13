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
const conversation = "console:completion-test";
const root = () => ({ id: "148", goal: "Accepted root task with a long title 验收完成的主任务，保留会话与原生上下文 ".repeat(3), channel: conversation, member: "worker", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, pending_results: 0, uncertain_results: 0, can_complete: true, turns: 2, max_turns: 0, updated_at: at });
const fixture = { task: root(), calls: [], errors: [], mode: "success", release: null, onHold: null };

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
            if (pathname === "/state") return route.fulfill({ json: { at, hub: { node: "local", version: "test" }, tasks: [fixture.task], nodes: [], agents: [], projects: [], plans: [], attempts: [], landings: [] } });
            if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 1 } });
            if (pathname === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
            if (pathname === "/console/send") {
                const body = request.postDataJSON();
                fixture.calls.push(body);
                assert.equal(body.conversation, conversation);
                assert.equal(body.input, "/tasks complete 148");
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
            fixture.release();
            await dialog.getByRole("status").filter({ hasText: locale === "en" ? "completed" : "已完成" }).waitFor();
            await complete.waitFor({ state: "hidden" });
            assert.equal(await dialog.getByRole("alert").count(), 0);
            await page.screenshot({ path: path.join(artifacts, `completed-${locale}-${width}.png`) });
            await page.keyboard.press("Escape");
            const summary = page.getByRole("region", { name: locale === "en" ? "Root task summary" : "主任务统计" });
            assert.match(await summary.innerText(), locale === "en" ? /Completed\s*1/ : /已完成\s*1/);
        }
        for (const overrides of [{ can_complete: false }, { parent: "147" }, { execution: "running" }, { execution: "unknown" }, { attention: 1 }, { pending_results: 1 }, { uncertain_results: 1 }, { origin: "plan" }, { plan_id: "p1" }, { lifecycle: "paused" }, { lifecycle: "failed" }, { lifecycle: "cancelled" }]) {
            fixture.task = { ...root(), ...overrides };
            const dialog = await open();
            assert.equal(await dialog.getByRole("button", { name: completeName, exact: true }).count(), 0, JSON.stringify(overrides));
        }
        await context.close();
    }
    assert.deepEqual(fixture.errors, []);
    console.log("TASK COMPLETION UI PASS: en/zh, wide/narrow, keyboard, pending, retries, rejection and completion");
} finally {
    await browser.close();
    await server.close();
}
