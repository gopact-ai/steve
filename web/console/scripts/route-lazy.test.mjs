// Inspect the actual production chunk graph without writing tracked dist.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";
import { build } from "../node_modules/vite/dist/node/index.js";

// Use the real production config, with an isolated optimizer/output directory
// so this regression can run alongside other production builds.
const scratch = await mkdtemp(path.join(tmpdir(), "steve-route-lazy-"));
const root = fileURLToPath(new URL("..", import.meta.url));
let result;
try {
    result = await build({
        root,
        envDir: false,
        cacheDir: path.join(scratch, "cache"),
        logLevel: "error",
        build: { outDir: path.join(scratch, "dist"), write: false, reportCompressedSize: false },
    });
} finally {
    await rm(scratch, { recursive: true, force: true });
}
const chunks = new Map(result.output.filter((item) => item.type === "chunk").map((chunk) => [chunk.fileName, chunk]));
const entry = [...chunks.values()].find((chunk) => chunk.isEntry);
assert.ok(entry, "production HTML has an entry chunk");
const initial = new Set();
function visit(name) {
    if (initial.has(name)) return;
    initial.add(name);
    for (const imported of chunks.get(name)?.imports ?? []) visit(imported);
}
visit(entry.fileName);
const modules = [...initial].flatMap((name) => Object.keys(chunks.get(name)?.modules ?? {}));
const bytes = [...initial].reduce((sum, name) => sum + Buffer.byteLength(chunks.get(name)?.code ?? ""), 0);
const gzipBytes = [...initial].reduce((sum, name) => sum + gzipSync(chunks.get(name).code, { level: 6 }).length, 0);
console.log(JSON.stringify({ initialJSBytes: bytes, initialJSGzipBytes: gzipBytes, initialChunks: initial.size }));
const reachable = new Set();
function visitReachable(name) {
    if (reachable.has(name)) return;
    reachable.add(name);
    const chunk = chunks.get(name);
    for (const imported of [...(chunk?.imports ?? []), ...(chunk?.dynamicImports ?? [])]) visitReachable(imported);
}
visitReachable(entry.fileName);
const reachableModules = [...reachable].flatMap((name) => Object.keys(chunks.get(name)?.modules ?? {}));
const failures = [];
function demandLoaded(suffix) {
    if (modules.some((id) => id.endsWith(suffix))) failures.push(`${suffix} must not be evaluated on Console first paint`);
    if (!reachableModules.some((id) => id.endsWith(suffix))) failures.push(`${suffix} must remain reachable on demand`);
}
for (const page of ["dashboard", "fleet", "settings", "skills", "mcp", "plugins", "projects", "home", "inbox", "board"]) demandLoaded(`/src/pages/${page}.tsx`);
for (const component of ["desktop-setup-dialog", "ssh-connect", "node-agent-enrollment", "native-session-import"]) demandLoaded(`/src/components/steve/${component}.tsx`);
for (const component of ["application/table/table", "base/select/select", "base/select/combobox"]) demandLoaded(`/src/components/${component}.tsx`);
for (const dependency of ["recharts", "mermaid", "highlight.js"]) {
    if (modules.some((id) => id.includes(`/node_modules/${dependency}/`))) failures.push(`${dependency} must be demand-loaded`);
    if (!reachableModules.some((id) => id.includes(`/node_modules/${dependency}/`))) failures.push(`${dependency} must remain reachable`);
}
// Messages are identified by their text, not by file layout: the first
// paint carries neither language, and each language loads on its own.
const holding = (text) => [...reachable].filter((name) => chunks.get(name)?.code.includes(text));
for (const [locale, text] of [["en", "Devices online: {online}/{total}"], ["zh", "{online}/{total} 台设备在线"]]) {
    const found = holding(text);
    if (found.length !== 1) failures.push(`${locale} messages must live in exactly one chunk, found ${found.length}`);
    else if (initial.has(found[0])) failures.push(`${locale} messages must be demand-loaded, not part of the first paint`);
}
if (holding("Devices online: {online}/{total}").some((name) => holding("{online}/{total} 台设备在线").includes(name))) failures.push("each language must load without the other");
assert.ok(modules.some((id) => id.endsWith("/src/pages/console.tsx")), "the default Console is immediately available");
assert.ok(modules.some((id) => id.endsWith("/src/components/steve/desktop-onboarding.tsx")), "the lightweight desktop availability gate remains eager");
if (bytes > 1_550_000) failures.push(`initial JS aggregate ${bytes} exceeds 1,550,000 bytes`);
if (gzipBytes > 480 * 1024) failures.push(`initial JS aggregate gzip ${gzipBytes} exceeds 480 KiB`);
assert.deepEqual(failures, [], "production demand boundaries and aggregate budgets");
console.log("Route production chunk boundary passed");
