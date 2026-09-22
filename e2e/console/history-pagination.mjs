import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "../../web/console/node_modules/typescript/lib/typescript.js";

test("history API forwards opaque cursors and cancellation without legacy sequence parameters", async () => {
    const requests = [];
    globalThis.__historyRequest = (path, options) => { requests.push({ path, options }); return Promise.resolve({ entries: [], next: "" }); };
    try {
        const source = readFileSync(new URL("../../web/console/src/lib/api/work.ts", import.meta.url), "utf8")
            .replace('import { request } from "../http";', 'const request = globalThis.__historyRequest;');
        const code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
        const api = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
        const controller = new AbortController();
        for (const cursor of ["", "eyJ2IjoxfQ", "opaque+/=&?"]) {
            const page = await api.fetchHistory(cursor, 1, controller.signal);
            const { path, options } = requests.at(-1);
            const query = new URL(path, "https://fixture.invalid").searchParams;
            assert.equal(query.has("before"), false);
            assert.equal(query.get("cursor") || "", cursor);
            assert.equal(query.get("limit"), "1");
            assert.equal(options.signal, controller.signal);
            assert.equal(page.next, "");
        }
    } finally { delete globalThis.__historyRequest; }
});
