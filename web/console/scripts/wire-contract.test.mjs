// Compile owner fixtures on every run, then feed their bytes through the real
// TS API and public conversation projection. No saved synthetic JSON contract.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, realpath, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import ts from "typescript";
import { createConversationProjection, reduceConversation } from "../src/lib/conversation-projection.ts";
import { ConversationController } from "../src/lib/conversation-controller.ts";

const web = fileURLToPath(new URL("..", import.meta.url));
// Go resolves cwd symlinks before matching overlay paths (/tmp on macOS).
const repo = await realpath(process.env.CONSOLE_GO_ROOT || path.resolve(web, "../.."));
const temp = await mkdtemp(path.join(tmpdir(), "console-wire-"));
try {
    const output = path.join(temp, "wire.json"), overlay = path.join(temp, "overlay.json");
    await writeFile(overlay, JSON.stringify({ Replace: {
        [path.join(repo, "internal/httpapi/console_frontend_wire_test.go")]: path.join(web, "scripts/wire-fixture.go.txt"),
    } }));
    const go = spawnSync("go", ["test", "-overlay", overlay, "./internal/httpapi", "-run", "^TestConsoleWireFixture$", "-count=1"], {
        cwd: repo, encoding: "utf8", env: { ...process.env, CONSOLE_WIRE_FIXTURE: output },
    });
    assert.equal(go.status, 0, go.stdout + go.stderr);
    const fixture = JSON.parse(await readFile(output, "utf8"));
    // Compile the actual serialized Go values against the consumed TS shapes.
    const types = path.join(web, "src/lib/types.ts");
    const compile = path.join(temp, "wire.ts");
    const queue = JSON.parse(fixture.queue.body), ack = JSON.parse(fixture.ack.body);
    const contract = (value, type) => `(${JSON.stringify(value)} satisfies ${type});`;
    const parseSSE = (stream) => stream.trim().split("\n\n").map((frame) => JSON.parse(frame.replace(/^data: /, "")));
    await writeFile(compile, `import type { Exchange, Event, Reply, Snapshot, WorkPage, Task, TaskDetail, AccountingItem, NativeAttempt, Plan } from ${JSON.stringify(types)};\n` +
        Object.entries({ state: "Snapshot", tasks: "WorkPage<Task>", detail: "TaskDetail", accounting: "WorkPage<AccountingItem>", plans: "WorkPage<Plan>", attempts: "WorkPage<NativeAttempt>" }).map(([name,type]) => contract(JSON.parse(fixture.work[name].body), type)).join("\n") +
        contract(queue.queue, "Exchange[]") + contract(ack, "Exchange") + contract(fixture.events, "Event[]")
        + contract(fixture.retry_replies, "Reply[]") + contract(parseSSE(fixture.nano_sse), "Event[]")
        + Object.values(fixture.retry).map((r) => contract(JSON.parse(r.body).queue, "Exchange[]")).join("\n"));
    const program = ts.createProgram([compile], { noEmit: true, strict: true, skipLibCheck: true, allowImportingTsExtensions: true, target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, moduleResolution: ts.ModuleResolutionKind.Bundler });
    const diagnostics = ts.getPreEmitDiagnostics(program);
    assert.equal(diagnostics.length, 0, ts.formatDiagnosticsWithColorAndContext(diagnostics, {
        getCurrentDirectory: () => temp, getCanonicalFileName: (file) => file, getNewLine: () => "\n",
    }));
    for (const [name, source] of [["http", "src/lib/http.ts"], ["console", "src/lib/api/console.ts"], ["work", "src/lib/api/work.ts"]]) {
        let code = ts.transpileModule(await readFile(path.join(web, source), "utf8"), { compilerOptions: { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext } }).outputText;
        if (name !== "http") code = code.replace('from "../http"', 'from "./http.mjs"');
        await writeFile(path.join(temp, `${name}.mjs`), code);
    }
    globalThis.window = { location: { search: "" } };
    globalThis.sessionStorage = { getItem: () => "" };
    const http = await import(pathToFileURL(path.join(temp, "http.mjs")));
    const api = await import(pathToFileURL(path.join(temp, "console.mjs")));
    const work = await import(pathToFileURL(path.join(temp, "work.mjs")));
    let response = fixture.queue;
    const reads = [];
    globalThis.fetch = async (url, options) => {
        reads.push({ url, options });
        const value = options?.method === "POST" ? fixture.ack : response;
        return new Response(value.body, { status: value.status });
    };
    assert.equal(fixture.queue.cache, "no-store");
    const got = await api.fetchQueue("console:wire");
    assert.deepEqual(got, queue);
    assert.equal(reads.at(-1).options.cache, "no-store");
    assert.deepEqual(api.getSubmissionSupport(), { state: "supported", checking: false, error: "", material_refs: true, interactive_requests: true });
    assert.deepEqual(await api.enqueue("console:wire", "wire input", [{ conversation: "console:source", reply_id: "r1" }], "wire-command"), ack);
    assert.equal(ack.key, "client:wire-command");
    assert.equal(Object.hasOwn(ack, "history"), false, "handler strips private prompt history");
    assert.deepEqual(ack.quotes, [{ conversation: "console:source", reply_id: "r1" }]);
    response = fixture.empty;
    assert.deepEqual((await api.fetchQueue("console:empty")).queue, []);
    assert.equal(api.getSubmissionSupport().material_refs, false);
    assert.equal(api.getSubmissionSupport().interactive_requests, false);
    const expected = { invalid: [400, true], missing: [404, true], rewound: [404, true], not_queued: [409, false], conflict: [409, false], closing: [503, false], unwired: [501, false] };
    for (const [name, [status, rejected]] of Object.entries(expected)) {
        response = fixture.errors[name];
        await assert.rejects(http.request("/console/queue"), (error) => {
            assert.ok(error instanceof http.HTTPError);
            assert.equal(error.status, status, name);
            const message = name === "unwired" ? response.body.trim() : JSON.parse(response.body).error;
            assert.equal(error.message, message);
            assert.equal(http.isRejectedRequest(error), rejected, name);
            return true;
        });
    }
    for (const [name, read] of Object.entries({ tasks: () => work.fetchTasks({}, ""), detail: () => work.fetchTask("root"), accounting: () => work.fetchTaskAccounting("root"), plans: () => work.fetchPlans("root"), attempts: () => work.fetchAttempts({ conversation: "opaque" }) })) {
        response = fixture.work[name];
        assert.equal(response.status, 200, name);
        assert.equal(response.cache, "no-store", name);
        assert.deepEqual(await read(), JSON.parse(response.body), name);
        assert.equal(reads.at(-1).options.cache, "no-store", name);
    }
    const detail = JSON.parse(fixture.work.detail.body);
    assert.equal(detail.task.attempt_count, 2);
    assert.equal(detail.accounting.total, 2);
    assert.equal(detail.children.total, 1);
    assert.ok(JSON.parse(fixture.work.tasks.body).next_cursor);
    assert.ok(JSON.parse(fixture.work.attempts.body).next_cursor);
    response = fixture.work.invalid;
    await assert.rejects(work.fetchTasks({}, "legacy"), (err) => err.status === 400);
    response = fixture.queue;
    const controller = new ConversationController("console:wire", {
        fetchQueue: api.fetchQueue, fetchReplies: async () => ({ enabled: true, replies: [] }), reconcileSubmission: async () => {},
    });
    await controller.loadQueue();
    assert.deepEqual(controller.getSnapshot().exchanges.map((e) => e.state), queue.queue.map((e) => e.state));
    controller.dispose();
    const retry = new ConversationController("console:wire", {
        fetchQueue: api.fetchQueue, fetchReplies: async () => ({ enabled: true, replies: fixture.retry_replies }), reconcileSubmission: async () => {},
    });
    response = fixture.retry.failed;
    await retry.reload();
    response = fixture.retry.running;
    await retry.reload();
    await retry.loadReplies();
    assert.equal(retry.getSnapshot().live?.exchangeID, "stop", "old production receipt does not close authorized retry");
    assert.equal(retry.getSnapshot().exchanges[0].state, "running");
    response = fixture.retry.done;
    await retry.loadQueue();
    assert.equal(retry.getSnapshot().live, null);
    retry.dispose();
    const nano = reduceConversation(createConversationProjection("console:wire"), { type: "events", events: parseSSE(fixture.nano_sse) });
    assert.equal(nano.live.turn.answer, "newer", "real Go SSE nanoseconds and zone offsets survive ordering");
    const events = parseSSE(fixture.sse);
    assert.deepEqual(events, fixture.events, "real SSE frame encoder preserves event identities and payloads");
    let state = createConversationProjection("console:wire");
    for (const event of events) state = reduceConversation(state, { type: "events", events: [event] });
    assert.equal(state.live, null);
    assert.equal(state.entries.find((r) => r.id === "reply").text, "wire done");
    assert.equal(state.entries.some((r) => r.id === "notice"), false);
    assert.equal(state.delegations["#child"].step.answer, "child done");
    const stale = reduceConversation(state, { type: "events", events: events.slice(0, 3) });
    assert.equal(stale.live, null);
    assert.equal(stale.delegations["#child"].step.state, "done");
    console.log("Go owners → TS wire contracts passed: queue/capabilities/ack, HTTP errors, SSE/reducer/controller");
} finally {
    await rm(temp, { recursive: true, force: true });
}
