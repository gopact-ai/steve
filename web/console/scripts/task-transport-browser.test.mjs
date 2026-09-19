import { workState, workDetail, nativeHistory } from "../../../e2e/console/work-fixture.mjs";
// Real task controls in an isolated browser. Every API call is intercepted.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const server = await createServer({
    root: fileURLToPath(new URL("..", import.meta.url)), logLevel: "error",
    plugins: [{ name: "task-transport-fixture", enforce: "pre", transform(source, id) {
        if (!id.endsWith("/src/app.tsx")) return;
        return `import { TaskDrawer } from "@/components/steve/task-drawer";
            import { TaskCloseDialog, useTaskClose } from "@/components/steve/task-close";\n` +
            source.replace("<Shell />", "<Shell /><TaskTransportProbe />") + `
            function TaskTransportProbe() {
                const { snap } = useFleet();
                return snap.tasks[0] ? <TaskTransportControls task={snap.tasks[0]} /> : null;
            }
            function TaskTransportControls({task}) {
                const [opened, setOpened] = useState(false);
                const close = useTaskClose(task);
                return <div style={{position:"fixed",top:0,right:0,zIndex:9999}}>
                    <button onClick={()=>setOpened(true)}>Fixture task</button>
                    {close.closable && <button onClick={close.ask}>Fixture close</button>}
                    <button onClick={close.ask}>Fixture stale confirmation</button>
                    {opened && <TaskDrawer t={task} tasks={[task]} onClose={()=>setOpened(false)} />}
                    {close.asking && <TaskCloseDialog t={task} onClose={close.dismiss} />}
                </div>;
            }`;
    } }],
    server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(10000);
const at = "2026-09-19T00:00:00Z";
let task;
const requests = [], errors = [];
let visit = 0;
const base = { id: "transport-task", goal: "Transport fixture", member: "fixture", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, turns: 0, max_turns: 0, can_complete: true };
await context.route("**/*", async (route) => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
    if (p === `/console/tasks/${task.id}`) return route.fulfill({ json: workDetail(task) });
    if (p === "/state") return route.fulfill({ json: workState({ at, hub: { node: "fixture", started: at }, nodes: [], agents: [], tasks: [task], projects: [], plans: [], attempts: [], landings: [] }) });
    if (!p.startsWith("/console/") && !["/events", "/history"].includes(p)) return route.continue();
    if (p === "/console/send" && req.method() === "POST") {
        requests.push(req.postDataJSON());
        return route.fulfill({ json: { reply: { kind: "reply", text: "Fixture accepted" } } });
    }
    if (req.method() !== "GET") { errors.push(`Unexpected write: ${p}`); return route.abort(); }
    if (p === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
    if (p === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [
        { id: task.channel, title: "Opaque task conversation", count: 1, last_at: at },
    ] } });
    if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: url.searchParams.get("conversation") === task.channel
        ? [{ id: "opaque-history", conversation: task.channel, kind: "notice", text: "Actual opaque conversation history", at }] : [] } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    return route.fulfill({ json: { enabled: true, conversations: [], replies: [], verbs: [], suggestions: [], questions: [], attempts: [] } });
});
page.on("pageerror", (error) => errors.push(String(error)));
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
});
try {
    for (const address of [
        { transport: "feishu", channel: "console:opaque-native-id" },
        { channel: "console:unknown" },
        { transport: "console", channel: "" },
        { transport: "console", channel: "opaque-without-prefix" },
    ]) {
        task = { ...base, ...address };
        const allowed = address.transport === "console" && !!address.channel;
        await page.goto(`${origin}/?fixture=${++visit}#/console`);
        await page.getByRole("button", { name: "Fixture task", exact: true }).click();
        const drawer = page.getByRole("dialog");
        const complete = drawer.getByRole("button", { name: "Complete task", exact: true });
        await complete.waitFor();
        assert.equal(await complete.isEnabled(), allowed, JSON.stringify(address));
        assert.equal(await drawer.getByRole("button", { name: "Open in workbench", exact: true }).isEnabled(), allowed);
        for (const button of await drawer.getByRole("button").all()) {
            if (["Pause", "Cancel"].includes(await button.innerText())) assert.equal(await button.isEnabled(), allowed);
        }
        if (process.env.PHASE4_SCREENSHOTS) {
            await mkdir(process.env.PHASE4_SCREENSHOTS, { recursive: true });
            await page.screenshot({ path: path.join(process.env.PHASE4_SCREENSHOTS, `task-transport-${visit}.png`) });
        }
        if (allowed) {
            await drawer.getByRole("button", { name: "Open in workbench", exact: true }).click();
            await page.waitForFunction((id) => sessionStorage.getItem("steve.conversation") === id, address.channel);
            await page.locator(".transcript-messages").getByText("Actual opaque conversation history", { exact: true }).waitFor();
            await page.locator("main header").getByText("Opaque task conversation", { exact: true }).waitFor();
            assert.equal(new URL(await page.evaluate(() => location.href)).hash, "#/console");
        } else await page.keyboard.press("Escape");
        assert.equal(await page.getByRole("button", { name: "Fixture close", exact: true }).count(), Number(allowed));
        // Exercise the dialog guard even if a stale caller retained a close action.
        await page.getByRole("button", { name: "Fixture stale confirmation", exact: true }).click();
        const before = requests.length;
        await page.getByRole("dialog").getByRole("button", { name: "End task", exact: true }).click();
        if (allowed) {
            await page.getByRole("dialog").waitFor({ state: "hidden" });
            assert.equal(requests.length, before + 1);
            assert.equal(requests.at(-1).conversation, address.channel);
            assert.equal(requests.at(-1).input, "/tasks complete transport-task");
        } else {
            await page.getByRole("dialog").getByRole("alert").waitFor();
            assert.equal(requests.length, before);
        }
    }
    assert.deepEqual(errors, []);
    console.log("Task transport controls: native/unknown fail closed; opaque console identity preserved");
} finally {
    await context.close(); await browser.close(); await server.close();
}
