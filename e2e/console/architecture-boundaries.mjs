import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import ts from "../../web/console/node_modules/typescript/lib/typescript.js";

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
