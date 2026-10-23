#!/usr/bin/env node
// Split `make test-console` into balanced CI shards and prove nothing was dropped.
//
//   console-shards.mjs plan N I        test commands of shard I of N
//   console-shards.mjs all             every test command, in gate order
//   console-shards.mjs run FILE        run the gate's setup, then the commands in FILE
//   console-shards.mjs verify ALL F..  fail unless the files cover ALL exactly once
//   console-shards.mjs weights LOG..   print console-weights.txt from `run` logs
//
// The command list is derived from the Makefile's test-console recipe: each
// `npm --prefix web/console run test:*` line is expanded through package.json
// into the single commands its `&&` chain runs. Other recipe lines (installing
// the mail provider, building the console) are setup that every shard runs.
// Commands are placed longest-first on the least-loaded shard using the
// seconds in console-weights.txt; commands without a weight then go to the
// lightest shard, so a new script is never skipped.
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
export const repo = path.resolve(here, "../..");
const consoleDir = path.join(repo, "web/console");

export function gateRecipe(makefile = readFileSync(path.join(repo, "Makefile"), "utf8")) {
    const target = makefile.match(/^test-console:[^\n]*\n((?:\t[^\n]*\n)+)/m);
    if (!target) throw new Error("Makefile has no test-console recipe");
    return target[1].split("\n").map((line) => line.trim()).filter(Boolean);
}

// The gate split into setup lines (run by every shard, in order) and the
// single test commands (run by exactly one shard), with the npm script each
// command came from.
export function gatePlan(makefile, scripts = JSON.parse(readFileSync(path.join(consoleDir, "package.json"), "utf8")).scripts) {
    const setup = [], commands = [], origin = new Map();
    function expand(name, from) {
        if (!Object.hasOwn(scripts, name)) throw new Error(`gate invokes missing npm script ${name}`);
        for (const part of scripts[name].split("&&").map((p) => p.trim())) {
            const nested = part.match(/^npm run ([\w:-]+)$/);
            if (nested) expand(nested[1], from);
            else if (/^(?:[A-Z_]+=\S+ )*node [^&|;<>`$]+$/.test(part)) {
                if (origin.has(part)) throw new Error(`console command runs twice in the gate: ${part}`);
                origin.set(part, from);
                commands.push(part);
            } else throw new Error(`npm script ${name}: cannot shard command ${JSON.stringify(part)}`);
        }
    }
    for (const line of gateRecipe(makefile)) {
        const test = line.match(/^npm --prefix web\/console run (test:[\w:-]+)$/);
        if (test) expand(test[1], test[1]);
        else setup.push(line);
    }
    return { setup, commands, origin };
}

export function loadWeights(file = path.join(here, "console-weights.txt")) {
    const weights = new Map();
    for (const line of readFileSync(file, "utf8").split("\n")) {
        if (!line || line.startsWith("#")) continue;
        const tab = line.indexOf("\t");
        weights.set(line.slice(tab + 1), Number(line.slice(0, tab)));
    }
    return weights;
}

// Longest-processing-time first; unweighted commands then fill the lightest shard.
export function assign(commands, weights, shards) {
    const bins = Array.from({ length: shards }, () => ({ load: 0, commands: [] }));
    const known = commands.filter((c) => weights.has(c)).sort((a, b) => weights.get(b) - weights.get(a) || (a < b ? -1 : 1));
    const unknown = commands.filter((c) => !weights.has(c));
    for (const command of [...known, ...unknown]) {
        const bin = bins.reduce((best, b) => (b.load < best.load || (b.load === best.load && b.commands.length < best.commands.length) ? b : best));
        bin.commands.push(command);
        bin.load += weights.get(command) ?? 0;
    }
    return bins;
}

export function coverage(all, shards) {
    const seen = shards.flat(), problems = [];
    for (const c of all) if (!seen.includes(c)) problems.push(`missing: ${c}`);
    for (const c of new Set(seen)) {
        if (!all.includes(c)) problems.push(`unexpected: ${c}`);
        if (seen.indexOf(c) !== seen.lastIndexOf(c)) problems.push(`ran twice: ${c}`);
    }
    return problems;
}

const lines = (file) => readFileSync(file, "utf8").split("\n").filter((l) => l.trim());

function sh(command, cwd, env) {
    const result = spawnSync("sh", ["-c", command], { cwd, env, stdio: "inherit" });
    return result.status ?? 1;
}

function run(file) {
    const { setup } = gatePlan();
    for (const line of setup) {
        console.log(`$ ${line}`);
        if (sh(line, repo, process.env)) process.exit(1);
    }
    // What `npm run` puts on PATH, so each command behaves as in its script.
    const env = { ...process.env, PATH: `${path.join(consoleDir, "node_modules/.bin")}${path.delimiter}${process.env.PATH}` };
    for (const command of lines(file)) {
        console.log(`::group::${command}`);
        const start = performance.now();
        const status = sh(command, consoleDir, env);
        console.log("::endgroup::");
        console.log(`TIMING\t${((performance.now() - start) / 1000).toFixed(1)}\t${command}`);
        if (status) {
            console.error(`FAILED (exit ${status}): ${command}`);
            process.exit(1);
        }
    }
}

function main([cmd, ...args]) {
    if (cmd === "plan") {
        const bins = assign(gatePlan().commands, loadWeights(), Number(args[0]));
        bins.forEach((b, i) => console.error(`shard ${i}/${bins.length}: ${b.commands.length} commands, ${b.load.toFixed(0)}s measured`));
        console.log(bins[Number(args[1])].commands.join("\n"));
    } else if (cmd === "all") console.log(gatePlan().commands.join("\n"));
    else if (cmd === "run") run(args[0]);
    else if (cmd === "verify") {
        const all = lines(args[0]), problems = coverage(all, args.slice(1).map(lines));
        problems.forEach((p) => console.log(p));
        if (problems.length || !all.length) process.exit(1);
        console.log(`${all.length} console commands covered exactly once by ${args.length - 1} shards`);
    } else if (cmd === "weights") {
        const seconds = new Map();
        for (const file of args)
            for (const line of readFileSync(file, "utf8").split("\n")) {
                const m = line.match(/TIMING\t([\d.]+)\t(.+?)\r?$/);
                if (m) seconds.set(m[2], [...(seconds.get(m[2]) || []), Number(m[1])]);
            }
        const mean = [...seconds].map(([c, v]) => [c, v.reduce((a, b) => a + b) / v.length]).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
        for (const [c, s] of mean) console.log(`${s.toFixed(1)}\t${c}`);
    } else {
        console.error("usage: console-shards.mjs plan N I | all | run FILE | verify ALL FILE... | weights LOG...");
        process.exit(2);
    }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main(process.argv.slice(2));
