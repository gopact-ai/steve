// Run source tests in a disposable checkout-shaped fixture. Reuse installed
// dependencies without installing packages or writing their shared caches.
import assert from "node:assert/strict";
import { cp, mkdir, mkdtemp, readdir, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const web = fileURLToPath(new URL("..", import.meta.url));
const repo = path.resolve(web, "../..");
const dependencies = path.resolve(process.env.CONSOLE_NODE_MODULES || path.join(web, "node_modules"));
const temp = await mkdtemp(path.join(tmpdir(), "console-conversations-"));
const isolatedWeb = path.join(temp, "web/console");
function run(command, args, cwd = isolatedWeb) {
    const result = spawnSync(command, args, { cwd, stdio: "inherit", env: {
        ...process.env, CONSOLE_GO_ROOT: process.env.CONSOLE_GO_ROOT || repo,
        CONSOLE_DIST: path.join(temp, "dist"), USAGE_DIST: path.join(temp, "dist"), HUB: "http://127.0.0.1:0",
    } });
    if (result.error) throw result.error;
    assert.equal(result.status, 0, `${command} ${args.join(" ")} failed`);
}
try {
    await mkdir(isolatedWeb, { recursive: true });
    for (const name of ["src", "scripts", "index.html", "package.json", "tsconfig.json", "tsconfig.app.json", "tsconfig.node.json", "vite.config.ts"]) {
        await cp(path.join(web, name), path.join(isolatedWeb, name), { recursive: true });
    }
    await cp(path.join(repo, "e2e/console"), path.join(temp, "e2e/console"), { recursive: true });
    await mkdir(path.join(temp, "e2e/childcard"), { recursive: true });
    await cp(path.join(repo, "e2e/childcard/preview.mjs"), path.join(temp, "e2e/childcard/preview.mjs"));
    await mkdir(path.join(temp, "desktop/macos/Assets"), { recursive: true });
    await cp(path.join(repo, "desktop/macos/Assets/AppIcon.png"), path.join(temp, "desktop/macos/Assets/AppIcon.png"));
    await mkdir(path.join(isolatedWeb, "node_modules"));
    for (const name of await readdir(dependencies)) {
        if (name.startsWith(".") && name !== ".bin") continue;
        await symlink(path.join(dependencies, name), path.join(isolatedWeb, "node_modules", name));
    }
    const requested = new Set(process.argv.slice(2));
    const all = !requested.size;
    if (all || requested.has("types")) {
        run(process.execPath, ["node_modules/typescript/bin/tsc", "--noEmit", "--project", "tsconfig.app.json"]);
        run(process.execPath, ["node_modules/typescript/bin/tsc", "--noEmit", "--project", "tsconfig.node.json"]);
        run(process.execPath, ["node_modules/typescript/bin/tsc", "--noEmit", "--strict", "--skipLibCheck", "--target", "ESNext", "--moduleResolution", "bundler", "--module", "ESNext", "scripts/choice-types.test.ts"]);
    }
    if (all || requested.has("unit")) {
        run("npm", ["run", "test:unit"]);
        run("npm", ["run", "test:boundaries"]);
    }
    if (all || requested.has("browser")) {
        run("npm", ["run", "test:streaming"]);
        run(process.execPath, [path.join(temp, "e2e/console/side-chat-ui.mjs")]);
        run(process.execPath, ["node_modules/vite/bin/vite.js", "build", "--outDir", path.join(temp, "dist")]);
        run("npm", ["run", "test:ui"]);
        run("npm", ["run", "test:task-completion"]);
        run("npm", ["run", "test:architecture"]);
        run(process.execPath, [path.join(temp, "e2e/console/durable-drafts.mjs")]);
    }
    if (requested.has("architecture")) {
        run(process.execPath, ["node_modules/vite/bin/vite.js", "build", "--outDir", path.join(temp, "dist")]);
        run("npm", ["run", "test:architecture"]);
    }
} finally {
    await rm(temp, { recursive: true, force: true });
}
