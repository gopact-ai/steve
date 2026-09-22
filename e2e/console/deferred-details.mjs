// Real React consumers and native disclosures; no Hub, real config or dist.
// Instrument parsing/rendering in this server only, not production components.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const require = createRequire(new URL("../../web/console/package.json", import.meta.url));
const { default: react } = await import(require.resolve("@vitejs/plugin-react"));
const { default: tailwindcss } = await import(require.resolve("@tailwindcss/vite"));
const cache = await mkdtemp(path.join(tmpdir(), "steve-deferred-details-"));
const entry = `
export { default as React } from "react";
export { createRoot } from "react-dom/client";
export { flushSync } from "react-dom";
export { LocaleProvider } from "/src/providers/locale-provider.tsx";
export { SelectionSurface } from "/src/providers/selection-provider.tsx";
export { selectionForReply } from "/src/lib/selection.ts";
export { Md } from "/src/components/steve/markdown.tsx";
export { InlineProcess } from "/src/components/steve/message.tsx";
export { ThinkingFold } from "/src/components/steve/thinking-fold.tsx";
export { ToolCalls } from "/src/components/steve/tool-calls.tsx";
export { Trace } from "/src/components/steve/progress-view.tsx";
export { DelegationCard } from "/src/components/steve/delegation.tsx";
export { SplitPaneProvider, useSplitPane } from "/src/components/steve/split-pane.tsx";
import "/src/styles/globals.css";
`;
const server = await createServer({
    root: web, configFile: false, resolve: { alias: { "@": path.join(web, "src") } },
    cacheDir: cache, envDir: cache, server: { host: "127.0.0.1", port: 0, watch: null },
    optimizeDeps: { include: ["react", "react-dom", "react-dom/client", "react-markdown", "remark-gfm", "@untitledui/icons"] },
    plugins: [react(), tailwindcss(), {
        name: "deferred-details-measurements", enforce: "pre",
        resolveId: (id) => id === "virtual:deferred-test" ? "\0deferred-test" : null,
        load: (id) => id === "\0deferred-test" ? entry : null,
        transform(source, id) {
            const instrument = (anchor, counter) => {
                assert.ok(source.includes(anchor), `Instrumentation must match ${id}`);
                return source.replace(anchor, `${anchor} globalThis.__deferred.${counter};`);
            };
            if (id.endsWith("/components/steve/markdown.tsx")) return instrument("const raw = String(file.value);", "markdown[raw] = (globalThis.__deferred.markdown[raw] || 0) + 1");
            if (id.endsWith("/components/steve/code-block.tsx")) return instrument("const [copied, setCopied] = useState(false);", "code[code] = (globalThis.__deferred.code[code] || 0) + 1");
            if (id.endsWith("/components/steve/trace.tsx")) return instrument("export function ProcessBody({ process, omitFinalText, omitDelegations }: { process: Process; omitFinalText?: boolean; omitDelegations?: boolean }) {", "processBodies = (globalThis.__deferred.processBodies || 0) + 1");
        },
    }],
});
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
try {
    await server.listen();
    const origin = server.resolvedUrls.local[0].replace(/\/$/, "");
    const context = await browser.newContext({ viewport: { width: 1200, height: 900 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.route("**/*", (route) => {
        const req = route.request(), url = new URL(req.url());
        if (url.origin !== origin || req.method() !== "GET" || /^\/(console|state|events|history)(\/|$)/.test(url.pathname)) {
            errors.push(`Unexpected request: ${req.method()} ${url.pathname}`);
            return route.abort();
        }
        if (url.pathname === "/deferred-fixture") return route.fulfill({ contentType: "text/html", body: '<!doctype html><html><body><main id="fixture"></main></body></html>' });
        return route.continue();
    });
    await page.addInitScript(() => {
        localStorage.setItem("steve.ui.locale", "en");
        window.__deferred = { markdown: {}, code: {} };
    });
    await page.goto(`${origin}/deferred-fixture`);
    await page.evaluate(async () => {
        const { default: RefreshRuntime } = await import("/@react-refresh");
        RefreshRuntime.injectIntoGlobalHook(window);
        window.$RefreshReg$ = () => {};
        window.$RefreshSig$ = () => (type) => type;
        window.__vite_plugin_react_preamble_installed__ = true;
        const api = await import("/@id/__x00__deferred-test");
        const h = api.React.createElement, root = api.createRoot(document.getElementById("fixture"));
        window.api = api;
        window.render = (children) => api.flushSync(() => root.render(h(api.LocaleProvider, null, h(api.SplitPaneProvider, null, children))));
        window.tool = (id, more = {}) => ({ id, name: "Read source", kind: "read", status: "completed", input: `input ${id}`, output: `output ${id}`, ...more });
        const section = (id, component, props) => h("section", { key: id, id }, h(component, props));
        window.consumerProps = {
            process: { process: { timeline: [{ kind: "thought", text: "hidden process **trace**" }, { kind: "text", text: "omitted final answer" }] } },
            thinking: { text: "hidden thinking **trace**" },
            calls: { tools: [window.tool("closed-group")], defaultOpen: false },
            activity: { p: { timeline: [{ kind: "tool", tool: "activity" }], tools: [window.tool("activity")] } },
            row: { tools: [window.tool("shell", { input: '{"cmd":"echo retained"}', output: '{"output":"tool output","exit_code":0}' })] },
            folded: { tools: [window.tool("poll-1"), window.tool("poll-2")] },
            delegation: { id: "#done", info: { state: "done", goal: "Completed goal", answer: "hidden delegated answer" }, progress: { timeline: [{ kind: "thought", text: "hidden delegated **trace**" }] } },
            live: { text: "visible live **thinking**", open: true, live: true },
            running: { id: "#running", info: { state: "running" }, progress: { reasoning: "visible running thinking" } },
            failed: { id: "#failed", info: { state: "failed", answer: "visible failed answer" } },
        };
        const components = { process: api.InlineProcess, thinking: api.ThinkingFold, calls: api.ToolCalls, activity: api.Trace, row: api.ToolCalls, folded: api.ToolCalls, delegation: api.DelegationCard, live: api.ThinkingFold, running: api.DelegationCard, failed: api.DelegationCard };
        function SplitProbe() {
            const split = api.useSplitPane();
            return h("output", { id: "split-state", hidden: true }, JSON.stringify({ tabs: split.tabs, active: split.active }));
        }
        window.renderConsumers = () => window.render([...Object.entries(components).map(([id, component]) => section(id, component, window.consumerProps[id])), h(SplitProbe, { key: "split" })]);
        window.renderConsumers();
    });
    const first = await page.evaluate(() => ({
        hiddenParses: Object.keys(window.__deferred.markdown).filter((text) => text.startsWith("hidden ")),
        codeRenders: Object.keys(window.__deferred.code),
        processBodies: window.__deferred.processBodies || 0,
        hiddenBodies: ["process", "thinking", "calls", "activity", "delegation"].map((id) => [id, document.querySelector(`#${id} details`).children.length - 1]),
        visibleParses: Object.keys(window.__deferred.markdown).filter((text) => text.startsWith("visible ")),
    }));
    console.log("Initial real-consumer instrumentation:", JSON.stringify(first));
    assert.deepEqual(first.hiddenParses, [], "Never-opened consumers must not invoke the Markdown parser");
    assert.deepEqual(first.codeRenders, [], "Closed tool I/O must not render CodeBlock");
    assert.equal(first.processBodies, 0, "A closed process must not execute its trace body");
    assert.ok(first.hiddenBodies.every(([, count]) => count === 0), "Closed details must contain only their original summary");
    assert.equal(first.visibleParses.length, 3, "Default-open/live consumers parse in the initial synchronous commit");
    assert.equal(await page.locator("#folded details[class~='group/row']").count(), 0, "Repeated tools must not mount rows behind their closed group");
    console.log("PASS all closed consumers skip parsing/mounting; live/running/failed bodies render immediately");

    async function toggle(details, key = "Enter") {
        await details.evaluate((el) => {
            window.nextToggle = new Promise((resolve) => el.addEventListener("toggle", () => resolve(), { once: true }));
        });
        await details.locator(":scope > summary").focus();
        await page.keyboard.press(key);
        await page.evaluate(() => window.nextToggle);
    }
    const processFold = page.locator("#process details").first();
    await toggle(processFold);
    await page.locator("#process .md").waitFor();
    assert.equal(await page.getByText("omitted final answer", { exact: true }).count(), 0);
    const thinking = page.locator("#thinking details");
    await toggle(thinking, "Space");
    await page.locator("#thinking .md").waitFor();
    for (const selector of ["#calls details", "#activity details[data-span-kind='tool']", "#folded details[class~='group/fold']"]) {
        const details = page.locator(selector).first();
        await toggle(details);
        await details.locator("details[class~='group/row']").first().waitFor();
        assert.equal(await details.locator("pre").count(), 0, "Opening a group does not open its tool I/O");
    }
    const row = page.locator("#row details[class~='group/row']");
    assert.match(await row.locator("summary").innerText(), /echo retained/);
    assert.match(await row.locator("summary").innerText(), /exit 0/);
    await toggle(row);
    await row.locator("pre").last().waitFor();
    assert.deepEqual(await row.locator("pre").allTextContents(), ["echo retained", "tool output"]);
    const bodyIdentity = await row.locator("pre").first().elementHandle();
    const parsesBeforeFold = await page.evaluate(() => structuredClone(window.__deferred));
    await toggle(row, "Space");
    await toggle(row);
    assert.equal(await bodyIdentity.evaluate((el) => el === document.querySelector("#row pre")), true);
    assert.deepEqual(await page.evaluate(() => window.__deferred), parsesBeforeFold, "Close/reopen must not reparse or rerender retained bodies");
    console.log("PASS native Enter/Space open real trace, thinking, activities, repeated tools and I/O one layer at a time");

    const delegation = page.locator("#delegation details").first();
    await delegation.locator("summary button").click();
    assert.equal(await delegation.evaluate((el) => el.open), false, "Split button must not toggle the disclosure");
    assert.equal(await delegation.locator(".md").count(), 0, "Opening a split must not mount the closed card body");
    assert.deepEqual(await page.locator("#split-state").evaluate((el) => JSON.parse(el.textContent)), { tabs: [{ id: "delegation:#done", kind: "delegation", task: "done" }], active: "delegation:#done" });
    await toggle(delegation);
    await delegation.getByText("hidden delegated answer", { exact: true }).waitFor();
    const delegatedBody = await delegation.locator(":scope > div").elementHandle();
    await page.evaluate(() => { window.consumerProps.delegation.info = { ...window.consumerProps.delegation.info, answer: "updated delegated answer" }; window.renderConsumers(); });
    assert.equal(await delegation.evaluate((el) => el.open), true, "An unchanged open prop must not override the user's open state");
    await delegation.getByText("updated delegated answer", { exact: true }).waitFor();
    await toggle(delegation);
    await toggle(delegation);
    assert.equal(await delegatedBody.evaluate((el) => el === document.querySelector("#delegation details > div")), true);
    await toggle(page.locator("#running details").first());
    await page.evaluate(() => { window.consumerProps.running.progress = { reasoning: "updated running thinking" }; window.renderConsumers(); });
    assert.equal(await page.locator("#running details").first().evaluate((el) => el.open), false, "Streaming updates must not reopen a manually closed running card");
    await page.evaluate(() => { window.consumerProps.delegation.info = { ...window.consumerProps.delegation.info, state: "failed" }; window.renderConsumers(); });
    assert.equal(await delegation.evaluate((el) => el.open), true, "A changed failed-state open prop takes effect");
    console.log("PASS completed delegation defers, running/failed defaults and manual state survive updates, split action stays independent");

    // Exercise the primitive only after checking existing consumers, so the Red
    // failure on unmodified code proves eager real parsing, not a missing module.
    await page.evaluate(async () => {
        const { DeferredDetails } = await import("/src/components/steve/deferred-details.tsx");
        const { React, Md, SelectionSurface } = window.api, h = React.createElement;
        window.probe = { mounts: 0, unmounts: 0, toggles: [] };
        window.raw = "Before **retained selection** and `inline code`.";
        function Body({ text }) {
            const [count, setCount] = React.useState(0);
            React.useEffect(() => { window.probe.mounts++; return () => window.probe.unmounts++; }, []);
            return h("div", { id: "probe-body" },
                h("button", { id: "counter", onClick: () => setCount((value) => value + 1) }, String(count)),
                h(SelectionSurface, { version: "stable", resolve: () => null }, h(Md, { text })),
                h(DeferredDetails, { id: "nested", summary: h("summary", null, "Nested") }, h("input", { defaultValue: "nested state" })),
            );
        }
        window.primitive = { open: false, text: window.raw, key: "first" };
        window.renderPrimitive = () => window.render(h(DeferredDetails, {
            id: "primitive", key: window.primitive.key, open: window.primitive.open, className: "native-details",
            onToggle: (event) => window.probe.toggles.push(event.currentTarget.open),
            summary: h("summary", { title: "Original summary", className: "original-summary" }, "Details"),
        }, h(Body, { text: window.primitive.text })));
        window.renderPrimitive();
    });
    const primitive = page.locator("#primitive");
    assert.equal(await primitive.locator("#probe-body").count(), 0);
    assert.equal(await primitive.locator("summary").getAttribute("title"), "Original summary");
    await page.evaluate(() => { window.primitive.text = "Updated before first **open**"; window.renderPrimitive(); });
    assert.equal(await page.evaluate(() => window.__deferred.markdown["Updated before first **open**"] || 0), 0);
    await toggle(primitive);
    await primitive.locator("#probe-body").waitFor();
    await primitive.getByText("open", { exact: true }).waitFor();
    await primitive.locator("#counter").click();
    await page.evaluate(() => { window.primitive.text = window.raw; window.renderPrimitive(); });
    await page.evaluate(() => {
        const surface = document.querySelector("#primitive [data-selection-surface]");
        const text = surface.querySelector("strong span").firstChild;
        const range = document.createRange();
        range.selectNodeContents(text);
        window.retained = { surface, text, range, body: document.getElementById("probe-body") };
    });
    const identity = await page.evaluate(() => ({ mounts: window.probe.mounts, unmounts: window.probe.unmounts }));
    await toggle(primitive, "Space");
    await toggle(primitive);
    assert.equal(await primitive.locator("#counter").innerText(), "1");
    assert.deepEqual(await page.evaluate(() => ({ mounts: window.probe.mounts, unmounts: window.probe.unmounts })), identity);
    assert.equal(await page.evaluate(() => {
        const { surface, text, range, body } = window.retained;
        return body === document.getElementById("probe-body") && text.isConnected &&
            surface === document.querySelector("[data-selection-surface]") &&
            window.api.selectionForReply(range, surface, window.raw)?.selector.quote === "retained selection";
    }), true, "Retained DOM ranges and real Markdown source mapping survive close/reopen");
    const outerToggles = await page.evaluate(() => window.probe.toggles.length);
    await toggle(page.locator("#nested"));
    await page.locator("#nested input").fill("retained input");
    await page.evaluate(() => document.getElementById("nested").dispatchEvent(new Event("toggle", { bubbles: true })));
    assert.equal(await page.evaluate(() => window.probe.toggles.length), outerToggles, "Nested toggle must not call the parent callback");
    await toggle(primitive);
    await page.evaluate(() => { document.getElementById("nested").open = false; });
    await page.waitForFunction(() => !document.getElementById("nested").open);
    assert.equal(await primitive.evaluate((el) => el.open), false);
    await page.evaluate(() => { window.primitive.text = "Updated while **closed**"; window.renderPrimitive(); });
    assert.equal(await primitive.locator(".md strong").textContent(), "closed", "Mounted children still receive updates when folded");
    await toggle(primitive);
    await toggle(page.locator("#nested"));
    assert.equal(await page.locator("#nested input").inputValue(), "retained input");
    console.log("PASS first-open updates, retained React/input state, DOM/source selection identity and nested toggle isolation");

    const propChanges = await page.evaluate(() => {
        const body = document.getElementById("probe-body");
        window.primitive.open = true; window.renderPrimitive();
        const opened = document.getElementById("primitive").open;
        window.primitive.open = false; window.renderPrimitive();
        const closed = !document.getElementById("primitive").open;
        const retained = body === document.getElementById("probe-body");
        window.primitive = { key: "pulse", open: false, text: "Pulse body" }; window.renderPrimitive();
        window.primitive.open = true; window.renderPrimitive();
        const pulseBody = document.getElementById("probe-body");
        window.primitive.open = false; window.renderPrimitive();
        const pulseRetained = !!pulseBody && pulseBody === document.getElementById("probe-body");
        window.primitive = { key: "default-open", open: true, text: "Default open body" }; window.renderPrimitive();
        const immediate = !!document.querySelector("#primitive[open] #probe-body");
        window.primitive.open = false; window.renderPrimitive();
        return { opened, closed, retained, pulseRetained, immediate, defaultRetained: !!document.getElementById("probe-body") };
    });
    assert.deepEqual(propChanges, { opened: true, closed: true, retained: true, pulseRetained: true, immediate: true, defaultRetained: true });
    await toggle(primitive);
    await page.evaluate(() => { window.primitive.text = "No open prop"; window.primitive.open = undefined; window.renderPrimitive(); });
    assert.equal(await primitive.evaluate((el) => el.open), false, "Removing the open prop preserves native React attribute semantics");
    console.log("PASS open prop changes, same-task open/close before toggle dispatch, default-open synchronous mounting");

    await page.evaluate(() => {
        window.consumerProps.live = { text: Array.from({ length: 40 }, (_, i) => `Live line ${i}`).join("\n\n"), open: false, live: true };
        window.renderConsumers();
    });
    const live = page.locator("#live details"), scroll = live.locator(":scope > div");
    const layoutSettled = () => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await scroll.count(), 0);
    await toggle(live);
    await page.waitForFunction(() => {
        const el = document.querySelector("#live details > div");
        return el && el.scrollHeight > el.clientHeight && Math.abs(el.scrollHeight - el.clientHeight - el.scrollTop) < 2;
    });
    await toggle(live);
    await layoutSettled();
    await page.evaluate(() => {
        window.consumerProps.live.text += "\n\n" + Array.from({ length: 20 }, (_, i) => `Hidden live line ${i}`).join("\n\n");
        window.renderConsumers();
    });
    await layoutSettled();
    await toggle(live);
    await layoutSettled();
    await page.waitForFunction(() => {
        const el = document.querySelector("#live details > div");
        return Math.abs(el.scrollHeight - el.clientHeight - el.scrollTop) < 2;
    });
    const followed = await scroll.evaluate((el) => ({ top: el.scrollTop, end: el.scrollHeight - el.clientHeight }));
    await scroll.focus();
    await scroll.evaluate((el) => { el.dispatchEvent(new WheelEvent("wheel", { deltaY: -100, bubbles: true })); el.scrollTop = 80; });
    await page.evaluate(() => { window.consumerProps.live.text += "\n\nNew live chunk"; window.renderConsumers(); });
    assert.equal(await scroll.evaluate((el) => el.scrollTop), 80, "Streaming must preserve manual scroll intent after deferred mount");
    await toggle(live);
    await layoutSettled();
    await page.evaluate(() => { window.consumerProps.live.text += "\n\nAnother hidden live chunk"; window.renderConsumers(); });
    await layoutSettled();
    await toggle(live);
    await layoutSettled();
    assert.equal(await scroll.evaluate((el) => el.scrollTop), 80);
    console.log("Live reopen scroll measurements:", JSON.stringify({ followed, manualTop: await scroll.evaluate((el) => el.scrollTop) }));
    for (const width of [1200, 390]) {
        await page.setViewportSize({ width, height: 900 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "Disclosures must fit narrow windows");
    }
    if (process.env.SCREENSHOT_DIR) {
        await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, "deferred-details-mobile.png"), fullPage: true });
        await page.setViewportSize({ width: 1200, height: 900 });
        await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, "deferred-details-desktop.png"), fullPage: true });
    }
    console.log("PASS deferred live follow-tail initialization, manual-scroll retention and narrow viewport");
    assert.deepEqual(errors, [], "No browser errors or unexpected network requests");
    await context.close();
} finally {
    await browser.close();
    await server.close();
    await rm(cache, { recursive: true, force: true });
}
