import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

// Real React components and Blob URLs; every content request is test-local.
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const fixtureID = "virtual:material-thumbnail-fixture";
const fixture = `
import React from "react";
import {createRoot} from "react-dom/client";
import {LocaleProvider} from "/src/providers/locale-provider.tsx";
import {MaterialAttachments} from "/src/components/steve/material-attachments.tsx";
localStorage.setItem("steve.ui.locale","en");
const root=createRoot(document.getElementById("root"));
window.showImages=(keys,id="shared",strict=false)=>{
 const item={ref:{id},material:{id,project:"p",title:id+".png",kind:"image",mime:"image/png",size:68,width:1,height:1,digest:"d".repeat(64),source:{kind:"upload"},created_at:"2026-09-01T00:00:00Z"}};
 const children=keys.map(key=>React.createElement(MaterialAttachments,{key,items:[item]}));
 const body=React.createElement(LocaleProvider,null,children);
 root.render(strict?React.createElement(React.StrictMode,null,body):body);
};
window.fixtureReady=true;
`;
const server = await createServer({
    configFile: path.join(web, "vite.config.ts"), root: web,
    server: { host: "127.0.0.1", port: 0 },
    plugins: [{
        name: "material-thumbnail-test",
        resolveId(id) { if (id === fixtureID) return "\0" + id; },
        load(id) { if (id === "\0" + fixtureID) return fixture; },
        configureServer(vite) {
            vite.middlewares.use(async (req, res, next) => {
                if (req.url !== "/__thumbnail-fixture") return next();
                res.setHeader("Content-Type", "text/html");
                res.end(await vite.transformIndexHtml(req.url,
                    `<div id="root"></div><script type="module" src="/@id/__x00__${fixtureID}"></script>`));
            });
        },
    }],
});
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(7000);
const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+gxkAAAAASUVORK5CYII=", "base64");
const reads = [];
let failContent = true;
const errors = [];
page.on("pageerror", error => errors.push(String(error)));
await page.addInitScript(bytes => {
    window.thumbnailURLs = { created: [], revoked: [] };
    const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
    URL.createObjectURL = blob => { const url = create(blob); window.thumbnailURLs.created.push(url); return url; };
    URL.revokeObjectURL = url => { window.thumbnailURLs.revoked.push(url); return revoke(url); };
    window.heldThumbnails = [];
    const fetch = window.fetch.bind(window);
    window.holdThumbnails = false;
    window.fetch = (input, options) => {
        if (window.holdThumbnails && String(input).includes("/content?")) {
            return new Promise((resolve, reject) => window.heldThumbnails.push({
                signal: options?.signal,
                finish: failed => failed ? reject(new Error("controlled content failure"))
                    : resolve(new Response(new Blob([new Uint8Array(bytes)], { type: "image/png" }))),
            }));
        }
        return fetch(input, options);
    };
}, [...png]);
await page.route("**/*", route => {
    const request = route.request(), parsed = new URL(request.url());
    assert.equal(parsed.origin, new URL(url).origin);
    if (!parsed.pathname.startsWith("/console/")) return route.continue();
    assert.equal(request.method(), "GET", "thumbnail rendering cannot submit mutations");
    if (parsed.pathname.endsWith("/content")) {
        reads.push(parsed.pathname);
        if (failContent && parsed.pathname.includes("failed")) return route.fulfill({ status: 503, body: "controlled refusal" });
        return route.fulfill({ contentType: "image/png", body: png });
    }
    throw new Error("Unexpected API " + parsed.pathname);
});
const show = (keys, id, strict) => page.evaluate(({ keys, id, strict }) => window.showImages(keys, id, strict), { keys, id, strict });
const urls = () => page.evaluate(() => window.thumbnailURLs);
const loaded = count => page.waitForFunction(count => {
    const images = [...document.querySelectorAll("img")];
    return images.length === count && images.every(image => image.complete && image.naturalWidth === 1);
}, count);
const released = () => page.waitForFunction(() => window.thumbnailURLs.created.every(url => window.thumbnailURLs.revoked.includes(url)));
try {
    await page.goto(url + "__thumbnail-fixture");
    await page.waitForFunction(() => window.fixtureReady);
    await show(["first", "second"], "shared");
    await loaded(2);
    assert.equal(reads.length, 1, "simultaneous copies share one actual content fetch");
    const firstURL = (await urls()).created[0];
    await show(["second"], "shared");
    await loaded(1);
    assert.equal((await urls()).revoked.includes(firstURL), false, "one live consumer retains its shared URL");
    await show([], "shared");
    await page.waitForFunction(() => document.querySelectorAll("img").length === 0);
    await released();
    assert.equal((await urls()).revoked.filter(url => url === firstURL).length, 1);
    await show(["returned"], "shared");
    await loaded(1);
    assert.equal(reads.length, 2, "retired images do not keep an idle blob cache");
    await show([], "shared");
    await released();
    console.log("PASS shared visible copies fetch once; final unmount revokes exactly once and returning refetches");

    // An old transport can ignore abort. Its completion must not create a URL
    // or delete the replacement entry for the same identity.
    for (const failed of [false, true]) {
        await page.evaluate(() => { window.holdThumbnails = true; window.heldThumbnails = []; });
        await show(["old"], "late");
        await page.waitForFunction(() => window.heldThumbnails.length === 1);
        await show([], "late");
        await page.waitForFunction(() => window.heldThumbnails[0].signal?.aborted === true);
        await show(["new"], "late");
        await page.waitForFunction(() => window.heldThumbnails.length === 2);
        const before = (await urls()).created.length;
        await page.evaluate(failed => window.heldThumbnails[0].finish(failed), failed);
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
        assert.equal((await urls()).created.length, before, "retired completion cannot allocate an orphan URL");
        await page.evaluate(() => window.heldThumbnails[1].finish(false));
        await loaded(1);
        await show(["new", "joined"], "late");
        await loaded(2);
        assert.equal(await page.evaluate(() => window.heldThumbnails.length), 2, "stale completion cannot discard the new shared entry");
        await show([], "late");
        await released();
    }
    await page.evaluate(() => { window.holdThumbnails = false; });
    console.log("PASS retired pending requests abort; late ignored-abort success and failure leave a replacement lease intact");

    await show(["strict"], "strict", true);
    await loaded(1);
    await show([], "strict", true);
    await released();
    console.log("PASS StrictMode acquire/release replay leaves no retained URLs");

    await show(["failed"], "failed");
    await page.getByRole("button", { name: "Open failed.png", exact: true }).getByText("68 B", { exact: true }).waitFor();
    assert.equal(await page.locator("img").count(), 0, "failed read is a file fallback, not a loading image");
    // A failed copy can remain on the page when another reference appears.
    // Recovering the service permits that new consumer to try a new read.
    failContent = false;
    await show(["failed", "retry"], "failed");
    await loaded(1);
    assert.equal(reads.filter(path => path.includes("failed")).length, 2, "new consumer retries even while an old fallback remains mounted");
    await show([], "failed");
    await released();
    assert.deepEqual(errors, []);
    console.log("PASS failed fetch shows a file fallback; new consumer retries after recovery while the failed copy remains mounted");
} finally {
    await context.close();
    await browser.close();
    await server.close();
}
