// Real Composer and production styles; no app entry, live API or shared Vite cache.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import react from "../node_modules/@vitejs/plugin-react/dist/index.js";
import tailwindcss from "../node_modules/@tailwindcss/vite/dist/index.mjs";
import { chromium } from "../node_modules/playwright/index.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const scratch = await realpath(await mkdtemp(path.join(tmpdir(), "steve-composer-agent-")));
const checks = process.env.CHECK?.split(",") ?? ["empty", "populated", "guards"];
const errors = [], failures = [];
let server, browser;
const fixture = `
import React, { useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { LocaleProvider } from "@/providers/locale-provider";
import { Composer } from "@/components/steve/composer";
import "./fixture.css";
function Fixture() {
    const box = useRef(null);
    const [state, setState] = useState({ agents: [], disabled: false, pending: false });
    window.updateComposer = patch => setState(previous => ({ ...previous, ...patch }));
    window.chosen ??= [];
    return <div className="workbench-shell"><main className="app-main">
        <div className="console-workbench"><div className="console-content"><div className="conversation-content">
            <div className="transcript-scroll min-h-0 flex-1 overflow-y-auto overflow-x-hidden">New conversation</div>
            <div className="composer-dock"><Composer {...state}
                boxRef={box} value="" onChange={() => {}} onKey={() => {}} onSubmit={() => {}} onStop={() => {}}
                busy={false} suggestions={[]} pick={0} onApply={() => {}} verbs={[]} onVerb={() => {}}
                projects={[]} onProject={() => {}} onAgent={id => window.chosen.push(id)}
            /></div>
        </div></div></div>
    </main></div>;
}
createRoot(document.getElementById("root")).render(<LocaleProvider><Fixture /></LocaleProvider>);
`;
const agents = [
    { id: "ready-agent", usable: true, node: "fixture", harness: "test" },
    { id: "offline-agent", usable: false, node: "fixture", harness: "test", because: "Machine is offline" },
    { id: "long-agent-" + "name".repeat(40), usable: true, node: "fixture", harness: "test" },
];
const settle = page => page.evaluate(() => Promise.all(document.getAnimations()
    .filter(animation => animation.effect?.getComputedTiming().iterations !== Infinity)
    .map(animation => animation.finished.catch(() => {}))));
