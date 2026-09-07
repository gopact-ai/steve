// Source preview with isolated API fixtures; no live coordinator or dist build.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { mkdir } from "node:fs/promises";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(7000);
const conversation = "console:question-ui", at = "2026-09-07T01:00:00Z";
const explanation = "I tried the integration checks on build-node. Configuration checks and unit tests passed.\n\nThe integration environment cannot be reached from this machine. Its network access is restricted, so retrying here will not resolve the problem.\n\nI recommend running the remaining integration checks on dev-box. The completed checks do not need to run again. Should I use dev-box?";
const makeQuestion = (id, extra = {}) => ({ id, conversation, exchange_id: "e1", task_id: "11", kind: "question", title: "Continue integration checks", message: explanation, options: [{ id: "dev-box", label: "Use dev-box", description: "Run the remaining integration checks." }, { id: "wait", label: "Wait for network access", description: "Continue once access is restored." }], required: true, created_at: at, deadline: "2030-01-01T00:00:00Z", updated_at: at, state: "pending", ...extra });
const f = { questions: [makeQuestion("q-original")], answers: [], queue: [], errors: [], holdAnswer: false, releaseAnswer: null, reset: false, malformed: false, staleReads: false, questionReads: 0, activeState: "" };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    const input = req.method() === "GET" ? null : req.postDataJSON();
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "dev-box", version: "test" }, nodes: [], agents: [], projects: [{ id: "p", node: "dev-box", path: "/work/p", repo: "inplace", level: "public", agents: [], workspaces: [] }], tasks: [], plans: [], attempts: [], landings: [] } });
    if (p === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation, project: { id: "p", node: "dev-box", path: "/work/p", repo: "inplace", level: "public", bound: true }, agents: [] } } });
    if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [{ id: "r1", conversation, kind: "reply", at, text: "Configuration checks and unit tests are complete.", project_id: "p", revision: "r1" }] } });
    if (p === "/console/conversations") return route.fulfill({ json: { conversations: [{ id: conversation, title: "Integration checks", project: "p", count: 1, last_at: at, running: false }] } });
    if (p === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
    if (p === "/console/suggest") return route.fulfill({ json: { suggestions: [] } });
    if (p === "/console/queue") {
        if (req.method() !== "GET") { f.queue.push(input); return route.fulfill({ status: 500, json: { error: "A question response must not create work" } }); }
        return route.fulfill({ json: { queue: f.activeState ? [{ id: "retained", conversation, input: "Continue the original task", state: f.activeState, enqueued_at: at, started_at: at }] : [], submission_keys: true, material_refs: true, interactive_requests: true } });
    }
    if (p === "/console/annotations") return route.fulfill({ json: { annotations: [] } });
    if (p === "/console/questions") { f.questionReads++; return route.fulfill({ json: { questions: f.questions.map((q) => f.staleReads ? { ...q, state: "pending", answer: undefined } : q) } }); }
    if (/^\/console\/questions\/[^/]+\/answer$/.test(p)) {
        const id = decodeURIComponent(p.split("/")[3]);
        f.answers.push({ id, ...input });
        if (f.holdAnswer) await new Promise((resolve) => { f.releaseAnswer = resolve; });
        if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
        if (f.malformed) { f.malformed = false; return route.fulfill({ json: {} }); }
        const q = f.questions.find((item) => item.id === id);
        q.state = input.decision === "accept" ? "answered" : input.decision === "decline" ? "declined" : "cancelled";
        q.answer = input;
        return route.fulfill({ json: { question: q } });
    }
    f.errors.push(req.method() + " " + p);
    return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
await page.addInitScript((id) => {
    sessionStorage.setItem("steve.conversation", id);
    localStorage.setItem("steve.ui.locale", "en");
    window.sources = [];
    window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((source) => source !== this); } };
    window.emit = (event) => window.sources.forEach((source) => source.onmessage?.({ data: JSON.stringify(event) }));
}, conversation);
async function waitFor(test, label) { for (let i = 0; i < 100; i++) { if (await test()) return; await new Promise((resolve) => setTimeout(resolve, 30)); } assert.fail(label); }
async function show(q) { f.questions = [q]; await page.evaluate((event) => window.emit(event), { kind: "console.question", conversation, text: q.id, at }); await page.getByRole("heading", { name: q.title, exact: true }).waitFor(); }
async function screenshot(name) { if (!process.env.QUESTION_SCREENSHOTS) return; await mkdir(process.env.QUESTION_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.QUESTION_SCREENSHOTS, name + ".png"), animations: "disabled" }); }

