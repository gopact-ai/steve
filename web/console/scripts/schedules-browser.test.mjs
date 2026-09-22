// All data and mutations are synthetic. No hub, Agent, or dispatcher is started.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import { workState } from "../../../e2e/console/work-fixture.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-schedule-ui-"));
const server = await createServer({ root, configFile: path.join(root, "vite.config.ts"), cacheDir: path.join(scratch, "cache"),
    server: { host: "127.0.0.1", port: 0, hmr: false }, logLevel: "error" });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const conversation = "console:reports";
const prompt = "检查 CI\n\n- 保留  两个空格\n- 仅报告失败";
const jobs = [];
const writes = [], errors = [];
let loseReceipt = true, refuse = false, readsFail = false;
let project = "p", terminalFailure = false, bare502 = false, wrongReceipt = false;
const receipts = new Map();
const at = "2026-09-22T09:00:00Z";
const conversations = [
    { id: conversation, title: "报告会话", project: "p", agent: "builder", count: 1, last_at: at },
    { id: "console:other", title: "其他会话", project: "p", agent: "builder", count: 1, last_at: at },
    { id: "external", title: "飞书会话", transport: "feishu", read_only: true, count: 1, last_at: at },
];
try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(30000);
    await page.addInitScript(() => {
        localStorage.setItem("steve.ui.locale", "zh");
        sessionStorage.setItem("steve.conversation", "console:other");
        window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    });
    page.on("pageerror", error => errors.push(String(error)));
    await page.route("**/*", async route => {
        const req = route.request(), url = new URL(req.url());
        if (url.origin !== origin) { errors.push("external request"); return route.abort(); }
        if (url.pathname === "/state") return route.fulfill({ json: workState({ at, hub: { node: "fixture", started: at },
            nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [], schedules: jobs,
            sources: [{ name: "schedules", wired: true }] }) });
        if (url.pathname === "/usage") return route.fulfill({ json: { at, sources: [{ name: "ledger-usage", wired: false }] } });
        if (url.pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
        if (url.pathname === "/console/desktop") return route.fulfill({ json: { enabled: false } });
        if (url.pathname === "/console/conversations") return route.fulfill(readsFail
            ? { status: 503, body: "fixture directory unavailable" }
            : { json: { enabled: true, conversations } });
        if (url.pathname === "/console/context") return route.fulfill({ json: { enabled: true, context: {
            conversation: url.searchParams.get("conversation"), project: { id: project },
            agent: { id: "builder", usable: true }, agents: [{ id: "builder", usable: true }],
        } } });
        if (url.pathname === "/console/replies") return route.fulfill({ json: { enabled: true, replies: terminalFailure
            ? [...receipts.values()].map(reply => wrongReceipt ? { ...reply, exchange_id: "unrelated" } : reply) : [] } });
        if (url.pathname === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
        if (url.pathname === "/console/suggest") return route.fulfill({ json: { suggestions: [] } });
        if (url.pathname === "/console/queue") return route.fulfill({ json: { queue: terminalFailure ? [...receipts.entries()].map(([id, reply]) => ({
            id: reply.exchange_id, key: `client:${id}`, conversation: reply.conversation, state: "failed", reply_id: reply.id,
        })) : [], submission_keys: true } });
        if (url.pathname === "/console/send") {
            const body = req.postDataJSON();
            writes.push(body);
            if (bare502) return route.fulfill({ status: 502, body: "fixture proxy unavailable" });
            if (!receipts.has(body.command_id)) {
                let text;
                if (body.input.includes("/every") || body.input.includes("/at")) {
                    if (terminalFailure) text = "fixture：终态执行失败";
                    else if (refuse) text = "fixture：时间无效，未创建定时任务";
                    else {
                        jobs.push({ id: String(jobs.length + 1), conversation: body.conversation, agent: "builder",
                            prompt, spec: "30m", next_at: at, runs: 0, state: "scheduled" });
                        text = `定时任务 #${jobs.length} 已建，下次 ${at}`;
                    }
                } else {
                    const [, action, id] = body.input.split(" ");
                    assert.equal(body.conversation, conversation, "management must target the original conversation, not the selected chat");
                    const index = jobs.findIndex(job => job.id === id);
                    if (action === "cancel") jobs.splice(index, 1);
                    if (action === "retry" || action === "confirm") jobs[index].state = "scheduled";
                    text = `fixture ${action} ${id}`;
                }
                receipts.set(body.command_id, { id: "reply-" + body.command_id, exchange_id: "exchange-" + body.command_id,
                    at, conversation: body.conversation, text, kind: "reply" });
            }
            if (loseReceipt) { loseReceipt = false; return route.abort(); }
            if (terminalFailure) return route.fulfill({ status: 502, json: { error: "fixture：终态执行失败", reply: receipts.get(body.command_id) } });
            return route.fulfill({ json: { reply: receipts.get(body.command_id) } });
        }
        if (url.pathname.startsWith("/console/")) { errors.push(`Unexpected ${req.method()} ${url.pathname}`); return route.fulfill({ status: 501, body: "Unmocked" }); }
        return route.continue();
    });
    await page.goto(`${origin}/#/console?view=board&tab=scheduled`);
    if (process.env.SCHEDULE_BEFORE_SCREENSHOT) await page.screenshot({ path: process.env.SCHEDULE_BEFORE_SCREENSHOT });
    const create = page.getByRole("button", { name: "创建定时任务", exact: true });
    try { await create.click(); } catch (error) {
        console.error(errors, await page.locator("body").innerText());
        throw error;
    } // Red: the old listing has no creation entry.
    let dialog = page.getByRole("dialog", { name: "创建定时任务", exact: true });
    const choose = async (label, option) => {
        await dialog.getByRole("button", { name: label }).click();
        await page.getByRole("option", { name: option, exact: true }).click();
    };
    await choose("目标会话", "报告会话");
    await dialog.getByRole("textbox", { name: "时间", exact: true }).fill("30m");
    await dialog.getByRole("textbox", { name: "任务内容", exact: true }).fill(prompt);
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await dialog.getByRole("alert").waitFor();
    assert.equal(jobs.length, 1);
    assert.equal(writes[0].conversation, conversation);
    assert.equal(writes[0].input, "@builder /every 30m " + prompt);
    assert.equal(writes[0].expected_project, "p", "creation freezes the displayed project");
    assert.equal(await dialog.getByRole("textbox", { name: "任务内容", exact: true }).inputValue(), prompt);
    assert.ok(await dialog.getByRole("textbox", { name: "任务内容", exact: true }).isDisabled());
    // The same immutable operation is retained across a reload after a lost receipt.
    await page.reload();
    dialog = page.getByRole("dialog", { name: "创建定时任务", exact: true });
    // Restored submissions must revalidate their target before writing. The
    // old receipt is retained, not redirected to the currently selected chat.
    const original = conversations[0];
    for (const replacement of [
        { ...original, read_only: true },
        { ...original, transport: "feishu", read_only: false },
        { ...original, id: "console:replacement" },
    ]) {
        conversations[0] = replacement;
        await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
        await dialog.getByRole("alert").filter({ hasText: "原目标会话" }).waitFor();
        assert.equal(writes.length, 1, "invalid original target must not receive a POST");
    }
    conversations[0] = original;
    readsFail = true;
    await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "fixture directory unavailable" }).waitFor();
    assert.equal(writes.length, 1, "directory failure must fail closed");
    readsFail = false;
    await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
    await dialog.getByRole("status").filter({ hasText: "定时任务 #1 已建" }).waitFor();
    assert.equal(writes[0].command_id, writes[1].command_id);
    assert.equal(jobs.length, 1);
    assert.ok(await dialog.getByRole("button", { name: "再创建一项", exact: true }).isVisible());
    assert.equal(await dialog.getByRole("button", { name: "修改后重新提交", exact: true }).count(), 0);
    await dialog.getByRole("button", { name: "关闭", exact: true }).click();
    await page.getByRole("button", { name: "刷新列表", exact: true }).click();
    const row = page.getByRole("row").filter({ hasText: prompt.split("\n")[0] });
    await row.waitFor();
    assert.equal(await row.getByRole("link", { name: "打开原会话" }).getAttribute("href"), "#/console?conversation=console%3Areports");
    await row.getByRole("button", { name: "取消定时任务", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "取消定时任务", exact: true });
    await dialog.getByRole("button", { name: "确认取消", exact: true }).click();
    await dialog.getByRole("status").filter({ hasText: "fixture cancel 1" }).waitFor();
    await dialog.getByRole("button", { name: "关闭", exact: true }).click();
    assert.equal(jobs.length, 0);
    assert.equal(writes.at(-1).input, "/schedules cancel 1");

    // A server refusal is shown verbatim, never replaced by a fake success toast.
    refuse = true;
    await create.click();
    dialog = page.getByRole("dialog", { name: "创建定时任务", exact: true });
    await choose("目标会话", "报告会话");
    await choose("执行方式", "仅一次");
    await dialog.getByRole("textbox", { name: "时间", exact: true }).fill("yesterday");
    await dialog.getByRole("textbox", { name: "任务内容", exact: true }).fill(prompt);
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await dialog.getByRole("status").filter({ hasText: "时间无效，未创建" }).waitFor();
    assert.equal(writes.at(-1).input, "@builder /at yesterday " + prompt);
    assert.equal(jobs.length, 0);
    await dialog.getByRole("button", { name: "再创建一项", exact: true }).click();
    assert.equal(await dialog.getByRole("textbox", { name: "任务内容", exact: true }).inputValue(), "");
    await dialog.getByRole("textbox", { name: "任务内容", exact: true }).fill(prompt.repeat(30));
    await page.setViewportSize({ width: 390, height: 700 });
    await page.keyboard.press("Tab");
    assert.ok(await dialog.evaluate(el => el.contains(document.activeElement)), "keyboard focus stays in dialog");
    assert.ok(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth + 1), "long fields wrap at narrow width");
    if (process.env.SCHEDULE_SCREENSHOT) await page.screenshot({ path: process.env.SCHEDULE_SCREENSHOT });
    await dialog.getByRole("button", { name: "取消", exact: true }).click();

    await page.setViewportSize({ width: 1280, height: 900 });
    await create.click();
    dialog = page.getByRole("dialog", { name: "创建定时任务", exact: true });
    await choose("目标会话", "报告会话");
    await dialog.getByRole("textbox", { name: "时间", exact: true }).fill("30m");
    await dialog.getByRole("textbox", { name: "任务内容", exact: true }).fill(prompt);
    // Drift after the form resolved p must not silently create in q.
    const beforeDrift = writes.length;
    project = "q"; conversations[0] = { ...original, project };
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "项目已变更" }).waitFor();
    assert.equal(writes.length, beforeDrift);
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "项目已变更" }).waitFor();
    assert.equal(writes.length, beforeDrift, "failed preflight must not silently refresh the frozen project");
    project = "p"; conversations[0] = original;
    // A transport 502 without a correlated terminal receipt stays uncertain.
    bare502 = true;
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "fixture proxy unavailable" }).waitFor();
    assert.ok(await dialog.getByRole("button", { name: "重试原请求", exact: true }).isVisible());
    const failedID = writes.at(-1).command_id;
    await page.reload();
    dialog = page.getByRole("dialog", { name: "创建定时任务", exact: true });
    project = "q";
    const beforeRetry = writes.length;
    await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "项目已变更" }).waitFor();
    assert.equal(writes.length, beforeRetry, "a restored, never accepted operation must not drift");
    project = "p";
    bare502 = false; terminalFailure = true;
    wrongReceipt = true;
    await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
    await dialog.getByRole("alert").filter({ hasText: "fixture：终态执行失败" }).waitFor();
    assert.ok(await dialog.getByRole("button", { name: "重试原请求", exact: true }).isVisible(), "unrelated receipt must not unlock the operation");
    wrongReceipt = false;
    await dialog.getByRole("button", { name: "重试原请求", exact: true }).click();
    await dialog.getByRole("status").filter({ hasText: "fixture：终态执行失败" }).waitFor();
    assert.equal(writes.at(-1).command_id, failedID);
    assert.equal(writes.at(-1).expected_project, "p");
    assert.equal(await page.evaluate(() => sessionStorage.getItem("steve.schedule.pending")), null);
    await dialog.getByRole("button", { name: "再创建一项", exact: true }).click();
    assert.ok(await dialog.getByRole("textbox", { name: "任务内容", exact: true }).isEnabled());
    await dialog.getByRole("button", { name: "取消", exact: true }).click();
    terminalFailure = false;

    // Both uncertainty-resolution controls retain the original conversation.
    for (const [action, label] of [["confirm", "确认已执行"], ["retry", "授权重试"]]) {
        jobs.splice(0, jobs.length, { id: "uncertain", conversation, agent: "builder", prompt,
            spec: "30m", next_at: at, runs: 0, state: "unknown" });
        await page.getByRole("button", { name: "刷新列表", exact: true }).click();
        await page.getByRole("row").filter({ hasText: prompt.split("\n")[0] }).getByRole("button", { name: label, exact: true }).click();
        dialog = page.getByRole("dialog", { name: label, exact: true });
        await dialog.getByRole("button", { name: label, exact: true }).click();
        await dialog.getByRole("status").filter({ hasText: `fixture ${action} uncertain` }).waitFor();
        assert.equal(writes.at(-1).input, `/schedules ${action} uncertain`);
        await dialog.getByRole("button", { name: "关闭", exact: true }).click();
    }
    assert.deepEqual(errors, []);
    console.log("PASS scheduled creation, scoped management, project freeze/reload drift guard, correlated terminal 502 recovery, uncertain 502 retention, keyboard and narrow layout");
} finally {
    await browser.close();
    await server.close();
    await rm(scratch, { recursive: true, force: true });
}
