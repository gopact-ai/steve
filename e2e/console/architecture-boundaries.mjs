import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import ts from "../../web/console/node_modules/typescript/lib/typescript.js";
import { assign, coverage, gatePlan, loadWeights } from "../../scripts/ci/console-shards.mjs";

const root = fileURLToPath(new URL("../../web/console/", import.meta.url));
const config = ts.readConfigFile(path.join(root, "tsconfig.app.json"), ts.sys.readFile);
const parsed = ts.parseJsonConfigFileContent(config.config, ts.sys, root);
const source = path.join(root, "src");
const graph = new Map();
for (const file of parsed.fileNames) {
    const imports = new Set();
    const ast = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true);
    function visit(node) {
        if ((ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) && node.moduleSpecifier && ts.isStringLiteral(node.moduleSpecifier)) {
            const target = ts.resolveModuleName(node.moduleSpecifier.text, file, parsed.options, ts.sys).resolvedModule?.resolvedFileName;
            if (target?.startsWith(source + path.sep)) imports.add(path.relative(source, target));
        }
        ts.forEachChild(node, visit);
    }
    visit(ast);
    graph.set(path.relative(source, file), imports);
}

test("frontend modules form a directed acyclic dependency graph", () => {
    const complete = new Set(), active = new Set(), route = [];
    function visit(file) {
        assert.ok(!active.has(file), `Dependency cycle: ${[...route, file].join(" → ")}`);
        if (complete.has(file)) return;
        active.add(file); route.push(file);
        for (const dependency of graph.get(file) || []) visit(dependency);
        route.pop(); active.delete(file); complete.add(file);
    }
    for (const file of graph.keys()) visit(file);
});

test("data and shared components never import page composition", () => {
    for (const [file, imports] of graph) {
        if (!/^(lib|hooks|providers|components)\//.test(file)) continue;
        for (const dependency of imports) {
            assert.ok(!dependency.startsWith("pages/") && dependency !== "app.tsx", `${file} depends on composition root ${dependency}`);
            if (file.startsWith("lib/")) assert.ok(!dependency.startsWith("components/"), `${file} depends on presentation ${dependency}`);
        }
    }
});

test("the standard console gate reaches every registered regression suite", () => {
    const { scripts } = JSON.parse(readFileSync(path.join(root, "package.json"), "utf8"));
    const makefile = readFileSync(path.join(root, "../../Makefile"), "utf8");
    const target = makefile.match(/^test-console:\s*\n((?:\t[^\n]*\n)+)/m);
    assert.ok(target, "make test-console must remain the standard frontend gate");
    const reached = new Set();
    function visit(command) {
        for (const match of command.matchAll(/\bnpm\s+(?:--prefix\s+\S+\s+)?run\s+([\w:-]+)/g)) {
            const name = match[1];
            assert.ok(Object.hasOwn(scripts, name), `gate invokes missing npm script ${name}`);
            if (reached.has(name)) continue;
            reached.add(name);
            visit(scripts[name]);
        }
    }
    visit(target[1]);
    const missing = Object.keys(scripts).filter((name) => name.startsWith("test:") && !reached.has(name));
    assert.deepEqual(missing, [], "registered regressions must not silently sit outside make test-console");
});

function workflowJobs() {
    const workflow = readFileSync(path.join(root, "../../.github/workflows/test.yml"), "utf8");
    const jobs = new Map();
    for (const block of workflow.split(/^(?=  [\w-]+:\s*$)/m).slice(1)) jobs.set(block.match(/^  ([\w-]+):/)[1], block);
    return jobs;
}

// A required check that only aggregates other jobs must run even when they
// fail: a job skipped because a need failed is reported to branch protection
// as Success. So it runs `if: always()` and fails unless each need succeeded.
function assertAggregate(jobs, name, required) {
    const job = jobs.get(name);
    assert.ok(job, `the required ${name} check must remain a job named ${name}`);
    const needs = job.match(/^    needs:\s*\[([^\]]*)\]\s*$/m)?.[1].split(",").map((s) => s.trim()).filter(Boolean) ?? [];
    for (const need of required) assert.ok(needs.includes(need), `${name} must wait for ${need} so the required check reflects it`);
    assert.match(job, /^    if: always\(\)\s*$/m, `${name} must run with if: always(); skipped after a failed need, it would report Success`);
    for (const need of needs) {
        assert.ok(jobs.has(need), `${name} needs a missing job ${need}`);
        assert.match(job, new RegExp(`^\\s+(?:run: )?test "\\$\\{\\{ needs\\.${need}\\.result \\}\\}" = success\\s*$`, "m"), `${name} must fail when ${need} does not succeed`);
    }
}

