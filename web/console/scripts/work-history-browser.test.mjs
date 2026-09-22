// Actual workbench consumers, isolated from all non-test APIs.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
const server = await createServer({ root: fileURLToPath(new URL("..", import.meta.url)), logLevel: "error", server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(12000);
await page.clock.install();
const at = "2026-09-19T00:00:00Z";
const task = (id, overrides = {}) => ({ id, goal: `Historical task ${id}`, state: "done", lifecycle: "done", execution: "idle", lane: "ended", attention: 0, turns: 10021, max_turns: 0, transport: "console", channel: "opaque-conversation", project_id: "p", children_count: 0, children_complete: true, attempt_count: 21, ...overrides });
const live = task("live", { state: "running", lifecycle: "running", lane: "pending", can_complete: true });
const historical = task("outside-state", { children_count: 21, children_complete: false });
const counts = { total: 10030, live: 1, closed: 10029, roots: 10009, completed_roots: 10008, cancelled_roots: 0, paused_roots: 0 };
const snapshot = { at, hub: { node: "fixture", started: at }, nodes: [], agents: [], tasks: [live, task("recent")], plans: [], projects: [{ id: "p", node: "fixture", path: "/test/p", level: "team", repo: "git", agents: [], workspaces: [], task_counts: counts }], attempts: [], landings: [], inbox: [], schedules: [], sources: [{ name: "tasks", wired: true }, { name: "plans", wired: true }], task_coverage: { ...counts, included: 2, recent_limit: 20, recent_closed: 1, has_more_closed: true }, plan_coverage: { total: 0, included: 0, has_more: false } };
const accounting = (index) => ({ index, execution_id: `exec-${index}`, day: "2026-09-19", agent: `accounting-${index}`, started: at, seconds: 1, tokens: { total: 1 }, reported: true });
const native = (id, tid, ended) => ({ id, task_id: tid, project: "p", kind: "chat", state: "closed", agent: "fixture", started_at: at, ended_at: ended, artifact: id + "-artifact", base: id + "-base", files: 1, files_known: true });
const plan = (id) => ({ id, task_id: historical.id, rev: 1, by: "fixture", state: "done", goal: `Plan ${id}`, steps: [{ id: `step-${id}`, state: "done", goal: `Verify ${id}` }] });
// The summary can be older than a successful detail/page read. Its timestamp
// is not a task revision and must never choose the Drawer owner.
live.title = "STALE SNAPSHOT TITLE";
live.updated_at = "2099-01-01T00:00:00Z";
let liveOwner = { ...live, title: "FRESH POINT OWNER", execution: "running", can_complete: false, children_count: 21, children_complete: false };
let childOwner = task("live-child-0", { parent: live.id, goal: "FRESH CHILD FROM DETAIL", result_delivery: { state: "uncertain", at } });
snapshot.tasks.push({ ...childOwner, goal: "STALE CHILD FROM STATE", result_delivery: { state: "pending", at } },
 task("live-child-20", { parent: live.id, goal: "STALE PAGE CHILD FROM STATE" }));
snapshot.task_coverage.included = snapshot.tasks.length;
snapshot.task_coverage.recent_closed = 3;
snapshot.plans.push({ ...plan("stale"), task_id: live.id, steps: [{ id: "old", goal: "STALE PLAN FROM STATE", state: "done" }] });
let livePlan = { ...plan("fresh"), task_id: live.id, steps: [{ id: "new", goal: "FRESH PLAN FROM DETAIL", state: "running" }] };
const requests = [], errors = [];
let failPage = false, stale = false, failDetail = false, allowCompletion = false;
let failStateAfterCompletion = false, stateUnavailable = false, rejectedStateReads = 0;
let holdDetail = false, releaseDetail, detailStarted;
await context.route("**/*", async (route) => {
 const req = route.request(), url = new URL(req.url()), p = url.pathname;
 if (url.origin !== origin) { errors.push(`external: ${url.origin}`); return route.abort(); }
 if (p === "/state") {
  requests.push(p);
  if (stateUnavailable) { rejectedStateReads++; return route.fulfill({status:503,body:"independent summary unavailable"}); }
  return route.fulfill({ json: snapshot });
 }
 if (!p.startsWith("/console/") && !["/events", "/usage", "/history"].includes(p)) return route.continue();
 requests.push(p + url.search);
 if (p === "/console/send" && allowCompletion) {
  const body=req.postDataJSON();
  assert.equal(req.method(),"POST");
  assert.equal(body.conversation,"opaque-conversation");
  assert.equal(body.input,`/tasks complete ${historical.id}`);
  historical.lifecycle="done"; historical.can_complete=false;
  stateUnavailable=failStateAfterCompletion;
  return route.fulfill({json:{reply:{kind:"reply",text:"Historical task completed"}}});
 }
 if (req.method() !== "GET") { errors.push(`unexpected write: ${p}`); return route.abort(); }
 if (p === "/console/tasks/live") {
  if (failDetail) return route.fulfill({ status: 503, body: "point owner temporarily unavailable" });
  const value = structuredClone({ task: liveOwner, plan: livePlan, children: { items: [childOwner, ...Array.from({length:19}, (_,i)=>task(`live-child-${i+1}`,{parent:live.id}))], total: 21, next_cursor: "live-child-page-2" }, accounting: { items: [accounting(0)], total: 21, next_cursor: "live-accounting-page-2" } });
  if (holdDetail) {
   value.task.title="POINT AFTER PAGE";
   value.children.items[19] = task("live-child-20", {parent:live.id, goal:"OLDER IN-FLIGHT POINT CHILD"});
   await new Promise(resolve=>{releaseDetail=resolve; detailStarted();});
  }
  return route.fulfill({ json: value });
 }
 if (p === "/console/tasks") {
  const cursor = url.searchParams.get("cursor");
  if (cursor && stale) return route.fulfill({ status: 409, body: "task query changed; refresh" });
  if (cursor && failPage) { failPage = false; return route.fulfill({ status: 503, body: "temporary history failure" }); }
  if (url.searchParams.get("scope") === "children") return route.fulfill({ json: { items: [url.searchParams.get("scope_id") === live.id ? task("live-child-20", { parent: live.id, goal: "FRESH PAGE CHILD FROM QUERY" }) : task("child-20", { parent: historical.id })], total: 21 } });
  return route.fulfill({ json: cursor ? { items: [task("oldest")], total: 10030 } : { items: [historical], total: 10030, next_cursor: "opaque-page-2" } });
 }
 if (p === `/console/tasks/${historical.id}`) return route.fulfill({ json: { task: historical, children: { items: Array.from({length:20}, (_,i)=>task(`child-${i}`,{parent:historical.id})), total: 21, next_cursor: "child-page-2" }, accounting: { items: Array.from({length:20}, (_,i)=>accounting(i)), total: 21, next_cursor: "accounting-page-2" } } });
 if (p.endsWith("/accounting")) return route.fulfill({ json: { items: [accounting(20)], total: 21 } });
 if (p === "/console/plans") {
  assert.equal(url.searchParams.get("task_id"), historical.id);
  return route.fulfill({ json: url.searchParams.get("cursor") ? { items: [plan("old")], total: 2 } : { items: [plan("latest")], total: 2, next_cursor: "plan-page-2" } });
 }
 if (p === "/console/attempts") {
  if (url.searchParams.has("task_id")) {
   assert.equal(url.searchParams.get("task_id"), historical.id);
   assert.equal(url.searchParams.has("conversation"), false);
   return route.fulfill({ json: { items: [native("native-task-detail", historical.id, "2026-09-19T00:00:10Z")] } });
  }
  return route.fulfill({ json: url.searchParams.get("cursor") ? { items: [native("native-old", "other-old-task", "2026-09-19T00:00:01Z")] } : { items: [native("native-latest", "outside-state", "2026-09-19T00:00:09Z")], next_cursor: "native-page-2" } });
 }
 if (p.endsWith("/tree")) return route.fulfill({ json: { attempt: p.split("/")[3], commit: p.split("/")[3] + "-artifact", which: "result", dir: "", entries: [{ name: "proof.txt", path: "proof.txt", kind: "file", size: 15 }] } });
 if (p.endsWith("/file")) return route.fulfill({ json: { attempt: p.split("/")[3], commit: p.split("/")[3] + "-artifact", path: "proof.txt", text: `proof for ${p.split("/")[3]}`, size: 15 } });
 if (p.endsWith("/changes")) return route.fulfill({ json: { attempt: p.split("/")[3], artifact: p.split("/")[3] + "-artifact", base: p.split("/")[3] + "-base", changes: [] } });
 if (p === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
 if (p === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [{ id: "opaque-conversation", title: "History consumer", project: "p", count: 1, last_at: at }] } });
 if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [] } });
 if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
 return route.fulfill({ json: { enabled: true, items: [], total: 0, queue: [], replies: [], verbs: [], suggestions: [], questions: [], nodes: [], events: [], conversations: [], entries: [] } });
});
page.on("pageerror", (e) => errors.push(String(e)));
await page.addInitScript(() => { sessionStorage.setItem("steve.conversation", "opaque-conversation"); localStorage.setItem("steve.ui.locale", "en"); window.EventSource = class { constructor() { window.fixtureEventSource=this; setTimeout(()=>this.onopen?.(),0); } close() {} }; });
try {
 await page.goto(`${origin}/#/console?view=board`);
 await page.getByRole("button", {name: /Open task #live /}).click();
 const ownerDrawer = page.getByRole("dialog");
 await ownerDrawer.getByText("accounting-0", {exact:true}).waitFor();
 assert.ok((await ownerDrawer.innerText()).includes("FRESH POINT OWNER"), "point-read owner, never stale /state title");
 assert.equal(await ownerDrawer.getByRole("button", {name:"Complete task",exact:true}).count(), 0, "running owner must not expose stale completion eligibility");
 await ownerDrawer.getByText("First message: FRESH CHILD FROM DETAIL", {exact:true}).waitFor();
 assert.equal((await ownerDrawer.innerText()).includes("STALE CHILD FROM STATE"), false);
 assert.equal((await ownerDrawer.innerText()).includes("STALE PLAN FROM STATE"), false, "a previous plan ID for the same task is not the current detail plan");
 // A point revalidation started before a page request may finish later. Its
 // earlier image must not overwrite the page's successful owner response.
 holdDetail=true;
 const started=new Promise((resolve,reject)=>{
  const timeout=setTimeout(()=>reject(new Error("Related state change did not start point revalidation")),12000);
  detailStarted=()=>{clearTimeout(timeout);resolve();};
 });
 live.title="INVALIDATE BEFORE PAGE";
 await page.clock.fastForward(11000); await page.clock.runFor(500);
 await started;
 await ownerDrawer.getByRole("button", {name:"More subtasks",exact:true}).click();
 await ownerDrawer.getByText("First message: FRESH PAGE CHILD FROM QUERY", {exact:true}).waitFor();
 const response=page.waitForResponse(r=>new URL(r.url()).pathname==="/console/tasks/live" && r.status()===200);
 holdDetail=false; releaseDetail(); releaseDetail=null;
 await response;
 await ownerDrawer.getByRole("heading", {name:"#live POINT AFTER PAGE", exact:true, level:2}).waitFor();
 await ownerDrawer.getByText("First message: FRESH PAGE CHILD FROM QUERY", {exact:true}).waitFor();
 assert.equal((await ownerDrawer.innerText()).includes("OLDER IN-FLIGHT POINT CHILD"),false);
 await ownerDrawer.getByRole("button", {name:"More turns",exact:true}).click();
 await ownerDrawer.getByText("accounting-20", {exact:true}).waitFor();
 const detailReads=()=>requests.filter(r=>r==="/console/tasks/live").length;
 const childReads=()=>requests.filter(r=>r.startsWith("/console/tasks?") && r.includes("scope_id=live")).length;
 const stableDetail=detailReads(), stableChildren=childReads();
 await page.clock.fastForward(11000); await page.clock.runFor(500);
 assert.equal(detailReads(),stableDetail,"unchanged polling must not reread detail");
 assert.equal(childReads(),stableChildren);
 // A related summary change invalidates the owner read, not the history
 // cursor. Even a newer-looking summary must not replace owner content.
 live.title = "ANOTHER STALE SNAPSHOT";
 childOwner = { ...childOwner, result_delivery: { state: "delivered", at } };
 liveOwner = { ...liveOwner, title: "REFRESHED POINT OWNER" };
 livePlan = undefined; // Explicit no-plan must also hide the old summary plan.
 await page.clock.fastForward(11000); await page.clock.runFor(500);
 await ownerDrawer.getByRole("heading", {name:"#live REFRESHED POINT OWNER", exact:true, level:2}).waitFor();
 await ownerDrawer.getByText("Delivered", {exact:true}).waitFor();
 await ownerDrawer.getByText("accounting-20", {exact:true}).waitFor();
 await ownerDrawer.getByText("First message: FRESH PAGE CHILD FROM QUERY", {exact:true}).waitFor();
 assert.equal(childReads(),stableChildren,"owner invalidation does not restart child history");
 assert.equal((await ownerDrawer.innerText()).includes("STALE PLAN FROM STATE"),false);
 assert.equal((await ownerDrawer.innerText()).includes("FRESH PLAN FROM DETAIL"),false);
 failDetail=true; live.title="INVALIDATE FAILED READ";
 await page.clock.fastForward(11000); await page.clock.runFor(500);
 await ownerDrawer.getByRole("alert").filter({hasText:"point owner temporarily unavailable"}).waitFor();
 assert.equal((await ownerDrawer.innerText()).includes("INVALIDATE FAILED READ"),false,"failed owner read must not fall back to summary");
 failDetail=false;
 await ownerDrawer.getByRole("alert").getByRole("button",{name:"Retry",exact:true}).click();
 await ownerDrawer.getByRole("alert").waitFor({state:"hidden"});
 // Only an explicit user refresh resets pages. Background owner reads must
 // also preserve a rejected cursor rather than silently clearing its 409.
 await ownerDrawer.getByRole("button",{name:"Refresh list",exact:true}).click();
 await ownerDrawer.getByText("accounting-0",{exact:true}).waitFor();
 stale=true;
 await ownerDrawer.getByRole("button",{name:"More subtasks",exact:true}).click();
 await ownerDrawer.getByRole("alert").filter({hasText:"Records changed"}).waitFor();
 const invalidatedReads=detailReads();
 snapshot.plans[0].rev++;
 await page.clock.fastForward(11000); await page.clock.runFor(500);
 for(let i=0;i<100 && detailReads()===invalidatedReads;i++) await new Promise(r=>setTimeout(r,20));
 assert.ok(detailReads()>invalidatedReads,"same-task plan change also invalidates its owner read");
 await ownerDrawer.getByRole("alert").filter({hasText:"Records changed"}).waitFor();
 assert.equal(await ownerDrawer.getByRole("button",{name:"More subtasks",exact:true}).isEnabled(),false);
 stale=false;
 await ownerDrawer.getByRole("button",{name:"Close",exact:true}).click();
 await page.goto(`${origin}/#/console?view=board&tab=all`);
 await page.getByRole("row").filter({hasText:"outside-state"}).waitFor();
 assert.ok(requests.some(r=>r.startsWith("/console/tasks?")), "Board must query history, not treat /state as complete");
 await page.getByRole("region", {name:"Root task summary"}).getByText("10009", {exact:true}).waitFor();
 await page.getByRole("row").filter({hasText:"outside-state"}).click();
 const drawer = page.getByRole("dialog");
 await drawer.getByText("accounting-19", {exact:true}).waitFor();
 await drawer.getByRole("button",{name:"More turns",exact:true}).click();
 await drawer.getByText("accounting-20", {exact:true}).waitFor();
 await drawer.getByRole("button",{name:"More subtasks",exact:true}).click();
 await drawer.getByText("#child-20",{exact:true}).waitFor();
 await drawer.getByRole("button",{name:"Close",exact:true}).click();
 await drawer.waitFor({state:"hidden"});
 failPage = true;
 await page.getByRole("button", {name:"Load more",exact:true}).click();
 await page.getByRole("alert").filter({hasText:"temporary history failure"}).waitFor();
 await page.getByRole("button",{name:"Retry",exact:true}).last().click();
 await page.getByRole("row").filter({hasText:"oldest"}).waitFor();

 // Explicit invalidation: no automatic first-page fallback after 409.
 await page.getByRole("button", {name:"Refresh list",exact:true}).click();
 await page.getByRole("row").filter({hasText:"outside-state"}).waitFor();
 stale = true;
 await page.getByRole("button",{name:"Load more",exact:true}).click();
 await page.getByRole("alert").filter({hasText:"Records changed"}).waitFor();
 assert.equal(await page.getByRole("button",{name:"Load more",exact:true}).isEnabled(),false);
 stale = false;
 await page.getByRole("alert").getByRole("button",{name:"Refresh list",exact:true}).click();
 // State polling must not reread user-opened history or native files.
 const nativeReads=()=>requests.filter(r=>r.startsWith("/console/attempts?")).length;
 assert.equal(nativeReads(),0,"closed files must not request native history");
 await page.evaluate(()=>{location.hash="/console";});
 await page.getByRole("button",{name:"Show details",exact:true}).click();
 await page.getByRole("tab",{name:"Artifacts",exact:true}).click();
 const panel=page.getByRole("complementary",{name:"Details",exact:true});
 await panel.getByRole("button",{name:"Browse files",exact:true}).waitFor();
 assert.ok(requests.filter(r=>r.startsWith("/console/attempts?")).every(r=>new URL(r,"http://fixture").searchParams.get("conversation")==="opaque-conversation"));
 assert.equal(requests.some(r=>/\/console\/tasks\/.*\/attempts/.test(r)),false,"no per-task frontend fanout");
 const beforeNative=nativeReads(), beforeState=requests.filter(r=>r==="/state").length;
 await page.clock.fastForward(16000); await page.clock.runFor(500);
 for(let i=0;i<100 && requests.filter(r=>r==="/state").length===beforeState;i++) await new Promise(r=>setTimeout(r,20));
 assert.ok(requests.filter(r=>r==="/state").length>beforeState);
 assert.equal(nativeReads(),beforeNative,"/state changes must not reread files");
 await page.clock.resume();
 await panel.getByRole("button",{name:"Load more",exact:true}).click();
 await panel.locator("summary").click();
 await panel.getByText(/#other-old-task/).waitFor();
 await panel.getByRole("button",{name:"Browse files",exact:true}).click();
 const workspace=page.getByRole("dialog",{name:"Artifact workspace",exact:true});
 await workspace.getByRole("button",{name:"proof.txt",exact:true}).click();
 await workspace.getByText("proof for native-latest",{exact:false}).waitFor();
 assert.ok(requests.some(r=>r.startsWith("/console/attempts/native-latest/file")),"old task's newest result is latest");
 await workspace.getByRole("button",{name:/Select version/}).click();
 await page.getByRole("option",{name:/other-old-task/}).click();
 await workspace.getByRole("button",{name:"proof.txt",exact:true}).click();
 await workspace.getByText("proof for native-old",{exact:false}).waitFor();
 assert.ok(requests.some(r=>r.startsWith("/console/attempts/native-old/file")),"page two uses real native ID");
 await workspace.getByRole("button",{name:"Back",exact:true}).click();
 // Owner project count, not the two /state task cards.
 await page.evaluate(()=>{location.hash="/projects";});
 await page.getByRole("row").filter({hasText:"/test/p"}).getByRole("button",{name:/Project.*menu|p.*menu|More actions/i}).click();
 await page.getByRole("menuitem",{name:"Delete project",exact:true}).click();
 await page.getByRole("dialog").getByText(/10030/).waitFor();
 await page.getByRole("dialog").getByRole("button",{name:"Cancel",exact:true}).click();
 // Narrow, long-content and keyboard path on the actual historical detail.
 await page.setViewportSize({width:390,height:1000});
 historical.goal="Historical long task title with opaque identity ".repeat(8);
 await page.evaluate(()=>{location.hash="/console?view=board&tab=all";});
 await page.getByRole("row").filter({hasText:"outside-state"}).click();
 await drawer.getByText("accounting-19",{exact:true}).waitFor();
 await drawer.getByRole("button",{name:"More turns",exact:true}).focus();
 await page.keyboard.press("Enter");
 await drawer.getByText("accounting-20",{exact:true}).waitFor();
 assert.ok(await drawer.evaluate(el=>el.scrollWidth<=el.clientWidth),"drawer must fit narrow screen");
 await mkdir("/tmp/steve-task-hot-read/ui-after",{recursive:true});
 await page.screenshot({path:"/tmp/steve-task-hot-read/ui-after/work-history-narrow.png"});
 // Historical Drawer queries plans and task-native files only on expansion.
 const planReads=()=>requests.filter(r=>r.startsWith("/console/plans?")).length;
 const taskFiles=()=>requests.filter(r=>r.startsWith("/console/attempts?") && new URL(r,"http://fixture").searchParams.has("task_id")).length;
 assert.equal(planReads(),0);
 assert.equal(taskFiles(),0);
 const plans=drawer.locator("details").filter({has:page.locator(":scope > summary").filter({hasText:"Plan history (current revisions)"})});
 await drawer.locator("summary").filter({hasText:"Plan history (current revisions)"}).click();
 await plans.getByText(/Plan latest/).waitFor();
 await plans.getByRole("button",{name:"Load more",exact:true}).click();
 await plans.getByText(/Plan old/).waitFor();
 await drawer.locator("summary").filter({hasText:"File snapshots"}).click();
 await drawer.getByRole("button",{name:"Browse files",exact:true}).click();
 await workspace.getByRole("button",{name:"proof.txt",exact:true}).click();
 await workspace.getByText("proof for native-task-detail",{exact:false}).waitFor();
 // The source-preview's StrictMode may start and abort the initial read once.
 // There is still exactly one scope, and only the explicit second plan cursor.
 assert.ok(taskFiles() >= 1 && taskFiles() <= 2,"one task scope, no per-child fanout");
 assert.deepEqual([...new Set(requests.filter(r=>r.startsWith("/console/plans?")).map(r=>new URL(r,"http://fixture").searchParams.get("cursor") || ""))],["","plan-page-2"]);
 assert.ok(planReads() <= 3,"only initial read (plus StrictMode probe) and one page");

 // Completing a historical task absent from /state still rereads that owner,
 // even when the summary poll has no changed task row to invalidate.
 await workspace.getByRole("button",{name:"Back",exact:true}).click();
 historical.lifecycle="running"; historical.can_complete=true;
 await drawer.getByRole("button",{name:"Refresh list",exact:true}).first().click();
 const complete=drawer.getByRole("button",{name:"Complete task",exact:true});
 await complete.waitFor();
 allowCompletion=true;
 await complete.click();
 await drawer.getByRole("status").filter({hasText:"Historical task completed"}).waitFor();
 await complete.waitFor({state:"hidden"});
 assert.equal(requests.filter(r=>r==="/console/send").length,1,"only the explicit command may write");

 // The same successful command must revalidate its point owner even while
 // every shared-summary refresh fails. No resend or recovery of /state may
 // become a prerequisite for displaying the acknowledged durable result.
 historical.lifecycle="running"; historical.can_complete=true;
 await drawer.getByRole("button",{name:"Refresh list",exact:true}).first().click();
 await complete.waitFor();
 const historicalReads=()=>requests.filter(r=>r===`/console/tasks/${historical.id}`).length;
 const beforeCompletion=historicalReads();
 failStateAfterCompletion=true;
 const rejectedState=page.waitForResponse(r=>new URL(r.url()).pathname==="/state" && r.status()===503);
 await complete.click();
 await drawer.getByRole("status").filter({hasText:"Historical task completed"}).waitFor();
 await page.clock.runFor(1000); await rejectedState;
 const rejectedPoll=page.waitForResponse(r=>new URL(r.url()).pathname==="/state" && r.status()===503);
 await page.clock.fastForward(11000); await page.clock.runFor(500); await rejectedPoll;
 assert.ok(rejectedStateReads>=2,"the independent summary remains unavailable");
 assert.ok(historicalReads()>beforeCompletion,"successful action must reread its healthy owner despite /state failure");
 await complete.waitFor({state:"hidden"});
 assert.equal(await complete.count(),0,"acknowledged completion must remove stale eligibility");
 assert.equal(requests.filter(r=>r==="/console/send").length,2,"state recovery must not resubmit either command");

 assert.deepEqual(errors, []);
 console.log("Work history consumers: Board counts/page/409, Drawer children/accounting/plan pages/task-native files, conversation files on demand without state fanout, project counts, narrow keyboard");
} catch (error) { console.error(requests, errors); console.error((await page.locator("body").innerText()).slice(-14000)); await page.screenshot({path:"/tmp/steve-task-hot-read/consumer-failure.png"}); throw error; } finally { releaseDetail?.(); await context.close(); await browser.close(); await server.close(); }
