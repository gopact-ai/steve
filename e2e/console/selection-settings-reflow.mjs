import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { createServer as createHTTPServer } from "node:http";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState, nativeHistory } from "./work-fixture.mjs";

// Real app and browser; all backend reads/writes stay inside synthetic fixtures.
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const dist = process.env.SELECTION_REFLOW_DIST === "1";
const server = dist ? createHTTPServer(async (req, res) => {
    try {
        const pathname = new URL(req.url, "http://localhost").pathname;
        const file = path.join(web, "../../internal/readmodel/web/dist", pathname === "/" ? "index.html" : pathname);
        const body = await readFile(file);
        res.setHeader("Content-Type", file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : file.endsWith(".png") ? "image/png" : "text/html");
        res.end(body);
    } catch { res.writeHead(404); res.end(); }
}) : await createServer({ root: web, configFile: path.join(web, "vite.config.ts"), server: { host: "127.0.0.1", port: 0 } });
if (dist) await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
else await server.listen();
const origin = dist ? `http://127.0.0.1:${server.address().port}/` : server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1280, height: 720 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(10000);
const conversation = "console:selection-reflow", at = "2026-09-07T01:00:00Z", base = "a".repeat(40), commit = "b".repeat(40);
const source = Array.from({ length: 1000 }, (_, i) => `const line${i + 1} = ${i + 1};`).join("\n");
const changes = { attempt: "attempt1", project: "p", base, artifact: commit, files: 1, added: 2, deleted: 1 };
const replies = Array.from({ length: 1000 }, (_, i) => ({
    id: `r${i + 1}`, conversation, kind: "reply", at, project_id: "p", revision: "reply-version-1",
    text: `History reply ${i + 1}. Reading the immutable source must not lose its selection when this background history follows new content.`,
    ...(i === 999 ? { changes } : {}),
}));
const errors = [], writes = [], observations = [];
let replyReads = 0;
page.on("pageerror", error => errors.push(String(error)));
await context.route("**/*", async route => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (url.origin !== new URL(origin).origin) { errors.push("Unexpected external request"); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (req.method() !== "GET") { writes.push({ method: req.method(), path: p }); return route.fulfill({ status: 500, json: { error: "Unexpected write" } }); }
    const json = value => route.fulfill({ json: value });
    if (p === "/state") return json(workState({ at, hub: { node: "test-hub", version: "test" }, nodes: [], agents: [], projects: [{ id: "p", node: "test-hub", path: "/work/p", repo: "inplace", level: "public", agents: [], workspaces: [] }], tasks: [], plans: [], attempts: [], landings: [] }));
    if (p === "/console/desktop") return json({ enabled: false, setup_required: false, agent_count: 0 });
    if (p === "/console/coordination") return json({ enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false });
    if (p === "/console/context") return json({ enabled: true, context: { conversation, project: { id: "p", node: "test-hub", path: "/work/p", repo: "inplace", level: "public", bound: true }, agents: [] } });
    if (p === "/console/setup") return json({ enabled: true });
    if (p === "/console/conversations") return json({ conversations: [{ id: conversation, title: "Reflow conversation", project: "p", count: replies.length, last_at: at, running: false }] });
    if (p === "/console/replies") { replyReads++; return json({ enabled: true, replies }); }
    if (p === "/console/queue") return json({ queue: [], submission_keys: true, material_refs: true, interactive_requests: true });
    if (p === "/console/verbs") return json({ verbs: [] });
    if (p === "/console/suggest") return json({ suggestions: [] });
    if (p === "/console/questions") return json({ questions: [] });
    if (p === "/console/materials") return json({ materials: [] });
    if (p === "/console/annotations") return json({ annotations: [] });
    if (p === "/console/attempts") return json(nativeHistory([{ id: "attempt1", kind: "task", state: "done", base, artifact: commit, started_at: at, files: 1 }]));
    if (p === "/console/attempts/attempt1/changes") return json({ ...changes, changes: [{ path: "app.ts", status: "M", added: 2, deleted: 1 }] });
    if (p === "/console/attempts/attempt1/tree") return json({ attempt: "attempt1", commit, which: "result", dir: "", entries: Array.from({ length: 32 }, (_, i) => ({ name: i ? `other-${i}.ts` : "app.ts", path: i ? `other-${i}.ts` : "app.ts", kind: "file", size: source.length })) });
    if (p === "/console/attempts/attempt1/file") return json({ attempt: "attempt1", commit, path: "app.ts", text: source, size: source.length });
    if (p === "/console/attempts/attempt1/diff") return json({ path: "app.ts", diff: "--- a/app.ts\n+++ b/app.ts\n@@ -1 +1 @@\n-old\n+new\n" });
    errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
await context.addInitScript(conversation => {
    sessionStorage.setItem("steve.conversation", conversation);
    if (!localStorage.getItem("steve.ui.locale")) localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { addEventListener() {} constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    window.reflowEvents = [];
    const snapshot = event => {
        const target = event.target, bar = document.querySelector(".selection-toolbar"), surface = bar?.closest("[data-selection-surface]");
        window.reflowEvents.push({ at: performance.now(), type: event.type, class: target instanceof Element ? target.className : String(target),
            hidden: target instanceof Element && !!target.closest('[hidden],[aria-hidden="true"],[inert]'),
            affectsSurface: !!surface && target instanceof Element && (surface.contains(target) || target.contains(surface)),
            top: target instanceof Element ? target.scrollTop : null, bar: !!bar, key: event.key, trusted: event.isTrusted });
    };
    for (const type of ["scroll", "resize", "storage", "pointerdown", "pointerup", "selectionchange", "focusin", "focusout"]) window.addEventListener(type, snapshot, true);
}, conversation);
const bar = page.getByRole("toolbar", { name: "Selection actions", exact: true });
const artifacts = process.env.SELECTION_REFLOW_ARTIFACTS;
async function select() {
    await page.bringToFront();
    await page.locator('.source-number[data-line="2"]').click();
    await page.locator('.source-number[data-line="3"]').click({ modifiers: ["Shift"] });
    await bar.getByText("Selected L2–L3", { exact: true }).waitFor();
    await page.evaluate(() => { window.reflowEvents = []; window.reflowBar = document.querySelector(".selection-toolbar"); });
}
async function retained(label) {
    assert.ok(await bar.isVisible(), label);
    assert.ok(await bar.evaluate(el => el === window.reflowBar), "same toolbar survives " + label);
    assert.deepEqual(await page.locator('.source-number[aria-pressed="true"]').evaluateAll(els => els.map(el => el.dataset.line)), ["2", "3"]);
}
try {
    await page.goto(origin + "#/console");
    await page.getByRole("heading", { name: "Reflow conversation", exact: true }).waitFor();
    // Disable Blink scroll anchoring for this fixture so its immediate anchor
    // adjustment does not cancel the production auto-follow before the poll.
    // The transcript still scrolls only through the real controller/layout effect.
    await page.addStyleTag({ content: ".transcript-scroll { overflow-anchor: none; }" });
    await page.getByRole("button", { name: "Show details", exact: true }).click();
    await page.getByRole("tab", { name: "Artifacts", exact: true }).click();
    await page.getByRole("button", { name: "Browse files", exact: true }).click();
    await page.getByRole("button", { name: "app.ts", exact: true }).first().click();
    await page.locator('.source-number[data-line="3"]').waitFor();
    assert.equal(await page.locator(".source-line").count(), 1000);
    const settings = await context.newPage();
    const size = settings.getByRole("button", { name: /Interface size/ });
    await settings.goto(origin + "#/settings?section=appearance");
    async function font(n) {
        await settings.bringToFront(); await size.click();
        await settings.getByRole("option", { name: `${n} px`, exact: true }).click();
        await page.waitForFunction(n => getComputedStyle(document.documentElement).getPropertyValue("--ui-font-size").trim() === `${n}px`, n);
    }
    await font(14); await select();
    const before = await page.locator(".transcript-scroll").evaluate(el => ({ top: el.scrollTop, height: el.scrollHeight, client: el.clientHeight, hidden: !!el.closest('[hidden],[aria-hidden="true"],[inert]') }));
    assert.ok(before.top > 20000, "a long history is already following at the bottom");
    assert.ok(before.hidden, "Review makes the mounted transcript inert/hidden");
    const reads = replyReads;
    await font(18);
    // The real controller polls in the background. Its production auto-follow
    // layout effect scrolls the hidden transcript after the Settings reflow.
    await page.waitForFunction(() => window.reflowEvents.some(e => e.type === "scroll" && String(e.class).includes("transcript-scroll") && e.hidden && !e.affectsSurface), undefined, { timeout: 20000 });
    await page.waitForTimeout(200);
    assert.ok(replyReads > reads, "normal history polling drives the delayed auto-follow");
    const scroll = await page.evaluate(() => window.reflowEvents.find(e => e.type === "scroll" && String(e.class).includes("transcript-scroll") && e.hidden && !e.affectsSurface));
    console.log("Hidden history auto-follow after real Settings", JSON.stringify({ before, scroll, readsBefore: reads, readsAfter: replyReads }));
    await retained("unrelated hidden history auto-follow must not dismiss Source selection");
    observations.push(await page.evaluate(() => ({ stage: "after-18-history-poll", events: window.reflowEvents, sourceTop: document.querySelector(".source-scroll").scrollTop, same: document.querySelector(".selection-toolbar") === window.reflowBar, pressed: [...document.querySelectorAll('.source-number[aria-pressed="true"]')].map(el => el.dataset.line) })));
    if (artifacts) { await mkdir(artifacts, { recursive: true }); await page.screenshot({ path: path.join(artifacts, (dist ? "dist" : "source") + "-selected-18.png") }); }
    await font(14); await page.waitForTimeout(200);
    await retained("return Settings font reflow");
    assert.ok(await page.locator('.selection-toolbar button,.source-view [data-material-actions] button[aria-haspopup="true"]').evaluateAll(buttons => buttons.every(el => { const r = el.getBoundingClientRect(); return el.contains(document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2)); })), "reflow keeps inline Actions and floating buttons reachable");
    await page.bringToFront();
    const navigator = await page.locator(".code-tree:visible").boundingBox();
    await page.mouse.move(navigator.x + navigator.width / 2, navigator.y + navigator.height - 20);
    await page.mouse.wheel(0, 100);
    await page.waitForFunction(() => document.querySelector(".code-tree").scrollTop > 0);
    await page.waitForTimeout(100);
    await retained("unrelated visible navigator scroll");
    observations.push(await page.evaluate(() => ({ stage: "visible-navigator-scroll", events: window.reflowEvents })));
    await page.keyboard.press("Shift+F10");
    assert.ok(await bar.evaluate(el => el.contains(document.activeElement)), "toolbar keyboard focus still works");
    await page.keyboard.press("Escape"); await bar.waitFor({ state: "detached" });
    await select(); await page.locator(".review-title h1").click(); await bar.waitFor({ state: "detached" });
    await select(); const region = await page.locator(".source-scroll").boundingBox();
    await page.mouse.move(region.x + region.width / 2, region.y + region.height - 20); await page.mouse.wheel(0, 100);
    await bar.waitFor({ state: "detached" });
    assert.ok(await page.locator(".source-scroll").evaluate(el => el.scrollTop > 0), "real Source wheel scrolls and dismisses");
    await select(); await page.setViewportSize({ width: 1281, height: 720 }); await bar.waitFor({ state: "detached" });
    // Make a containing reader overflow in this isolated fixture. Its native
    // program scroll must dismiss without changing Source's own scroll position.
    await page.locator(".review-code-scroll").evaluate(el => {
        const surface = el.querySelector("[data-selection-surface]");
        window.reflowAncestorStyle = [el.style.cssText, surface.style.cssText];
        el.style.overflow = "auto"; surface.style.flex = "none"; surface.style.minHeight = `${el.clientHeight + 100}px`;
    });
    await select(); await page.locator(".review-code-scroll").evaluate(el => { el.scrollTop = 40; });
    await bar.waitFor({ state: "detached" });
    assert.ok(await page.evaluate(() => window.reflowEvents.some(e => e.type === "scroll" && e.class === "review-code-scroll" && e.affectsSurface && e.trusted)), "native containing-ancestor scroll dismisses");
    observations.push(await page.evaluate(() => ({ stage: "native-ancestor-scroll", events: window.reflowEvents })));
    await page.locator(".review-code-scroll").evaluate(el => {
        el.style.cssText = window.reflowAncestorStyle[0]; el.querySelector("[data-selection-surface]").style.cssText = window.reflowAncestorStyle[1];
    });
    await page.waitForTimeout(100);
    await page.evaluate(() => {
        window.reflowRootStyle = [document.documentElement.style.cssText, document.body.style.cssText];
        for (const el of [document.documentElement, document.body]) { el.style.height = "1600px"; el.style.overflow = "auto"; }
    });
    await select(); await page.evaluate(() => window.scrollTo(0, 100));
    await page.waitForFunction(() => window.scrollY > 0); await bar.waitFor({ state: "detached" });
    assert.ok(await page.evaluate(() => window.reflowEvents.some(e => e.type === "scroll" && e.class === "[object HTMLDocument]" && e.trusted)), "native viewport/document scroll dismisses");
    observations.push(await page.evaluate(() => ({ stage: "native-document-scroll", events: window.reflowEvents, top: scrollY })));
    await page.evaluate(() => {
        document.documentElement.style.cssText = window.reflowRootStyle[0]; document.body.style.cssText = window.reflowRootStyle[1]; window.scrollTo(0, 0);
    });
    await page.waitForTimeout(100);
    // Supplement native document scrolling with routing checks for both viewport targets.
    for (const target of ["document", "window"]) {
        await select(); await page.evaluate(target => (target === "document" ? document : window).dispatchEvent(new Event("scroll")), target);
        await bar.waitFor({ state: "detached" });
    }
    assert.deepEqual(writes, [], "regression performs no backend writes"); assert.deepEqual(errors, []);
    console.log("PASS Settings hidden-history scroll preserves selection; Source/ancestor/document/window/resize/outside/Escape dismissal retained (" + (dist ? "dist" : "source") + ")");
} finally {
    if (artifacts) {
        await mkdir(artifacts, { recursive: true });
        await writeFile(path.join(artifacts, (dist ? "dist" : "source") + "-trace.json"), JSON.stringify({ observations, events: await page.evaluate(() => window.reflowEvents), replyReads, writes, errors }, null, 2));
        await page.screenshot({ path: path.join(artifacts, (dist ? "dist" : "source") + "-final.png") });
    }
    await context.close(); await browser.close();
    if (dist) await new Promise(resolve => server.close(resolve)); else await server.close();
}