test("the Go test check fails when any shard or check fails", () => {
    const jobs = workflowJobs();
    assertAggregate(jobs, "test", ["go-shard", "go-checks"]);
    assert.match(jobs.get("test"), /shard\.py verify /, "test must check the shards' recorded package coverage");
});

test("console CI runs every command of make test-console exactly once", () => {
    const jobs = workflowJobs();
    assert.ok(jobs.has("console"), "the required console check must remain a job named console");

    // The commands come from the Makefile recipe, so a script added to the
    // gate is in this list without anyone touching CI.
    const { commands } = gatePlan();
    assert.ok(commands.length > 0, "make test-console must run console tests");
    const runs = [];
    for (const [name, job] of jobs) {
        const whole = /^\s+(?:-\s+)?run:\s*make test-console\s*$/m.test(job);
        const sharded = /console-shards\.mjs run /.test(job);
        if (!whole && !sharded) continue;
        assert.doesNotMatch(job, /\bnpm (?:--prefix \S+ )?run test:/, `${name}: CI must not maintain a second regression list`);
        assert.match(job, /uses: actions\/setup-go@[^\n]*\n\s+with:\n(?:\s+[\w-]+:[^\n]*\n)*?\s+go-version-file: go\.mod\n/, `${name}: Go wire fixtures need the repository toolchain from go.mod`);
        const gate = job.search(whole ? /run:\s*make test-console/ : /console-shards\.mjs run /);
        const dist = job.indexOf('test -z "$(git status --porcelain -- internal/readmodel/web/dist)"');
        assert.ok(dist > gate, `${name}: the embedded console must be checked against the build after the gate`);
        if (whole) {
            runs.push(commands);
            continue;
        }
        // A shard runs `plan $SHARDS <matrix.shard>`; every index of the
        // matrix must be present, so the shards are exactly the planned bins.
        const shards = Number(job.match(/^\s+SHARDS:\s*(\d+)\s*$/m)?.[1]);
        const matrix = job.match(/^\s+shard:\s*\[([\d,\s]+)\]\s*$/m)?.[1].split(",").map(Number);
        assert.ok(shards > 0 && matrix, `${name}: a sharded console job declares SHARDS and its shard matrix`);
        assert.match(job, /console-shards\.mjs plan "\$SHARDS" "\$\{\{ matrix\.shard \}\}"/, `${name}: each shard runs its planned share`);
        assert.deepEqual([...matrix].sort((a, b) => a - b), [...Array(shards).keys()], `${name}: the matrix must run every one of the ${shards} shards`);
        runs.push(...assign(commands, loadWeights(), shards).map((bin) => bin.commands));
        if (name !== "console") {
            assertAggregate(jobs, "console", [name]);
            assert.match(jobs.get("console"), /console-shards\.mjs verify /, "console must check the shards' recorded coverage");
        }
    }
    assert.ok(runs.length > 0, "CI must run make test-console, whole or sharded");
    assert.deepEqual(coverage(commands, runs), [], "every console test command runs in CI exactly once");
});

test("force-stop presentation receives its request callback from page composition", () => {
    const file = "components/steve/force-stop.tsx";
    const seen = new Set();
    function visit(current) {
        if (seen.has(current)) return;
        seen.add(current);
        assert.ok(!current.startsWith("lib/api/") && current !== "lib/http.ts" && current !== "lib/fleet.tsx", `${file} requests data through ${current}`);
        for (const dependency of graph.get(current) || []) visit(dependency);
    }
    // Shared visual controls have their own boundaries; only the force-stop
    // control's data dependencies and hooks belong to this contract.
    for (const dependency of graph.get(file) || []) {
        if (!dependency.startsWith("components/") && !dependency.startsWith("providers/")) visit(dependency);
    }
});
