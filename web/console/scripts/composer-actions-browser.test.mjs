// Real Composer and production styles. Pointer clicks keep their original
// position while submit clears the draft and the turn becomes busy.
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
const scratch = await realpath(await mkdtemp(path.join(tmpdir(), "steve-composer-actions-")));
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
    const [state, setState] = useState({
        busy: false, value: "Only once", hasMaterials: false,
        pending: false, stopping: false, disabled: false, queueing: true
    });
    window.updateComposer = patch => setState(previous => ({ ...previous, ...patch }));
    window.actions ??= [];
    const submit = () => {
        window.actions.push("submit");
        setState(previous => ({ ...previous, busy: true, value: "", hasMaterials: false }));
    };
    const stop = () => {
        window.actions.push("stop");
        setState(previous => ({ ...previous, stopping: true }));
    };
    return <div className="workbench-shell"><main className="app-main">
        <div className="console-workbench"><div className="console-content"><div className="conversation-content">
            <div className="transcript-scroll min-h-0 flex-1 overflow-y-auto overflow-x-hidden">Conversation</div>
            <div className="composer-dock"><Composer {...state}
                boxRef={box} onChange={value => setState(previous => ({ ...previous, value }))}
                onKey={() => {}} onSubmit={submit} onStop={stop}
                project={{ id: "a-project-with-a-long-name", node: "fixture", path: "/workspace/project", bound: true }}
                projects={[]}
                agent={{ id: "remote-worker-with-a-long-name", node: "fixture", harness: "test", model: "A long model preference label" }}
                agents={[]}
                onSelectors={async () => ({})}
                onToggleQueueing={() => setState(previous => ({ ...previous, queueing: !previous.queueing }))}
                suggestions={[]} pick={0} onApply={() => {}} verbs={[]} onVerb={() => {}}
                onProject={() => {}} onAgent={() => {}}
            /></div>
        </div></div></div>
    </main></div>;
}
createRoot(document.getElementById("root")).render(<LocaleProvider><Fixture /></LocaleProvider>);
`;

async function runCase(name, run) {
    try { await run(); console.log("PASS " + name); }
    catch (error) { failures.push(name + ": " + error.message); console.error("FAIL " + name + ": " + error.stack); }
}
async function pointerDoubleClick(page, target) {
    const rect = await target.boundingBox();
    assert.ok(rect, "the original action is visible");
    assert.equal(await target.evaluate(el => {
        const r = el.getBoundingClientRect();
        return r.x >= 0 && r.y >= 0 && r.right <= innerWidth && r.bottom <= innerHeight
            && [0.1, 0.5, 0.9].every(f => el.contains(document.elementFromPoint(r.x + r.width * f, r.y + r.height / 2)));
    }), true, "the original action is painted and hittable, not internally clipped");
    // Do not re-resolve a locator after the first click: the second click
    // belongs to the same physical gesture, not an intentional Stop action.
    await page.mouse.click(rect.x + rect.width / 2, rect.y + rect.height / 2, { clickCount: 2, delay: 70 });
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
    for (const width of [1440, 780, 420, 390]) {
        for (const locale of ["zh", "en"]) {
            const context = await browser.newContext({ viewport: { width, height: 900 }, serviceWorkers: "block", reducedMotion: "reduce" });
            const page = await context.newPage();
            page.setDefaultTimeout(5000);
            page.on("pageerror", error => errors.push(String(error)));
            await context.route("**/*", route => {
                const request = route.request();
                if (new URL(request.url()).origin !== origin || request.method() !== "GET"
                    || ["fetch", "xhr", "eventsource"].includes(request.resourceType())) {
                    errors.push(`Unexpected request: ${request.method()} ${request.url()}`);
                    return route.abort();
                }
                return route.continue();
            });
            await page.addInitScript(locale => localStorage.setItem("steve.ui.locale", locale), locale);
            await page.goto(origin);
            const labels = locale === "zh" ? { send: "发送", queue: "排队", stop: "停止" } : { send: "Send", queue: "Queue", stop: "Stop" };
            const send = page.getByRole("button", { name: labels.send, exact: true });
            const stop = page.getByRole("button", { name: labels.stop, exact: true });
            const queue = page.locator(".composer-actions").getByRole("button", { name: labels.queue, exact: true });
            const reset = async patch => {
                await page.evaluate(patch => {
                    window.actions.length = 0;
                    window.updateComposer({ busy: false, value: "Only once", hasMaterials: false,
                        pending: false, stopping: false, disabled: false, queueing: true, ...patch });
                }, patch);
            };
            await send.waitFor();
            for (const queueing of [true, false]) {
                await runCase(`send-pointer-double-click ${width}/${locale}/queue=${queueing}`, async () => {
                    await reset({ queueing });
                    await send.waitFor();
                    await pointerDoubleClick(page, send);
                    assert.deepEqual(await page.evaluate(() => window.actions), ["submit"], "double-click Send must not submit Stop");
                    await stop.waitFor();
                    const slot = page.locator(".composer-actions > span[aria-hidden=true]");
                    assert.equal(await slot.evaluate(el => {
                        const before = document.activeElement;
                        el.focus();
                        return getComputedStyle(el).visibility === "hidden" && el.tabIndex === -1 && document.activeElement === before;
                    }), true, "the preserved slot is invisible and cannot steal keyboard focus");
                });
            }
            await runCase(`attachment-pointer-double-click ${width}/${locale}`, async () => {
                await reset({ value: "", hasMaterials: true });
                await send.waitFor();
                await pointerDoubleClick(page, send);
                assert.deepEqual(await page.evaluate(() => window.actions), ["submit"], "attachment submission must not become cancellation");
                await stop.waitFor();
            });
            await runCase(`queue-pointer-double-click ${width}/${locale}`, async () => {
                await reset({ busy: true, value: "Queue once" });
                await queue.waitFor();
                await pointerDoubleClick(page, queue);
                assert.deepEqual(await page.evaluate(() => window.actions), ["submit"], "clearing a queued draft must not move Stop under the pointer");
            });
            await runCase(`explicit-stop-and-keyboard ${width}/${locale}`, async () => {
                await reset({});
                await send.waitFor();
                await send.press("Enter");
                await stop.waitFor();
                assert.deepEqual(await page.evaluate(() => window.actions), ["submit"]);
                await stop.press("Enter");
                assert.deepEqual(await page.evaluate(() => window.actions), ["submit", "stop"], "an explicit keyboard Stop is immediate");
                await reset({ busy: true, value: "" });
                await stop.waitFor();
                await pointerDoubleClick(page, stop);
                assert.deepEqual(await page.evaluate(() => window.actions), ["stop"], "pending cancellation still deduplicates Stop");
            });
            assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "actions must fit narrow viewports");
            await context.close();
        }
    }
    assert.deepEqual(errors, [], "the component fixture must not contact APIs or raise runtime errors");
    assert.deepEqual(failures, []);
} finally {
    await browser?.close();
    await server?.close();
    await rm(scratch, { recursive: true, force: true });
}
