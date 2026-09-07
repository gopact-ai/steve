// Isolated real-browser DOM checks. Vite serves source modules only; no build,
// live hub, application route mocks or additional dependency is required.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../web/console/", import.meta.url));
const require = createRequire(new URL("../../web/console/package.json", import.meta.url));
const { createServer } = await import(require.resolve("vite"));
const playwright = await import(process.env.PLAYWRIGHT_MODULE || require.resolve("playwright"));
const { chromium } = playwright.default || playwright;
const vite = await createServer({ root, server: { host: "127.0.0.1", port: 0 }, optimizeDeps: { include: ["react", "react-dom", "react-dom/client", "react-markdown", "remark-gfm", "@untitledui/icons"] }, plugins: [{
    name: "selection-test-entry",
    resolveId: (id) => id === "virtual:selection-test" ? "\0selection-test" : null,
    load: (id) => id === "\0selection-test" ? 'export {default as React} from "react"; export {createRoot} from "react-dom/client"; export {flushSync} from "react-dom"; export {Md} from "/src/components/steve/markdown.tsx"; export * as pick from "/src/lib/selection.ts";' : null,
}] });
await vite.listen();
const url = vite.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const page = await browser.newPage();
const errors = [];
page.on("pageerror", (error) => errors.push(String(error)));
await page.route("**/selection-fixture", (route) => route.fulfill({ contentType: "text/html", body: '<!doctype html><html><body><div id="fixture"></div></body></html>' }));
try {
    await page.goto(new URL("selection-fixture", url).href);
    await page.evaluate(async () => {
        const { default: RefreshRuntime } = await import("/@react-refresh");
        RefreshRuntime.injectIntoGlobalHook(window);
        window.$RefreshReg$ = () => {};
        window.$RefreshSig$ = () => (type) => type;
        window.__vite_plugin_react_preamble_installed__ = true;
        const { React, createRoot, flushSync, Md, pick } = await import("/@id/__x00__selection-test");
        window.pick = pick;
        const host = document.getElementById("fixture");
        const renderer = createRoot(host);
        window.markdown = (text) => { flushSync(() => renderer.render(React.createElement(Md, { text }))); return host.querySelector(".md"); };
        window.select = (start, startOffset, end = start, endOffset = start.textContent.length) => {
            const boundary = (element, offset) => {
                if (element.nodeType === 3) return [element, offset];
                const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
                for (let node = walker.nextNode(); node; node = walker.nextNode()) { if (offset <= node.length) return [node, offset]; offset -= node.length; }
                return [element, element.childNodes.length];
            };
            const range = document.createRange(); range.setStart(...boundary(start, startOffset)); range.setEnd(...boundary(end, endOffset)); return range;
        };
    });

    const cases = [
        { raw: "Before **bold** [link](https://example.com) and `inline code` after.", first: "strong", start: 0, last: "code", end: 6, want: "bold** [link](https://example.com) and `inline" },
        { raw: "**a &amp; b** and *escaped \\* stars*", first: "strong", start: 2, last: "em", end: 9, want: "&amp; b** and *escaped \\*" },
        { raw: "**left** and ` padded code ` end", first: "strong", start: 0, last: "code", end: 6, want: "left** and ` padded" },
        { raw: "**before**\r\ntext &amp; tail", first: "strong", start: 0, last: "p > span:last-child", end: 8, want: "before**\r\ntext &amp; " },
        { raw: "**😀前** `👋后`", first: "strong", start: 0, last: "code", end: 3, want: "😀前** `👋后" },
        { raw: "```js\r\nconst x = 1;\r\nnext();\r\n```", first: "pre code", start: 6, last: "pre code", end: 18, want: "x = 1;\r\nnext" },
    ];
    for (const item of cases) {
        const result = await page.evaluate((item) => {
            const root = window.markdown(item.raw);
            const range = window.select(root.querySelector(item.first), item.start, root.querySelector(item.last), item.end);
            return { result: window.pick.selectionForReply(range, root, item.raw), dom: root.innerHTML };
        }, item);
        assert.equal(result.result?.selector.quote, item.want, JSON.stringify(result));
    }
    console.log("PASS Markdown source mapping across emphasis, links, inline code, entities, escapes, CRLF, Unicode and fenced code");

    assert.deepEqual(await page.evaluate(() => {
        const raw = "a &NotEqualTilde; b", root = window.markdown(raw);
        const text = root.querySelector("p");
        return [window.pick.selectionForReply(window.select(text, 2, text, 3), root, raw), window.pick.selectionForReply(window.select(text, 2, text, 4), root, raw)?.selector.quote];
    }), [null, "&NotEqualTilde;"], "Part of a decoded entity has no exact raw boundary");

    const codeCopy = await page.evaluate(() => {
        const raw = "```js\nconst a = '<&>';\nnext();\n```", root = window.markdown(raw);
        const code = root.querySelector("pre code");
        return { code: code.textContent, picked: window.pick.selectionForReply(window.select(root, 0), root, raw), offset: Number(code.dataset.mdStart) };
    });
    assert.equal(codeCopy.code, "const a = '<&>';\nnext();");
    assert.equal(codeCopy.picked, null, "Code toolbar text is never quoted as source");
    assert.equal(codeCopy.offset, 6);
    const copied = await page.evaluate(async () => {
        let copied = "";
        Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value) => { copied = value; } } });
        const root = window.markdown("```js\nconst a = '<&>';\nnext();\n```");
        root.querySelector('button[aria-label="复制"]').click();
        await Promise.resolve();
        return copied;
    });
    assert.equal(copied, "const a = '<&>';\nnext();", "Source position spans must not alter the copy payload");

    const source = await page.evaluate(() => {
        const raw = "😀first\r\n\r\n第三行 tail\r\nlast";
        const root = document.createElement("div"); document.body.append(root);
        raw.split("\r\n").forEach((text, index) => { const row = document.createElement("div"); row.dataset.selectionLine = String(index + 1); const number = document.createElement("button"); number.textContent = String(index + 1); const code = document.createElement("span"); code.dataset.selectionCode = ""; code.textContent = text || "\n"; row.append(number, code); root.append(row); });
        const code = root.querySelectorAll("[data-selection-code]");
        const picked = window.pick.selectionForSource(window.select(code[0], 2, code[2], 3), root, raw);
        code[1].textContent = "changed";
        const stale = window.pick.selectionForSource(window.select(code[0], 2, code[2], 3), root, raw);
        root.remove(); return { picked, stale };
    });
    assert.equal(source.picked.selector.quote, "first\r\n\r\n第三行");
    assert.equal(source.stale, null, "Stale code in the middle of a selection must reject mapping");

    const diffs = await page.evaluate(() => {
        const pick = (rows, firstOffset = 0, lastOffset) => {
            const root = document.createElement("div"); document.body.append(root);
            rows.forEach(([before, after, text]) => { const code = document.createElement("code"); if (before) code.dataset.selectionBefore = String(before); if (after) code.dataset.selectionAfter = String(after); code.textContent = text; root.append(code); });
            const nodes = root.querySelectorAll("code");
            const result = window.pick.selectionForDiff(window.select(nodes[0], firstOffset, nodes[nodes.length - 1], lastOffset ?? nodes[nodes.length - 1].textContent.length), root);
            root.remove(); return result;
        };
        return {
            after: pick([[10, 10, "same"], [null, 11, "added"]]),
            before: pick([[10, 10, "same"], [11, null, "removed"]]),
            mixed: pick([[10, null, "removed"], [null, 10, "added"]]),
            gap: pick([[null, 10, "first hunk"], [null, 20, "next hunk"]]),
            partial: pick([[null, 10, "partial"]], 1),
            partialRows: pick([[null, 10, "first line"], [null, 11, "last line"]], 3, 4),
            beforeNextRow: pick([[null, 10, "first"], [null, 11, "next"]], 0, 0),
            splitMixed: pick([[10, null, "old"], [null, 10, "new"], [11, null, "left next"]]),
        };
    });
    assert.deepEqual(diffs.after.selector, { kind: "lines", start: 10, end: 11 });
    assert.equal(diffs.after.side, "after");
    assert.equal(diffs.before.side, "before");
    assert.equal(diffs.mixed, null);
    assert.equal(diffs.gap, null);
    assert.deepEqual(diffs.partial, { side: "after", selector: { kind: "lines", start: 10, end: 10 }, excerpt: "partial" });
    assert.deepEqual(diffs.partialRows, { side: "after", selector: { kind: "lines", start: 10, end: 11 }, excerpt: "first line\nlast line" });
    assert.deepEqual(diffs.beforeNextRow.selector, { kind: "lines", start: 10, end: 10 });
    assert.equal(diffs.splitMixed, null);
    assert.deepEqual(await page.evaluate(() => {
        const root = window.markdown("**first** and *second*");
        root.querySelector("strong span").dataset.mdStart = "999999";
        const invalid = window.pick.selectionForReply(window.select(root.querySelector("strong"), 0, root.querySelector("em"), 6), root, "**first** and *second*");
        const outside = document.createTextNode("outside"); document.body.append(outside);
        const crossRoot = window.pick.selectionForReply(window.select(root.querySelector("strong"), 0, outside, 7), root, "**first** and *second*");
        outside.remove();
        const huge = "x".repeat(65537), hugeRoot = window.markdown(huge);
        const tooLong = window.pick.selectionForReply(window.select(hugeRoot.querySelector("p"), 0), hugeRoot, huge);
        return [invalid, crossRoot, tooLong];
    }), [null, null, null], "Unknown positions, external ranges and oversized selections fail closed");
    console.log("PASS Source exact partial text, CRLF and stale DOM; Diff same-side full lines, gaps and mixed-side rejection");
    assert.deepEqual(errors, [], "Browser errors");
} finally {
    await browser.close();
    await vite.close();
}
