// Inspect the actual production chunk graph without writing tracked dist.
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";

const result = await build({
    root: fileURLToPath(new URL("..", import.meta.url)),
    logLevel: "error",
    build: { write: false, reportCompressedSize: false },
});
const chunks = new Map(result.output.filter((item) => item.type === "chunk").map((chunk) => [chunk.fileName, chunk]));
const entry = [...chunks.values()].find((chunk) => chunk.isEntry);
const initial = new Set();
function visit(name) {
    if (initial.has(name)) return;
    initial.add(name);
    for (const imported of chunks.get(name)?.imports ?? []) visit(imported);
}
visit(entry.fileName);
const modules = [...initial].flatMap((name) => Object.keys(chunks.get(name)?.modules ?? {}));
const bytes = [...initial].reduce((sum, name) => sum + Buffer.byteLength(chunks.get(name)?.code ?? ""), 0);
console.log(JSON.stringify({ initialJSBytes: bytes, initialChunks: initial.size }));
for (const page of ["dashboard", "fleet", "settings", "skills", "mcp"]) {
    assert.ok(!modules.some((id) => id.endsWith(`/src/pages/${page}.tsx`)), `${page} must not be evaluated on Console first paint`);
    assert.ok([...chunks.values()].some((chunk) => Object.keys(chunk.modules).some((id) => id.endsWith(`/src/pages/${page}.tsx`))), `${page} remains reachable`);
}
for (const dependency of ["recharts", "mermaid"]) {
    assert.ok(!modules.some((id) => id.includes(`/node_modules/${dependency}/`)), `${dependency} is demand-loaded`);
}
console.log("Route production chunk boundary passed");