const record = async (name, run) => {
    try { await run(); console.log(`PASS ${name}`); }
    catch (error) { failures.push(`${name}: ${error.stack}`); console.error(`FAIL ${name}: ${error.message}`); }
};
async function geometry(menu) {
    return menu.evaluate(el => {
        const rect = el.getBoundingClientRect();
        const style = getComputedStyle(el);
        const ancestors = [];
        for (let parent = el.parentElement; parent; parent = parent.parentElement) {
            const s = getComputedStyle(parent);
            ancestors.push({ tag: parent.tagName, class: parent.className, overflow: s.overflow, contain: s.contain });
        }
        return { x: rect.x, y: rect.y, width: rect.width, height: rect.height, padding: style.padding,
            inComposer: !!el.closest(".composer"), ancestors };
    });
}
async function visibleAndHittable(locator, viewport, fit = true) {
    const rect = await locator.boundingBox();
    assert.ok(rect && rect.width > 0 && rect.height > 0, "visible content has area");
    if (fit) assert.ok(rect.x >= 0 && rect.y >= 0 && rect.x + rect.width <= viewport.width + 1
        && rect.y + rect.height <= viewport.height + 1, `content fits viewport: ${JSON.stringify(rect)}`);
    assert.equal(await locator.evaluate(el => {
        const r = el.getBoundingClientRect();
        return [0.1, 0.5, 0.9].every(f => el.contains(document.elementFromPoint(r.x + r.width * f, r.y + r.height / 2)));
    }), true, "content is actually painted/hit-testable, not clipped or covered");
}
try {
    await symlink(path.join(root, "node_modules"), path.join(scratch, "node_modules"), "dir");
    await writeFile(path.join(scratch, "fixture.tsx"), fixture);
    await writeFile(path.join(scratch, "fixture.css"), `@import "${root}/src/styles/globals.css";\n@source "${root}/src";\n@source "./fixture.tsx";`);
    await writeFile(path.join(scratch, "index.html"), '<html><body><div id="root"></div><script type="module" src="/fixture.tsx"></script></body></html>');
    server = await createServer({ root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"),
        plugins: [react(), tailwindcss()], resolve: { alias: { "@": path.join(root, "src") }, dedupe: ["react", "react-dom"] },
        server: { host: "127.0.0.1", port: 0, fs: { allow: [root, scratch] } }, logLevel: "error" });
    await server.listen();
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }, { width: 844, height: 390 }]) {
        for (const locale of ["zh", "en"]) {
            const context = await browser.newContext({ viewport, serviceWorkers: "block", reducedMotion: "reduce" });
            const page = await context.newPage();
            page.setDefaultTimeout(5000);
            page.on("pageerror", error => errors.push(String(error)));
            await context.route("**/*", route => {
                const req = route.request();
                if (new URL(req.url()).origin !== origin || req.method() !== "GET" || ["fetch", "xhr", "eventsource"].includes(req.resourceType())) {
                    errors.push(`Unexpected request: ${req.method()} ${req.url()}`); return route.abort();
                }
                return route.continue();
            });
            await page.addInitScript(locale => {
                localStorage.setItem("steve.ui.locale", locale);
                if (locale === "zh") document.addEventListener("DOMContentLoaded", () => document.documentElement.classList.add("dark-mode"));
            }, locale);
            await page.goto(origin);
            const trigger = page.getByRole("button", { name: "Agent", exact: true });
            await trigger.waitFor();
            const menu = page.getByRole("menu");
            const open = async () => { await trigger.click(); await menu.waitFor(); await settle(page); };
            const close = async () => {
                await page.keyboard.press("Escape"); await menu.waitFor({ state: "hidden" });
                await page.waitForFunction(() => document.activeElement?.getAttribute("aria-label") === "Agent");
            };
            if (checks.includes("empty")) await record(`empty ${viewport.width}/${locale}`, async () => {
                await open();
                console.log("EMPTY GEOMETRY", JSON.stringify(await geometry(menu)));
                if (process.env.AGENT_SCREENSHOTS) {
                    await mkdir(process.env.AGENT_SCREENSHOTS, { recursive: true });
                    await page.screenshot({ path: path.join(process.env.AGENT_SCREENSHOTS, `empty-${viewport.width}-${locale}.png`) });
                }
                const title = locale === "zh" ? "当前会话没有可选 Agent" : "No agents available for this conversation";
                const hint = locale === "zh" ? "请在「资源」中检查机器连接和 Agent 登记。" : "Check machine connections and Agent registration in Resources.";
                assert.ok((await menu.innerText()).includes(title), "empty Agent menu must explain why there are no choices");
                assert.ok((await menu.innerText()).includes(hint), "empty Agent menu must explain the next step");
                assert.equal(await menu.locator("[data-key]").count(), 0, "empty text is not a fabricated Agent option");
                await visibleAndHittable(menu.getByText(title, { exact: true }), viewport);
                await visibleAndHittable(menu.getByText(hint, { exact: true }), viewport);
                await page.keyboard.press("Enter");
                assert.deepEqual(await page.evaluate(() => window.chosen), [], "empty state cannot select an Agent");
                await close();
                await trigger.press("Enter"); await menu.waitFor(); await close();
            });
            // A failed empty-state assertion must not hide populated-menu evidence.
            if (await menu.count()) { await page.keyboard.press("Escape"); await menu.waitFor({ state: "hidden" }); }
            if (checks.includes("populated")) await record(`populated ${viewport.width}/${locale}`, async () => {
                await page.evaluate(agents => window.updateComposer({ agents }), agents);
                await open();
                console.log("POPULATED GEOMETRY", JSON.stringify(await geometry(menu)));
                assert.equal((await geometry(menu)).inComposer, false, "menu is portaled outside composer containment");
                assert.equal(await menu.getByRole("menuitem").count(), 3);
                // Populated menus retain their sizing; test actual hit targets
                // rather than inferring visibility from a bounding box alone.
                for (const id of [agents[0].id, agents[1].id, agents[2].id]) await visibleAndHittable(menu.getByRole("menuitem", { name: new RegExp(`^${id} `) }), viewport, false);
                const offline = menu.getByRole("menuitem", { name: /^offline-agent / });
                assert.equal(await offline.getAttribute("aria-disabled"), "true");
                assert.ok((await offline.innerText()).includes("Machine is offline"));
                await offline.click({ force: true });
                assert.deepEqual(await page.evaluate(() => window.chosen), []);
                await menu.getByRole("menuitem", { name: /^ready-agent / }).click();
                await menu.waitFor({ state: "hidden" });
                assert.deepEqual(await page.evaluate(() => window.chosen), ["ready-agent"]);
                // Stronger than production: even paint containment on the dock cannot clip the portal.
                await page.locator(".composer-dock").evaluate(el => { el.style.overflow = "hidden"; el.style.contain = "paint"; });
                await trigger.focus(); await trigger.press("ArrowDown"); await menu.waitFor(); await settle(page);
                await visibleAndHittable(menu.getByRole("menuitem", { name: /^ready-agent / }), viewport, false);
                await page.keyboard.press("ArrowDown"); await page.keyboard.press("Enter");
                await menu.waitFor({ state: "hidden" });
                assert.deepEqual(await page.evaluate(() => window.chosen), ["ready-agent", agents[2].id], "keyboard skips unusable Agents");
                await page.locator(".composer-dock").evaluate(el => { el.style.overflow = ""; el.style.contain = ""; });
                await open();
                await page.evaluate(() => window.updateComposer({ agents: [] }));
                await page.waitForFunction(() => document.querySelectorAll('[role="menu"] [data-key]').length === 0);
                if (checks.includes("empty")) assert.ok((await menu.innerText()).includes(locale === "zh" ? "当前会话没有可选 Agent" : "No agents available for this conversation"), "open menu reflects Agents disappearing");
                await page.evaluate(agents => window.updateComposer({ agents }), agents);
                await menu.getByRole("menuitem", { name: /^ready-agent / }).waitFor();
                await close();
            });
            if (await menu.count()) { await page.keyboard.press("Escape"); await menu.waitFor({ state: "hidden" }); }
            if (checks.includes("guards")) await record(`guards ${viewport.width}/${locale}`, async () => {
                for (const key of ["disabled", "pending"]) {
                    await page.evaluate(key => window.updateComposer({ [key]: true }), key);
                    await page.waitForFunction(() => document.querySelector('[aria-label="Agent"]').disabled);
                    await trigger.click({ force: true });
                    assert.equal(await menu.count(), 0, "preparing/pending cannot open Agent menu");
                    await page.evaluate(key => window.updateComposer({ [key]: false }), key);
                    await page.waitForFunction(() => !document.querySelector('[aria-label="Agent"]').disabled);
                }
            });
            await context.close();
        }
    }
    assert.deepEqual(errors, [], "fixture must not contact APIs or raise runtime errors");
    assert.deepEqual(failures, []);
} finally {
    await browser?.close();
    await server?.close();
    await rm(scratch, { recursive: true, force: true });
}
