// Exercise the real sidebar with rolled snapshot values, never a Hub/API.
import assert from "node:assert/strict";
import test from "node:test";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const web = fileURLToPath(new URL("..", import.meta.url));
const task = (id, attention = 0, extra = {}) => ({
    id, goal: `Work ${id}`, transport: "console", channel: "a",
    lifecycle: "running", execution: "idle", attention, ...extra,
});
const cases = [
    { name: "one child approval counts once", tasks: [task("root", 1), task("child", 1, { parent: "root" })], waiting: 1 },
    { name: "one parent and one child approval count twice", tasks: [task("root", 2), task("child", 1, { parent: "root" })], waiting: 2 },
    { name: "deep descendant attention is already rolled through every ancestor", tasks: [
        task("root", 3), task("child", 2, { parent: "root" }), task("grandchild", 1, { parent: "child" }),
    ], waiting: 3 },
    { name: "multiple roots sum without crossing conversations or transports", tasks: [
        task("root", 1), task("child", 1, { parent: "root" }),
        task("second", 2), task("second-child", 2, { parent: "second" }),
        task("other", 7, { channel: "b" }), task("external", 9, { transport: "feishu" }),
    ], waiting: 3 },
];

test("session attention uses rolled roots in the real SessionsTree", async (t) => {
    const scratch = await mkdtemp(path.join(tmpdir(), "steve-sessions-attention-"));
    let server, browser;
    try {
        await writeFile(path.join(scratch, "index.html"), '<style>svg{width:16px;height:16px}</style><div id="root"></div><script type="module" src="/entry.tsx"></script>');
        await writeFile(path.join(scratch, "entry.tsx"), `
            import { useState } from "react";
            import { createRoot } from "react-dom/client";
            import { MemoryRouter } from "react-router";
            import { SessionsTree } from "@/components/steve/sessions-tree";
            import { LocaleProvider, useI18n } from "@/providers/locale-provider";
            const list = [{ id: "a", title: "Approval fixture", count: 0, running: false }];
            function Fixture() {
                const [tasks, setTasks] = useState([]);
                const { setLocale } = useI18n();
                window.fixture = { setTasks, setLocale };
                return <SessionsTree list={list} projects={[]} current="a" tasks={tasks}
                    onPick={() => {}} onNew={() => {}} onUpdate={() => {}} onDelete={async () => {}}
                    onTask={task => { window.taskPicked = task.id; }} />;
            }
            createRoot(document.getElementById("root")).render(
                <LocaleProvider><MemoryRouter><Fixture /></MemoryRouter></LocaleProvider>
            );
        `);
        server = await createServer({
            root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
            resolve: { alias: {
                "@": path.join(web, "src"), "react-router": path.join(web, "node_modules/react-router"),
                "react-dom": path.join(web, "node_modules/react-dom"), react: path.join(web, "node_modules/react"),
            } },
            esbuild: { jsx: "automatic" },
            server: { host: "127.0.0.1", port: 0, hmr: false, fs: { allow: [scratch, web] } },
        });
        await server.listen();
        const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
        browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
        const context = await browser.newContext({ serviceWorkers: "block" });
        const errors = [];
        await context.route("**/*", (route) => {
            const request = route.request(), url = new URL(request.url());
            if (url.origin === origin && request.method() === "GET" && !url.pathname.startsWith("/api/")) return route.continue();
            errors.push(`Unexpected request: ${request.method()} ${url.pathname}`);
            return route.abort();
        });
        const page = await context.newPage();
        page.on("pageerror", (error) => errors.push(String(error)));
        await page.addInitScript(() => {
            localStorage.setItem("steve.ui.locale", "en");
            localStorage.setItem("steve.sessions.arrangement", JSON.stringify({ group: "none", sort: "recent" }));
        });
        await page.goto(origin);
        await page.waitForFunction(() => !!window.fixture);
        const settle = () => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        const setTasks = async (tasks) => { await page.evaluate((tasks) => window.fixture.setTasks(tasks), tasks); await settle(); };
        const waiting = page.getByText(/^\d+ need attention$/);
        const open = async () => {
            const expand = page.getByRole("button", { name: "Expand tasks for Approval fixture", exact: true });
            if (await expand.count()) await expand.press("Enter");
            await settle();
        };
        for (const scenario of cases) {
            await t.test(scenario.name, async () => {
                await setTasks(scenario.tasks);
                await open();
                assert.equal(await waiting.innerText(), `${scenario.waiting} need attention`);
            });
        }
        await t.test("resolved approvals disappear on the next snapshot, including after reopening", async () => {
            await setTasks(cases[0].tasks);
            await open();
            // Deliberately check the transition even when the old count is wrong.
            assert.equal(await waiting.count(), 1);
            await setTasks(cases[0].tasks.map((item) => ({ ...item, attention: 0 })));
            assert.equal(await waiting.count(), 0);
            assert.equal(await page.getByText("Tasks: 1 · Delegations: 1", { exact: true }).count(), 1);
            await page.getByRole("button", { name: "Collapse tasks for Approval fixture", exact: true }).press("Enter");
            await open();
            assert.equal(await waiting.count(), 0);
        });
        await t.test("running, failed, direct-child display and keyboard task selection retain their semantics", async () => {
            await setTasks([
                task("root", 1, { execution: "running" }),
                task("child", 1, { parent: "root", execution: "running" }),
                task("failed", 0, { parent: "root", lifecycle: "failed" }),
                task("grandchild", 1, { parent: "child", execution: "running" }),
            ]);
            await open();
            assert.equal(await page.getByText("2 running", { exact: true }).count(), 1);
            assert.equal(await page.getByText("1 failed", { exact: true }).count(), 1);
            assert.equal(await page.getByRole("button", { name: /#grandchild/ }).count(), 0);
            await page.getByRole("button", { name: /#child.*Work child/ }).press("Enter");
            assert.equal(await page.evaluate(() => window.taskPicked), "child");
        });
        await t.test("narrow view and locale changes use the same rolled count", async () => {
            await setTasks(cases[1].tasks);
            await page.setViewportSize({ width: 390, height: 844 });
            await page.evaluate(() => window.fixture.setLocale("zh"));
            // Chinese messages are fetched on demand before the page switches.
            await page.locator("html[lang='zh-CN']").waitFor({ state: "attached" });
            await settle();
            assert.equal(await page.getByText("2 待你处理", { exact: true }).count(), 1);
        });
        assert.deepEqual(errors, [], "No runtime errors or non-fixture requests");
        await context.close();
    } finally {
        await browser?.close();
        await server?.close();
        await rm(scratch, { recursive: true, force: true });
    }
});
