// Run with an existing Playwright installation via PLAYWRIGHT_MODULE.
// TOKEN enables read-only screenshots of the live legacy conversations.
// PNGs stay in the ignored screenshots directory; stdout is the PR evidence.
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
const newPage = async () => {
    const page = await browser.newPage({ viewport: { width: 1600, height: 1100 }, deviceScaleFactor: 1 });
    page.on("pageerror", (e) => errors.push(String(e)));
    return page;
};

try {
    if (process.env.TOKEN) {
        for (const conversation of ["console:journal-demo", "console:e2e-auto-20260905T091349Z-e675fe07"]) {
            const page = await newPage();
            await page.addInitScript((c) => sessionStorage.setItem("steve.conversation", c), conversation);
            await page.goto(`${app.url}/?token=${encodeURIComponent(process.env.TOKEN)}#/console`);
            const data = await (await page.request.get(`${app.url}/console/replies?token=${encodeURIComponent(process.env.TOKEN)}&conversation=${encodeURIComponent(conversation)}`)).json();
            const replies = data.replies.filter((r) => r.kind === "reply");
            assert.ok(replies.length > 0);
            await page.locator('[data-task-id]').first().waitFor();
            if (conversation === "console:journal-demo") {
                for (const id of ["#72", "#73"]) {
                    const card = page.locator(`[data-task-id="${id}"]`).first();
                    const stored = replies.flatMap((r) => r.process?.steps || []).find((s) => s.id === id);
                    assert.ok(stored && !stored.timeline, "live hub should still supply legacy snapshots");
                    await card.locator(":scope > summary").click();
                    const fold = card.locator("details.group\\/think");
                    assert.equal(await fold.evaluate((el) => el.open), false);
                    await fold.locator("summary").click();
                    assert.ok((await fold.locator(".md").textContent()).length > 0);
                    assert.equal(await card.locator('[data-timeline]').count(), 0);
                    await card.scrollIntoViewIfNeeded();
                    await page.screenshot({ path: `${out}timeline-legacy-${id.slice(1)}.png` });
                    console.log(`PASS legacy ${conversation} ${id}: saved thought/tool folds retained; no timeline inferred`);
                }
            } else {
                assert.ok(data.replies.filter((r) => r.kind === "sent" && (r.input || "").includes("子任务")).length >= 2, "two continuations retained");
                await page.getByRole("button", { name: "细节", exact: true }).last().click();
                await page.screenshot({ path: `${out}timeline-legacy-continuations.png` });
                console.log(`PASS legacy autonomous conversation: ${replies.length} replies, parent and two continuations rendered`);
            }
            await page.close();
        }
    }

    const page = await newPage();
    const conversation = "console:timeline-fixture";
    const at = "2026-09-05T12:00:00Z";
    const span = (kind, value) => ({ kind, [kind === "tool" ? "tool" : "text"]: value, at });
    const tool = (id, kind, name, input = {}, extra = {}) => ({ id, kind, name, input: JSON.stringify(input), status: "completed", ...extra });
    let child = {
        id: "#72", kind: "delegate", state: "done", goal: "检查项目、修改说明并验证结果。", agent: "builder", node: "node-a", model: "child-model", elapsed: "2m10s",
        reasoning: "legacy aggregated thought", answer: "legacy aggregate answer must not be the final child answer", refs: ["artifact:release"],
        tools: [
            tool("r1", "", "Read", { path: "README.md" }), tool("r2", "read", "Read", { path: "go.mod" }), tool("r3", "read", "Read", { path: "main.go" }),
            tool("x1", "", "run", { cmd: "go build ./..." }), tool("x2", "execute", "run", { cmd: "go test ./..." }),
            tool("e1", "", "Edit", { file_path: "/scratch/README.md" }),
            tool("d1", "execute", "mcp.steve.steve_delegate", {}, { output: '{"state":"failed"}' }),
            tool("p1", "execute", "mcp.steve.steve_fleet"),
        ],
        timeline: [
            span("text", "先检查仓库与构建入口。"), span("thought", "确认这几份文件的关系。"),
            ...["r1", "r2", "r3", "x1", "x2"].map((id) => span("tool", id)),
            span("text", "检查结束，接下来更新文档。"), span("tool", "e1"), span("thought", "把验证交给子任务。"),
            span("tool", "d1"), span("text", "委派失败后查一下平台名册。"), span("tool", "p1"), span("text", "最终答复：文档已更新，构建与测试通过。"),
        ],
    };
    const parent = {
        id: "parent", kind: "reply", conversation, at,
        text: "父回复正文仍使用服务端的整段 Answer，保持这段文字原样。",
        process: {
            tools: [tool("parent-r", "read", "Read"), tool("parent-x", "execute", "run")],
            timeline: [span("text", "父过程：先检查目标。"), span("tool", "parent-r"), span("tool", "parent-x"), span("text", "父过程：已经完成委派。")],
            steps: [child],
        },
    };
    const replies = [{ id: "sent", kind: "sent", at, conversation, input: "完成项目检查并报告结果。", text: "" }, parent];
    let version = "v1";
    await page.route("**/console/**", (route) => {
        const pathname = new URL(route.request().url()).pathname;
        const body = pathname === "/console/replies" ? { enabled: true, replies }
            : pathname === "/console/queue" ? { submission_keys: true, queue: [] }
            : pathname === "/console/conversations" ? { enabled: true, conversations: [{ id: conversation, title: "过程时间线验证", count: 2, last_at: at }] }
            : pathname === "/console/context" ? { enabled: true, context: { conversation, agents: [], project: { id: "scratch", node: "node-a", path: "/scratch", bound: true } } }
            : { verbs: [], suggestions: [] };
        return route.fulfill({ json: body });
    });
    await page.route("**/state*", (route) => route.fulfill({ json: { at, hub: { version }, nodes: [], agents: [], tasks: [], plans: [], projects: [] } }));
    await page.addInitScript((conversation) => {
        sessionStorage.setItem("steve.conversation", conversation);
        sessionStorage.setItem("timeline-loads", String(Number(sessionStorage.getItem("timeline-loads") || 0) + 1));
        window.sources = [];
        window.EventSource = class {
            constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
            close() { window.sources = window.sources.filter((s) => s !== this); }
        };
        window.emit = (ev) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(ev) }); };
    }, conversation);
    const emit = async (event) => { await page.evaluate((ev) => window.emit(ev), { at, conversation, ...event }); await page.waitForTimeout(100); };
    const update = async (patch) => {
        child = { ...child, ...patch };
        parent.process.steps[0] = child;
        await emit({ kind: "console.step", task_id: "72", reply_id: "parent", step: child });
    };
    await page.goto(`${app.url}/#/console`);
    const processFold = page.locator("details.group\\/process").first();
    await processFold.waitFor();
    assert.equal(await processFold.evaluate((el) => el.open), false);
    assert.ok(await page.getByText(parent.text, { exact: true }).isVisible());
    await processFold.locator(":scope > summary").click();
    const card = page.locator('[data-task-id="#72"]').first();
    await card.locator(":scope > summary").click();
    const timeline = card.locator("[data-timeline]");
    assert.deepEqual(await timeline.locator(":scope > *").evaluateAll((nodes) => nodes.map((n) => n.dataset.spanKind)), ["text", "thought", "tool", "text", "tool", "thought", "tool", "text", "tool"]);
    const groups = timeline.locator("details.group\\/activity");
    const headings = await groups.locator(":scope > summary").allTextContents();
    assert.ok(headings[0].includes("读了 3 个文件、跑了 2 条命令"));
    assert.ok(headings[1].includes("改了 README.md"));
    assert.ok(headings[2].includes("委派了一个子任务") && headings[2].includes("有失败"));
    assert.ok(headings[3].includes("问了平台一次"));
    assert.ok((await groups.nth(2).locator(":scope > summary").getAttribute("class")).includes("text-error-primary"));
    assert.equal(await groups.first().locator("details.group\\/calls").isVisible(), false);
    await groups.first().locator(":scope > summary").click();
    assert.equal(await groups.first().locator("details.group\\/row").count(), 5);
    const final = "最终答复：文档已更新，构建与测试通过。";
    assert.equal(await timeline.getByText(final, { exact: true }).count(), 0);
    assert.equal(await card.getByText(final, { exact: true }).count(), 1);
    assert.equal(await card.getByText(child.answer, { exact: true }).count(), 0);
    const parentTimeline = processFold.locator("[data-timeline]").last();
    assert.deepEqual(await parentTimeline.locator(":scope > *").evaluateAll((nodes) => nodes.map((n) => n.dataset.spanKind)), ["text", "tool", "text"]);
    await groups.first().locator(":scope > summary").click();
    await card.scrollIntoViewIfNeeded();
    await page.screenshot({ path: `${out}timeline-completed.png` });
    console.log("PASS fixture: narration/thought/activity order, mixed categories, file name, delegation/platform wording, failed group, expandable tool details");
    console.log("PASS answers: child lifts only final text; parent keeps the complete server Answer");

    await page.getByRole("button", { name: "细节", exact: true }).click();
    const railCard = page.locator('aside [data-task-id="#72"]');
    await railCard.locator(":scope > summary").click();
    assert.deepEqual(await railCard.locator("details.group\\/activity > summary").allTextContents(), headings);
    await page.screenshot({ path: `${out}timeline-process-sidebar.png` });
    console.log("PASS process sidebar: same ordered timeline and activities as the child card");

    // Both event forms update a child after the parent has already replied.
    const growing = [span("text", "进行中的叙述保留在原位置。"), span("thought", Array.from({ length: 30 }, (_, i) => `检查第 ${i + 1} 项`).join("\n"))];
    await update({ state: "running", timeline: growing });
    const thought = card.locator('[data-span-kind="thought"]');
    await thought.waitFor();
    await page.waitForFunction(() => {
        const el = document.querySelector('[data-task-id="#72"] [data-span-kind="thought"]');
        return el && el.scrollHeight - el.clientHeight - el.scrollTop < 2;
    });
    assert.ok(await thought.evaluate((el) => el.clientHeight <= 20));
    assert.equal(await card.getByText("它说", { exact: true }).count(), 0);
    growing[1] = span("thought", growing[1].text + "\n新的结尾");
    child = { ...child, timeline: growing };
    parent.process.steps[0] = child;
    await emit({ kind: "delegate.progress", task_id: "72", step_id: "#72", progress: child, step: { kind: "delegate", state: "running" } });
    assert.ok(await thought.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop < 2));
    await thought.evaluate((el) => { el.scrollTop = 40; el.dispatchEvent(new Event("scroll")); });
    await update({ timeline: [growing[0], span("thought", growing[1].text + "\n手动滚动后仍保留位置")] });
    assert.equal(await thought.evaluate((el) => el.scrollTop), 40);
    await page.screenshot({ path: `${out}timeline-streaming.png` });
    console.log("PASS streaming: final thought stays one line, follows tail until manual scroll, both child event types update it");

    const loads = await page.evaluate(() => Number(sessionStorage.getItem("timeline-loads")));
    version = "v2";
    await emit({ kind: "console.meta" });
    await page.waitForFunction((n) => Number(sessionStorage.getItem("timeline-loads")) > n, loads);
    await page.locator("textarea").first().waitFor();
    await page.waitForTimeout(500);
    const afterReload = await page.evaluate(() => Number(sessionStorage.getItem("timeline-loads")));
    assert.equal(afterReload, loads + 1, "reload exactly once for a version change");
    await page.locator("textarea").first().fill("保留我的草稿");
    version = "v3";
    await emit({ kind: "console.meta" });
    await page.getByRole("status").filter({ hasText: "hub 已更新" }).waitFor();
    assert.equal(await page.locator("textarea").first().inputValue(), "保留我的草稿");
    assert.equal(await page.evaluate(() => Number(sessionStorage.getItem("timeline-loads"))), afterReload);
    await page.screenshot({ path: `${out}timeline-update-with-draft.png` });
    await page.locator("textarea").first().fill("");
    await page.waitForFunction((n) => Number(sessionStorage.getItem("timeline-loads")) > n, afterReload);
    console.log("PASS hub version: empty composer reloads once; draft shows refresh notice and survives; clearing draft reloads");
    assert.deepEqual(errors, []);
    console.log("TIMELINE PASS (0 browser errors; screenshots kept locally, not committed)");
} finally {
    await browser.close();
    app.close();
}
