// Uses an already installed Playwright (PLAYWRIGHT_MODULE may be its
// absolute module path). No npm dependency or running hub is modified.
// TOKEN enables screenshots of console:fleet-demo through the read-only proxy.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { preview } from "./preview.mjs";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || "playwright");
const out = fileURLToPath(new URL("./screenshots/", import.meta.url));
await mkdir(out, { recursive: true });
const app = await preview();
const browser = await chromium.launch({ args: ["--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage"] });
const errors = [];
const page = await browser.newPage({ viewport: { width: 1600, height: 1100 }, deviceScaleFactor: 1 });
page.on("pageerror", (e) => errors.push(String(e)));

try {
    if (process.env.TOKEN) {
        await page.addInitScript(() => sessionStorage.setItem("steve.conversation", "console:fleet-demo"));
        await page.goto(`${app.url}/?token=${encodeURIComponent(process.env.TOKEN)}#/console`);
        for (const id of ["#59", "#60"]) {
            const card = page.locator(`[data-task-id="${id}"]`).first();
            await card.waitFor();
            await card.locator(":scope > summary").click();
            const thought = card.locator("details.group\\/think");
            assert.equal(await thought.getAttribute("open"), null);
            await thought.locator("summary").click();
            const text = await thought.locator(".md").textContent();
            assert.ok(text.length > 100, `${id}: retained reasoning`);
            await card.scrollIntoViewIfNeeded();
            await page.screenshot({ path: `${out}fleet-${id.slice(1)}.png` });
            console.log(`PASS real ${id}: completed card expands reasoning (${text.length} characters) and tools`);
        }
    }
    await page.close();

    const test = await browser.newPage({ viewport: { width: 1600, height: 1100 }, deviceScaleFactor: 1 });
    test.on("pageerror", (e) => errors.push(String(e)));
    const at = "2026-09-05T10:00:00Z";
    const conversation = "console:childcard-check";
    let child = {
        id: "#59", kind: "delegate", agent: "builder", node: "node-a", model: "child-model",
        goal: "编译并验证发布产物，保留完整过程。", state: "running", since: at, elapsed: "1m20s",
        reasoning: "开始：先检查项目结构。\n\n" + Array.from({ length: 90 }, (_, i) => `第 ${i + 1} 步：检查构建、校验与发布说明，保留每一步的完整摘要。`).join("\n\n") + "\n\n初始结尾。",
        plan: [{ text: "检查项目", status: "completed" }, { text: "编译与验证", status: "in_progress" }],
        tools: Array.from({ length: 16 }, (_, i) => ({ id: `t${i}`, name: "execute", kind: "execute", status: i === 15 ? "running" : "completed", input: JSON.stringify({ cmd: `printf 'check ${i + 1}\\n'` }), output: `check ${i + 1}` })),
    };
    let replies = [
        { id: "sent-1", conversation, kind: "sent", at, input: "请委派构建任务，完成后再告诉我。" },
        { id: "parent-reply", exchange_id: "parent", conversation, kind: "reply", at, text: "已交给 builder，子任务完成后继续。", process: { reasoning: "父回合已结束。", steps: [child, { id: "plan-only", agent: "planner", node: "node-a", reasoning: "纯计划步骤保留原来的 Trace。", tools: [{ id: "p1", name: "inspect", status: "completed" }] }] } },
    ];
    let queue = [];
    const context = { conversation, agents: [], project: { id: "scratch", node: "node-a", path: "/scratch", bound: true } };
    await test.route("**/console/**", (route) => {
        const pathname = new URL(route.request().url()).pathname;
        const body = pathname === "/console/replies" ? { enabled: true, replies }
            : pathname === "/console/queue" ? { queue }
            : pathname === "/console/conversations" ? { enabled: true, conversations: [{ id: conversation, title: "子任务跨回合验证", running: !!queue.length, count: replies.length, last_at: at }] }
            : pathname === "/console/context" ? { enabled: true, context }
            : { verbs: [], suggestions: [] };
        return route.fulfill({ json: body });
    });
    await test.route("**/state*", (route) => route.fulfill({ json: { at, hub: {}, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } }));
    await test.addInitScript((c) => {
        sessionStorage.setItem("steve.conversation", c);
        window.sources = [];
        window.EventSource = class {
            constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
            close() { window.sources = window.sources.filter((s) => s !== this); }
        };
        window.emit = (ev) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(ev) }); };
    }, conversation);
    const emit = async (ev) => {
        await test.evaluate((ev) => window.emit(ev), { at, conversation, ...ev });
        await test.waitForTimeout(100);
    };
    const updateChild = async (kind, changes) => {
        child = { ...child, ...changes };
        replies[1].process.steps[0] = child;
        await emit(kind === "console.step"
            ? { kind, task_id: "59", reply_id: "parent-reply", step: child }
            : { kind, task_id: "59", step_id: "#59", step: { kind: "delegate", state: child.state, goal: child.goal }, progress: child });
    };
    await test.goto(`${app.url}/#/console`);
    const card = test.locator('[data-task-id="#59"]').first();
    const thought = card.locator("details.group\\/think");
    const scroll = thought.locator(":scope > div");
    await scroll.waitFor();
    await test.waitForFunction(() => {
        const el = document.querySelector('[data-task-id="#59"] details.group\\/think > div');
        return el && el.scrollHeight - el.clientHeight - el.scrollTop < 2;
    });
    assert.ok(await thought.evaluate((el) => el.open));
    assert.ok((await card.textContent()).includes("builder @ node-a · child-model"));
    await updateChild("delegate.progress", { reasoning: child.reasoning + "\n\n流式新增：自动跟到这一行。" });
    let position = await scroll.evaluate((el) => ({ top: el.scrollTop, height: el.scrollHeight, view: el.clientHeight }));
    assert.ok(position.height - position.view - position.top < 2, "follow the tail before manual scroll");
    await scroll.evaluate((el) => { el.scrollTop = 120; el.dispatchEvent(new Event("scroll")); });
    await updateChild("console.step", { reasoning: child.reasoning + "\n\n用户正在读前文时的新内容。" });
    assert.ok(Math.abs(await scroll.evaluate((el) => el.scrollTop) - 120) < 2, "preserve manual scroll during streaming");
    await card.scrollIntoViewIfNeeded();
    await test.screenshot({ path: `${out}running.png` });
    console.log("PASS running child after parent reply: full reasoning, plan/model/tools, automatic tail, manual scroll preserved");

    queue = [{ id: "new-turn", conversation, state: "running", input: "新回合", started_at: at, enqueued_at: at }];
    await emit({ kind: "console.sent", exchange_id: "new-turn", reply_id: "sent-2", text: "新回合" });
    await emit({ kind: "console.progress", exchange_id: "new-turn", progress: { agent: "parent", model: "parent-model", reasoning: "新回合自己的思考。" } });
    await updateChild("delegate.progress", { reasoning: child.reasoning + "\n\n旧子任务仍在原卡片更新。" });
    assert.equal(await test.locator('[data-task-id="#59"]').count(), 1, "old child must not enter the new turn's trace");
    assert.ok((await card.textContent()).includes("旧子任务仍在原卡片更新"));
    assert.equal(await scroll.evaluate((el) => el.scrollTop), 120);
    await updateChild("console.step", {
        state: "done", answer: "编译、校验通过，发布产物已准备好。", elapsed: "2m10s", refs: ["artifact:release"],
        attempt: "attempt-59", files: 2,
        tools: child.tools.map((t) => ({ ...t, status: "completed" })),
        plan: child.plan.map((p) => ({ ...p, status: "completed" })),
    });
    assert.equal(await thought.evaluate((el) => el.open), false, "completion folds reasoning");
    await card.locator(":scope > summary").click();
    await thought.locator("summary").click();
    const full = await thought.locator(".md").textContent();
    assert.ok(full.includes("开始：先检查项目结构") && full.includes("旧子任务仍在原卡片更新"));
    assert.equal(await card.locator("details.group\\/row").count(), 16, "retain every tool row");
    const lastTool = card.locator("details.group\\/row").last();
    await lastTool.locator(":scope > summary").click();
    assert.ok((await lastTool.textContent()).includes("check 16"), "last tool input and output remain expandable");
    assert.ok((await card.textContent()).includes(child.answer));
    assert.ok((await card.textContent()).includes("artifact:release"));
    await card.scrollIntoViewIfNeeded();
    await test.screenshot({ path: `${out}completed-during-new-turn.png` });
    console.log("PASS old child completes during new turn: no misattribution; full thought, 16 tools, answer, changes and refs retained");

    queue = [];
    replies.push({ id: "new-reply", exchange_id: "new-turn", conversation, kind: "reply", at, text: "新回合完成。" });
    await emit({ kind: "console.reply", exchange_id: "new-turn", reply_id: "new-reply", text: "新回合完成。" });
    await test.reload();
    await card.waitFor();
    assert.equal(await thought.evaluate((el) => el.open), false);
    await card.locator(":scope > summary").click();
    await thought.locator("summary").click();
    assert.equal(await thought.locator(".md").textContent(), full, "refresh restores the full durable child snapshot");
    await test.getByRole("button", { name: "细节", exact: true }).first().click();
    const railCard = test.locator('aside [data-task-id="#59"]');
    await railCard.waitFor();
    await railCard.locator(":scope > summary").click();
    await railCard.locator("details.group\\/think > summary").click();
    assert.equal(await railCard.locator("details.group\\/think .md").textContent(), full);
    assert.equal(await railCard.locator("details.group\\/row").count(), 16);
    assert.ok((await test.locator("aside").last().textContent()).includes("纯计划步骤保留原来的 Trace"));
    await card.scrollIntoViewIfNeeded();
    await test.screenshot({ path: `${out}restored-process.png` });
    console.log("PASS refresh and ProcessBody: durable snapshot restored; same DelegationCard; plain plan step unchanged");
    assert.deepEqual(errors, [], "browser errors");
    console.log("CHILDCARD PASS (0 browser errors)");
} finally {
    await browser.close();
    app.close();
}
