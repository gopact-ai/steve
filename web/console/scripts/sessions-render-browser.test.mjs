// Real sidebar rows: unchanged search membership must not redraw the list.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import ts from "../node_modules/typescript/lib/typescript.js";

const web = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-sessions-render-"));
let server, browser;
try {
    await writeFile(path.join(scratch, "index.html"), '<style>svg{width:16px;height:16px}</style><div id="root"></div><script type="module" src="/entry.tsx"></script>');
    await writeFile(path.join(scratch, "entry.tsx"), `
        import { useState, useMemo } from "react";
        import { createRoot } from "react-dom/client";
        import { MemoryRouter } from "react-router";
        import { SessionsTree } from "@/components/steve/sessions-tree";
        import { LocaleProvider, useI18n } from "@/providers/locale-provider";
        import { NodeNamesContext } from "@/lib/node-name";
        const projects = [{id:"p",node:"node",workspaces:[],agents:[]}];
        function Fixture() {
            const [list,setList] = useState(() => Array.from({length:1000},(_,i)=>({id:"c"+i,title:"Conversation "+String(i).padStart(4,"0"),project:"p",agent:"agent",count:0,running:false,last_at:"2026-09-19T00:00:00Z",place:{node:"node",kind:"canonical"}})));
            const [current,setCurrent] = useState("c0"), [tasks,setTasks] = useState([]), [generation,setGeneration] = useState(0), [nodeName,setNodeName] = useState("First node");
            const {setLocale} = useI18n();
            window.fixture = { setLocale, setGeneration, setNodeName, setTasks,
                change: (id,patch) => setList(list => list.map(c => c.id===id ? {...c,...patch} : c)) };
            const label = useMemo(() => () => nodeName, [nodeName]);
            return <NodeNamesContext.Provider value={label}><SessionsTree list={list} tasks={tasks} projects={projects} current={current}
                onPick={id=>{window.picked=id;setCurrent(id)}} onNew={()=>{}}
                onUpdate={(id,patch)=>{window.updates.push({id,patch,generation});setList(list=>list.map(c=>c.id===id?{...c,...patch}:c))}}
                onDelete={async id=>{window.deleted=id}} onTask={task=>{window.taskPicked=task.id}} /></NodeNamesContext.Provider>;
        }
        createRoot(document.getElementById("root")).render(<LocaleProvider><MemoryRouter><Fixture/></MemoryRouter></LocaleProvider>);
    `);
    await build({
        root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        resolve: { alias: { "@": path.join(web, "src"), "react-router": path.join(web, "node_modules/react-router"), "react-dom": path.join(web, "node_modules/react-dom"), "react": path.join(web, "node_modules/react") } },
        plugins: [{
            name: "count-real-session-rows", enforce: "pre",
            transform(source, id) {
                if (!id.endsWith("/src/components/steve/sessions-tree.tsx")) return;
                const ast = ts.createSourceFile(id, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
                let body;
                const visit = (node) => {
                    if ((ts.isFunctionDeclaration(node) || ts.isFunctionExpression(node)) && node.name?.text === "Thread") body = node.body;
                    ts.forEachChild(node, visit);
                };
                visit(ast);
                assert.ok(body, "Count the production Thread implementation");
                const at = body.getStart(ast) + 1;
                return source.slice(0, at) + 'window.rowRenders[c.id] = (window.rowRenders[c.id] || 0) + 1;' + source.slice(at);
            },
        }],
        esbuild: { jsx: "automatic" },
        build: { outDir: path.join(scratch, "dist"), minify: true, reportCompressedSize: false, chunkSizeWarningLimit: 10000 },
    });
    server = http.createServer(async (req, res) => {
        const url = new URL(req.url, "http://fixture");
        if (url.pathname !== "/" && !url.pathname.startsWith("/assets/")) { res.writeHead(404).end(); return; }
        try {
            const file = path.join(scratch, "dist", url.pathname === "/" ? "index.html" : url.pathname);
            res.setHeader("Content-Type", file.endsWith(".js") ? "application/javascript" : "text/html");
            res.end(await readFile(file));
        } catch { res.writeHead(404).end(); }
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    const context = await browser.newContext({ serviceWorkers: "block" }), page = await context.newPage(), errors = [];
    await context.route("**/*", (route) => {
        const request = route.request(), url = new URL(request.url());
        if (url.origin === origin && request.method() === "GET" && (url.pathname === "/" || url.pathname.startsWith("/assets/"))) return route.continue();
        errors.push(`Unexpected request: ${request.method()} ${url.pathname}`); return route.abort();
    });
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.addInitScript(() => { window.rowRenders = {}; window.updates = []; localStorage.setItem("steve.ui.locale", "en"); });
    const settle = () => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const reset = () => page.evaluate(() => { window.rowRenders = {}; });
    const renders = () => page.evaluate(() => Object.values(window.rowRenders).reduce((n, count) => n + count, 0));
    await page.goto(origin);
    await page.waitForFunction(() => document.querySelectorAll(".conversation-row").length === 1000);
    await settle();
    const search = page.getByRole("textbox", { name: "Search conversations" });
    await reset();
    await search.fill("Conversation");
    await settle();
    assert.equal(await renders(), 0, "Search with unchanged membership must not render 1000 rows");
    assert.equal(await page.locator(".conversation-row").count(), 1000);
    await search.fill("Conversation ");
    await settle();
    assert.equal(await renders(), 0, "Equivalent query and new parent action closures keep rows stable");

    await search.fill("Conversation 0001");
    await settle();
    assert.equal(await page.locator(".conversation-row").count(), 1, "Changed search membership is visible");
    const selected = page.locator(".conversation-row");
    await selected.press("Enter");
    assert.equal(await page.evaluate(() => window.picked), "c1");
    assert.equal(await selected.getAttribute("aria-current"), "page");
    await page.evaluate(() => window.fixture.setGeneration(2));
    await settle();
    await page.getByRole("button", { name: "More", exact: true }).click();
    await page.getByRole("menuitem", { name: "Rename", exact: true }).click();
    const rename = page.getByRole("textbox", { name: "Conversation name" });
    await rename.fill("Conversation 0001 renamed");
    await rename.press("Enter");
    await selected.getByText("Conversation 0001 renamed", { exact: true }).waitFor();
    assert.deepEqual(await page.evaluate(() => window.updates), [{ id: "c1", patch: { title: "Conversation 0001 renamed" }, generation: 2 }], "Rename retains identity and uses the latest callback closure");

    await search.fill("Conversation");
    await settle();
    await reset();
    await page.locator(".conversation-row").filter({ hasText: "Conversation 0000" }).press("Enter");
    await settle();
    assert.equal(await renders(), 2, "Only the old and new current rows update");
    await page.evaluate(() => window.fixture.change("c1", { running: true }));
    await settle();
    await page.evaluate(() => window.fixture.change("c1", { running: false }));
    await settle();
    const c1 = page.locator(".conversation-row").filter({ hasText: "Conversation 0001 renamed" });
    assert.equal(await c1.locator(".conversation-unseen").count(), 1, "Unseen completion still updates a memoized row");
    await c1.press("Enter");
    await settle();
    assert.equal(await c1.locator(".conversation-unseen").count(), 0);

    await page.evaluate(() => window.fixture.setTasks([{ id: "root", title: "Root work", goal: "Root work", transport: "console", channel: "c1", execution: "running", attention: 0 }]));
    await settle();
    await page.getByRole("button", { name: "Expand tasks for Conversation 0001 renamed", exact: true }).press("Enter");
    await page.getByRole("button", { name: /#root.*Root work/ }).press("Enter");
    assert.equal(await page.evaluate(() => window.taskPicked), "root");
    await reset();
    await search.fill("Conversation ");
    await settle();
    assert.equal(await renders(), 0, "An expanded work row also keeps its stable task and child lookup inputs");
    await page.evaluate(() => window.fixture.setTasks((tasks) => [...tasks, { id: "child", parent: "root", title: "Child work", goal: "Child work", transport: "console", channel: "c1", execution: "running", attention: 0 }]));
    await page.getByRole("button", { name: /#child.*Child work/ }).press("Enter");
    assert.equal(await page.evaluate(() => window.taskPicked), "child", "A changed child index still reaches expanded work");
    await page.evaluate(() => window.fixture.setNodeName("Renamed node"));
    await settle();
    assert.match(await c1.getAttribute("title"), /Renamed node/, "Node label context still invalidates rows");
    const englishTime = await c1.locator(".conversation-time").innerText();
    await page.evaluate(() => window.fixture.setLocale("zh"));
    await page.getByRole("textbox", { name: "搜索会话" }).waitFor();
    const chineseTime = await c1.locator(".conversation-time").innerText();
    assert.notEqual(chineseTime, englishTime, "Locale context still updates row formatting");
    assert.match(chineseTime, /[\u4e00-\u9fff]/);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("textbox", { name: "搜索会话" }).fill("Conversation 0001");
    await c1.press("Enter");
    assert.equal(await c1.getAttribute("aria-current"), "page");
    assert.deepEqual(errors, []);
    console.log("1000 rows: unchanged search renders 0; filtering, keyboard selection, rename/latest closure, current/unseen/work, locale and node labels passed");
    await context.close();
} finally {
    await browser?.close();
    if (server) { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)); }
    await rm(scratch, { recursive: true, force: true });
}
