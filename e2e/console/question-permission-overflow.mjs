// Run with: node e2e/console/question-permission-overflow.mjs
// Source-only browser regression: private Vite cache, synthetic APIs, no hub.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState } from "./work-fixture.mjs";

const root = fileURLToPath(new URL("../../web/console", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-permission-overflow-"));
const fixtureID = path.join(root, "__permission_overflow_entry.tsx");
const conversation = "console:permission-overflow";
const at = "2026-09-21T00:00:00Z";
const command = "echo_" + "abcdefghijklmnopqrstuvwxyz0123456789".repeat(12);
const options = [
    { id: "once", label: "Allow once" },
    { id: "session", label: `Allow this command for the session: ${command}`, description: "Only this command is approved. Review the complete command before continuing. ".repeat(5) },
    { id: "reject", label: "Reject this command", description: "Do not run it.", kind: "reject_once" },
];
const fixture = `
import React from "react";
import { createRoot } from "react-dom/client";
import { FleetProvider } from "@/lib/fleet";
import { LocaleProvider } from "@/providers/locale-provider";
import { QuestionPanel } from "@/components/steve/question-panel";
import "@/styles/globals.css";
createRoot(document.getElementById("root")).render(
  <LocaleProvider><FleetProvider><main style={{ padding: 12 }}>
    <button id="before">Before approval</button>
    <QuestionPanel conversation="${conversation}" />
    <button id="after">After approval</button>
  </main></FleetProvider></LocaleProvider>
);
`;
const errors = [], failures = [], samples = [];
let server, browser;
try {
    server = await createServer({
        root, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        server: { host: "127.0.0.1", port: 0 },
        plugins: [{
            name: "permission-overflow-fixture",
            resolveId(id) { if (id === "/__permission_overflow_entry.tsx" || id === fixtureID) return fixtureID; },
            load(id) { if (id === fixtureID) return fixture; },
            configureServer(vite) {
                vite.middlewares.use("/__permission_overflow", async (_req, res) => {
                    res.setHeader("Content-Type", "text/html");
                    res.end(await vite.transformIndexHtml("/__permission_overflow", '<html><body><div id="root"></div><script type="module" src="/__permission_overflow_entry.tsx"></script></body></html>'));
                });
            },
        }],
    });
    await server.listen();
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }, { width: 320, height: 568 }, { width: 844, height: 390 }]) {
        for (const fontSize of [14, 18]) {
            const name = `${viewport.width}x${viewport.height}-ui${fontSize}`;
            const context = await browser.newContext({ viewport, serviceWorkers: "block", reducedMotion: "reduce" });
            try {
                const page = await context.newPage();
                page.setDefaultTimeout(15000);
                page.on("pageerror", (error) => { errors.push(`${name}: ${error}`); console.error(`${name}: ${error}`); });
                page.on("console", (message) => {
                    if (message.type() === "warning" && /controlled|uncontrolled/i.test(message.text())) errors.push(`${name}: ${message.text()}`);
                });
                let question = { id: name, conversation, kind: "permission", title: "Approve command", message: "Review the options before responding.", options, required: true, state: "pending", created_at: at, updated_at: at };
                const answers = [];
                await page.addInitScript(() => {
                    localStorage.setItem("steve.ui.locale", "en");
                    window.EventSource = class { addEventListener() {} constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
                });
                await context.route("**/*", (route) => {
                    const req = route.request(), url = new URL(req.url()), p = url.pathname;
                    if (url.origin !== origin) { errors.push(`${name}: external request ${url.origin}`); return route.abort(); }
                    if (req.method() === "POST" && p === `/console/questions/${name}/answer`) {
                        answers.push(req.postDataJSON());
                        question = { ...question, state: "answered", answer: answers.at(-1) };
                        return route.fulfill({ json: { question } });
                    }
                    if (req.method() !== "GET") { errors.push(`${name}: unexpected mutation ${p}`); return route.abort(); }
                    if (p === "/console/questions") return route.fulfill({ json: { questions: [question] } });
                    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true, material_refs: true, interactive_requests: true } });
                    if (p === "/state") return route.fulfill({ json: workState({ at, hub: { node: "fixture", version: "fixture" }, nodes: [], projects: [], agents: [], tasks: [], plans: [], attempts: [], landings: [] }) });
                    if (p.startsWith("/console/") || p === "/events") { errors.push(`${name}: unhandled API ${p}`); return route.abort(); }
                    return route.continue();
                });
                await page.goto(`${origin}/__permission_overflow`, { waitUntil: "networkidle" });
                const panel = page.locator(".question-panel");
                const list = panel.getByRole("radiogroup");
                await list.waitFor();
                await page.evaluate((size) => document.documentElement.style.setProperty("--ui-font-size", `${size}px`), fontSize);
                await page.evaluate(() => document.fonts.ready);
                const geometry = await list.evaluate((el) => {
                    const rows = [...el.querySelectorAll(".question-radio")];
                    return {
                        height: el.clientHeight, scrollHeight: el.scrollHeight,
                        horizontalOverflow: el.scrollWidth - el.clientWidth,
                        rows: rows.map((row) => {
                            const box = row.getBoundingClientRect(), style = getComputedStyle(row);
                            const copy = row.querySelector(".question-option-copy").getBoundingClientRect();
                            return {
                                height: box.height, copyHeight: copy.height,
                                fontSize: style.fontSize,
                                topOverflow: box.top + parseFloat(style.paddingTop) + parseFloat(style.borderTopWidth) - copy.top,
                                bottomOverflow: copy.bottom + parseFloat(style.paddingBottom) + parseFloat(style.borderBottomWidth) - box.bottom,
                                rightOverflow: copy.right + parseFloat(style.paddingRight) + parseFloat(style.borderRightWidth) - box.right,
                            };
                        }),
                    };
                });
                samples.push({ name, ...geometry });
                const check = (condition, message) => { if (!condition) failures.push(`${name}: ${message}`); };
                assert.equal(geometry.rows[0].fontSize, `${fontSize}px`, "the real UI size token is applied");
                check(geometry.rows.every((row) => row.topOverflow <= 1 && row.bottomOverflow <= 1 && row.rightOverflow <= 1), `copy must fit inside each padded radio border: ${JSON.stringify(geometry.rows)}`);
                check(geometry.rows[1].height > geometry.rows[0].height + 40, "long option grows naturally instead of matching short rows");
                check(geometry.scrollHeight > geometry.height + 1, "the bounded options list scrolls");
                check(geometry.horizontalOverflow <= 1, "unbroken commands wrap without horizontal list overflow");
                check(await panel.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "panel has no horizontal overflow");
                check(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "page has no horizontal overflow");
                if (process.env.QUESTION_SCREENSHOTS) {
                    await mkdir(process.env.QUESTION_SCREENSHOTS, { recursive: true });
                    await page.screenshot({ path: path.join(process.env.QUESTION_SCREENSHOTS, `${name}.png`), animations: "disabled" });
                }

                // No pointer interaction, seeded choice, or direct radio focus:
                // a fresh request must be reachable using only Tab.
                const radios = list.getByRole("radio");
                assert.equal(await list.locator("input:checked").count(), 0, `${name}: no default choice`);
                assert.deepEqual(answers, [], `${name}: no default approval`);
                await page.keyboard.press("Tab");
                assert.equal(await page.locator("#before").evaluate((el) => document.activeElement === el), true, `${name}: Tab starts at the preceding control`);
                // Chromium may first tab to a scrollable ancestor.
                for (let i = 0; i < 6; i++) {
                    await page.keyboard.press("Tab");
                    if (await radios.first().evaluate((el) => document.activeElement === el)) break;
                }
                const initialFocus = await radios.evaluateAll((inputs) => inputs.map((input) => ({ value: input.value, tabIndex: input.tabIndex, checked: input.checked, focused: document.activeElement === input })));
                console.log(`${name}: initial keyboard entry ${JSON.stringify(initialFocus)}`);
                assert.equal(initialFocus[0].focused, true, `${name}: Tab enters unselected options without a mouse`);
                assert.equal(await list.locator("input:checked").count(), 0, `${name}: focus alone does not choose`);
                assert.deepEqual(answers, [], `${name}: focus alone does not approve`);
                assert.equal(await page.evaluate((id) => sessionStorage.getItem(`steve.question.answer:${id}`), name), null, `${name}: focus does not save a choice or pending answer`);
                assert.equal(await list.locator(".question-radio").first().getAttribute("data-focus-visible"), "true");
                await page.keyboard.press("Space");
                assert.equal(await radios.first().isChecked(), true);
                // Read the long option with real wheel input, not scrollTop assignment.
                await list.hover();
                await page.mouse.wheel(0, 240);
                await page.waitForFunction(() => document.querySelector(".question-permission-options").scrollTop > 0);
                await page.keyboard.press("ArrowDown");
                assert.equal(await radios.nth(1).isChecked(), true);
                assert.equal(await list.locator(".question-radio").nth(1).getAttribute("data-focus-visible"), "true");
                await page.keyboard.press("ArrowDown");
                assert.equal(await radios.last().isChecked(), true);
                const lastSeen = await list.evaluate((el) => {
                    const last = el.querySelector(".question-radio:last-child .question-option-label").getBoundingClientRect(), box = el.getBoundingClientRect();
                    return { last: last.toJSON(), box: box.toJSON(), scrollTop: el.scrollTop };
                });
                check(lastSeen.last.top >= lastSeen.box.top - 1 && lastSeen.last.bottom <= lastSeen.box.bottom + 1 && lastSeen.scrollTop > 0, `keyboard navigation reveals the final option label: ${JSON.stringify(lastSeen)}`);
                // Focus brings the selected label into view; the remaining
                // description must also be readable by scrolling to the end.
                await list.hover();
                await page.mouse.wheel(0, 2000);
                await page.waitForFunction(() => {
                    const el = document.querySelector(".question-permission-options");
                    return el.scrollTop + el.clientHeight >= el.scrollHeight - 1;
                });
                check(await list.evaluate((el) => {
                    const copy = el.querySelector(".question-radio:last-child .question-option-copy").getBoundingClientRect();
                    const box = el.getBoundingClientRect();
                    return copy.top >= box.top - 1 && copy.bottom <= box.bottom + 1;
                }), "the last description is readable at the end of the list");
                if (process.env.QUESTION_SCREENSHOTS) {
                    await page.screenshot({ path: path.join(process.env.QUESTION_SCREENSHOTS, `${name}-scrolled.png`), animations: "disabled" });
                }
                await page.keyboard.press("ArrowUp");
                assert.equal(await radios.nth(1).isChecked(), true);
                assert.deepEqual(answers, [], "selection alone never approves");
                await page.keyboard.press("Tab");
                const submit = panel.getByRole("button", { name: "Submit response", exact: true });
                assert.equal(await submit.evaluate((el) => document.activeElement === el), true, `${name}: Tab reaches submit`);
                check(await submit.evaluate((el) => {
                    const box = el.getBoundingClientRect(), frame = el.closest(".question-panel").getBoundingClientRect();
                    return box.top >= Math.max(0, frame.top) - 1 && box.bottom <= Math.min(innerHeight, frame.bottom) + 1;
                }), "focused submit is visible within the panel and viewport");
                await page.keyboard.press("Enter");
                await list.waitFor({ state: "hidden" });
                assert.equal(answers.length, 1);
                assert.equal(answers[0].choice, "session");
                assert.equal(answers[0].decision, "accept");
                console.log(`${failures.some((failure) => failure.startsWith(name + ":")) ? "FAIL" : "PASS"} ${name}: layout, scrolling, keyboard navigation and isolated submission`);
            } finally {
                await context.close();
            }
        }
    }
    console.log(JSON.stringify({ samples, failures, errors }, null, 2));
    assert.deepEqual(errors, [], "no browser errors, external requests or unhandled APIs");
    assert.deepEqual(failures, [], "permission option layout and reachability");
    console.log("PASS long permission options contain their copy at all tested sizes");
} finally {
    await browser?.close();
    await server?.close();
    await rm(scratch, { recursive: true, force: true });
}