try {
    await page.goto(url + "#/console");
    await page.getByRole("heading", { name: "Integration checks", exact: true }).waitFor();
    const panel = page.getByRole("region", { name: "Your response is needed" });
    await panel.getByRole("heading", { name: "Continue integration checks" }).waitFor();
    await screenshot("question-desktop-light");
    assert.ok((await panel.innerText()).includes("retrying here will not resolve the problem"));
    const composer = page.getByRole("textbox", { name: "Message", exact: true });
    await composer.fill("A separate follow-up draft");
    f.holdAnswer = true;
    const useNode = panel.getByRole("button", { name: "Use dev-box", exact: true });
    await useNode.focus();
    await page.keyboard.press("Enter");
    await waitFor(() => f.answers.length === 1, "one option answers the original question directly");
    await page.keyboard.press("Enter");
    assert.equal(f.answers.length, 1);
    assert.equal(await panel.getByText("Answered", { exact: true }).count(), 0);
    assert.equal(f.answers[0].id, "q-original");
    assert.equal(f.answers[0].choice, "dev-box");
    assert.equal(f.answers[0].decision, "accept");
    f.staleReads = true;
    f.holdAnswer = false; f.releaseAnswer();
    await panel.getByText("1 recent resolved requests", { exact: true }).waitFor();
    await panel.getByText("1 recent resolved requests", { exact: true }).click();
    await panel.getByText("Answered", { exact: true }).waitFor();
    await panel.getByText("Use dev-box", { exact: true }).waitFor();
    const staleRead = page.waitForResponse((response) => new URL(response.url()).pathname === "/console/questions");
    await page.evaluate((event) => window.emit(event), { kind: "console.question", conversation, at });
    await staleRead;
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await panel.getByRole("button", { name: "Use dev-box", exact: true }).count(), 0, "an acknowledged answer is not reopened by stale pending reads");
    assert.equal(await composer.inputValue(), "A separate follow-up draft");
    assert.deepEqual(f.queue, []);
    f.staleReads = false;
    console.log("PASS single choice replies directly, awaits acknowledgement and preserves history without creating work");

    for (const [state, label] of [["interrupted", "The session was interrupted; this request can no longer be answered."], ["cancelled", "Cancelled"]]) {
        const recovered = makeQuestion(`q-recovered-${state}`, { title: `Restore unanswered ${state} request`, allow_free_text: true });
        await show(recovered);
        await panel.getByRole("button", { name: "Give another answer", exact: true }).click();
        await panel.getByRole("textbox", { name: "Answer", exact: true }).fill("Keep my recovery answer draft");
        const before = f.answers.length;
        f.questions = [{ ...recovered, state }];
        await page.evaluate((event) => window.emit(event), { kind: "console.question", conversation, at });
        await panel.getByText("1 recent resolved requests", { exact: true }).click();
        await panel.getByText(label, { exact: true }).waitFor();
        assert.equal(await panel.getByRole("textbox", { name: "Answer", exact: true }).count(), 0);
        f.questions = [recovered];
        await page.evaluate((event) => window.emit(event), { kind: "console.question", conversation, at });
        const restoredInput = panel.getByRole("textbox", { name: "Answer", exact: true });
        await restoredInput.waitFor();
        assert.equal(await restoredInput.inputValue(), "Keep my recovery answer draft");
        assert.equal(await panel.getByText("1 recent resolved requests", { exact: true }).count(), 0);
        assert.equal(f.answers.length, before, "recovery never synthesizes a user response");
    }
    console.log("PASS recovered unanswered requests reopen under the same ID and retain answer drafts");

    await show(makeQuestion("q-later", { title: "Choose when to continue" }));
    await panel.getByRole("button", { name: "Answer later", exact: true }).click();
    await panel.getByText("Still awaiting your reply. The original question remains open.", { exact: true }).waitFor();
    assert.equal(f.questions[0].state, "pending");
    assert.equal(f.answers.length, 1);
    await page.reload();
    await panel.getByRole("button", { name: "Reply now", exact: true }).waitFor();
    await panel.getByRole("button", { name: "Reply now", exact: true }).click();
    await panel.getByRole("button", { name: "Wait for network access", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).click();
    await panel.getByText("Wait for network access", { exact: true }).waitFor();
    assert.equal(f.answers.at(-1).choice, "wait");
    assert.deepEqual(f.queue, []);
    console.log("PASS postponing keeps the pending question and an explicit wait choice answers the original request");

    await show(makeQuestion("q-retry", { title: "Retry without losing identity" }));
    f.reset = true;
    await panel.getByRole("button", { name: "Use dev-box", exact: true }).click();
    await panel.getByRole("button", { name: "Retry this response", exact: true }).waitFor();
    const retry = f.answers.at(-1);
    await page.reload();
    await panel.getByRole("button", { name: "Retry this response", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).waitFor();
    assert.deepEqual(f.answers.at(-1), retry);
    console.log("PASS uncertain response survives reload and reuses the exact command identity");

    await show(makeQuestion("q-malformed", { title: "Check the response acknowledgement" }));
    f.malformed = true;
    await panel.getByRole("button", { name: "Use dev-box", exact: true }).click();
    await panel.getByRole("button", { name: "Retry this response", exact: true }).waitFor();
    assert.equal(await panel.getByText("Answered", { exact: true }).count(), 0);
    const malformed = f.answers.at(-1);
    await panel.getByRole("button", { name: "Retry this response", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).waitFor();
    assert.deepEqual(f.answers.at(-1), malformed);
    console.log("PASS malformed success responses never imply the user decision was saved");

    await show(makeQuestion("q-free", { title: "Provide another arrangement", allow_free_text: true }));
    await panel.getByRole("button", { name: "Give another answer", exact: true }).click();
    const answerInput = panel.getByRole("textbox", { name: "Answer", exact: true });
    const custom = "  Use the existing VPN connection on dev-box.\nPlease keep the integration output.  ";
    await answerInput.fill(custom);
    await panel.getByRole("button", { name: "Back to options", exact: true }).click();
    await panel.getByRole("button", { name: "Give another answer", exact: true }).click();
    assert.equal(await answerInput.inputValue(), custom);
    await page.reload();
    await answerInput.waitFor();
    assert.equal(await answerInput.inputValue(), custom);
    assert.equal(await composer.inputValue(), "A separate follow-up draft");
    f.reset = true;
    await panel.getByRole("button", { name: "Submit response", exact: true }).click();
    await panel.getByRole("button", { name: "Retry this response", exact: true }).waitFor();
    const freeAnswer = f.answers.at(-1);
    assert.equal(freeAnswer.text, custom);
    assert.equal(Object.hasOwn(freeAnswer, "choice"), false);
    await page.reload();
    await panel.getByRole("button", { name: "Retry this response", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).click();
    await panel.getByText(custom, { exact: true }).waitFor();
    assert.deepEqual(f.answers.at(-1), freeAnswer);
    assert.equal(await composer.inputValue(), "A separate follow-up draft");
    assert.deepEqual(f.queue, []);
    console.log("PASS schema-supported free answers keep their own drafts, content and retry identity");

    await show(makeQuestion("q-text", { title: "Describe the target", options: [], allow_free_text: true }));
    await panel.getByRole("button", { name: "Submit response", exact: true }).click();
    await panel.getByText("Choose an option or enter a response.", { exact: true }).waitFor();
    assert.equal(await answerInput.evaluate((el) => document.activeElement === el), true);
    await answerInput.fill("dev-box");
    await panel.getByRole("button", { name: "Submit response", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).waitFor();
    assert.equal(f.answers.at(-1).text, "dev-box");
    console.log("PASS a text-only question validates its own reply and focuses the missing answer");

    await show(makeQuestion("q-permission", { title: "Approve a tool", kind: "permission", options: [{ id: "allow_once", label: "Allow once" }, { id: "reject_once", label: "Reject once", kind: "reject_once" }] }));
    assert.equal(await panel.getByRole("textbox").count(), 0);
    await panel.getByText("Allow once", { exact: true }).click();
    await panel.getByRole("button", { name: "Decline", exact: true }).click();
    await panel.getByText("1 recent resolved requests", { exact: true }).waitFor();
    assert.equal(f.answers.at(-1).decision, "decline");
    assert.equal(Object.hasOwn(f.answers.at(-1), "choice"), false);
    console.log("PASS permission choices cannot be replaced by unconstrained text");

    await show(makeQuestion("q-narrow", { title: "A decision with long context", message: explanation + "\n\n" + "Long-environment-hostname-".repeat(30) }));
    await page.getByRole("link", { name: "Coordinated by dev-box", exact: true }).first().waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    await panel.getByRole("button", { name: "Use dev-box", exact: true }).focus();
    assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("aria-label") || document.activeElement?.textContent), "Use dev-box");
    await waitFor(async () => { const box = await composer.boundingBox(); return box && box.y >= 0 && box.y + box.height <= 844; }, "the main composer remains reachable");
    assert.ok(await panel.evaluate((el) => el.scrollWidth <= el.clientWidth));
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    await screenshot("question-narrow-light");
    await page.evaluate(() => document.documentElement.classList.add("dark-mode"));
    await page.waitForTimeout(180); // Allow the existing theme color transition to finish before visual review.
    await screenshot("question-narrow-dark");
    await page.setViewportSize({ width: 1440, height: 1000 });
    await screenshot("question-desktop-dark");
    console.log("PASS narrow layout, long content, keyboard focus and coordinator wording");
    await show(makeQuestion("q-recovery", { title: "Recovery question", kind: "recovery", deadline: "0001-01-01T00:00:00Z" }));
    assert.equal(await panel.locator(".question-deadline").count(), 0, "durable recovery questions have no expiry");
    for (const [state, label] of [["recovering", "Recovering execution…"], ["awaiting-user", "Awaiting your reply"]]) {
        f.activeState = state;
        await page.evaluate((event) => window.emit(event), { kind: "console.queue", conversation, at });
        await page.locator(".console-status").getByText(label, { exact: true }).waitFor();
        assert.equal(await page.getByRole("button", { name: "Send", exact: true }).count(), 0, "recovery remains an active original execution");
    }
    f.activeState = "";
    console.log("PASS retained exchanges remain active and recovery questions have no automatic expiry");
    f.questions = [makeQuestion("q-zh", { title: "继续集成验证", message: "配置校验和单元测试已完成。我尝试在 build-node 运行集成验证，但无法访问项目内网；本机没有可用的网络凭据，重试仍无法解决。\n\n建议改到 dev-box 继续验证，保留已完成的结果。要这样继续吗？", allow_free_text: true, options: [{ id: "dev-box", label: "改到 dev-box 继续", description: "保留已完成的验证结果。" }, { id: "wait", label: "等待内网恢复" }] })];
    await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "zh"); localStorage.setItem("ui-theme", "dark"); });
    await page.reload();
    await page.getByRole("button", { name: "改到 dev-box 继续", exact: true }).waitFor();
    await screenshot("question-desktop-zh");
    await page.getByRole("button", { name: "给出其他安排", exact: true }).click();
    await page.getByRole("textbox", { name: "答复", exact: true }).fill("先用 dev-box 验证；不重跑已完成的测试。");
    await page.setViewportSize({ width: 390, height: 844 });
    await waitFor(async () => { const button = await page.getByRole("button", { name: "提交答复", exact: true }).boundingBox(); return button && button.y >= 0 && button.y + button.height <= 844; }, "the question submit button stays visible in a narrow window");
    await screenshot("question-free-text-narrow-zh");
    assert.deepEqual(f.queue, []);
    assert.deepEqual(f.errors, []);
} catch (error) {
    console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText());
    throw error;
} finally {
    await context.close(); await browser.close(); await server.close();
}
