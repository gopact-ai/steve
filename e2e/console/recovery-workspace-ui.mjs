// Exercise the real recovery notice with isolated state, never a Hub/API.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-recovery-notice-"));
const marker = path.join(scratch, ".owned-fixture");
await writeFile(marker, scratch);
let server, browser;
try {
    await writeFile(path.join(scratch, "index.html"), '<div id="root"></div><script type="module" src="/entry.tsx"></script>');
    await writeFile(path.join(scratch, "entry.tsx"), `
        import { useState } from "react";
        import { createRoot } from "react-dom/client";
        import { RecoveryWorkspaceNotice } from "@/components/steve/recovery-workspace";
        import { LocaleProvider, useI18n } from "@/providers/locale-provider";
        function Fixture() {
            const [recovery, setRecovery] = useState({ id: "episode", project: "p", phase: "draining", node: "fixture", path: "/owned-copy/work", base: "b", head: "c", version: 2 });
            const { setLocale } = useI18n();
            window.fixture = { setRecovery, setLocale };
            return <RecoveryWorkspaceNotice recovery={recovery} />;
        }
        createRoot(document.getElementById("root")).render(<LocaleProvider><Fixture /></LocaleProvider>);
    `);
    server = await createServer({ root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        resolve: { alias: { "@": path.join(web, "src"), "react-dom": path.join(web, "node_modules/react-dom"), react: path.join(web, "node_modules/react") } },
        esbuild: { jsx: "automatic" }, server: { host: "127.0.0.1", port: 0, hmr: false, fs: { allow: [scratch, web] } } });
    await server.listen();
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    const context = await browser.newContext({ serviceWorkers: "block", viewport: { width: 420, height: 680 } });
    const errors = [];
    await context.route("**/*", route => { const url = new URL(route.request().url()); if (url.origin === origin && route.request().method() === "GET") return route.continue(); errors.push(url.origin); return route.abort(); });
    const page = await context.newPage();
    page.on("pageerror", error => errors.push(String(error)));
    await page.addInitScript(() => localStorage.setItem("steve.ui.locale", "en"));
    await page.goto(origin);
    await page.getByRole("status").waitFor();
    const update = async (phase, extra = {}) => { await page.evaluate(({phase, extra}) => window.fixture.setRecovery({ id: "episode", project: "p", phase, node: "fixture", path: "/owned-copy/work", base: "b", head: "c", version: 2, ...extra }), { phase, extra }); };
    await page.getByText(/Draining the shared copy/).waitFor();
    await update("capture"); await page.getByText(/Saving the original directory as it is/).waitFor();
    await update("landing", { error: "exact root conflict remains unresolved" }); await page.getByText(/Merging the copy from its fixed baseline/).waitFor(); await page.getByText("exact root conflict remains unresolved").waitFor();
    await update("released", { cleanup_pending: true }); await page.getByText("Project recovery mergeback completed").waitFor(); await page.getByText(/owned copy remains protected/i).waitFor();
    assert.equal(await page.getByText("The original project directory remains isolated for recovery", { exact: true }).count(), 0);
    await update("released", { cleanup_pending: false }); await page.getByText(/Owned-copy cleanup is confirmed/).waitFor();
    await page.evaluate(() => window.fixture.setLocale("zh")); await page.getByText("项目恢复合回已完成").waitFor();
    await update("draining"); await page.getByText(/正在排空共享副本/).waitFor();
    assert.deepEqual(errors, []);
    console.log("Recovery notice phases, release/cleanup separation and both locales pass");
} finally {
    await browser?.close(); await server?.close();
    if (await readFile(marker, "utf8") !== scratch) throw new Error("fixture ownership changed");
    await rm(scratch, { recursive: true });
}
