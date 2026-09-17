import type { Agent, Condition } from "./types";
import type { Translator } from "./i18n";

// An agent that cannot run says so in the matcher's notation: "lacks
// tag:BOE (ABSENT)". That is precise and unreadable. These helpers turn
// the same facts into a sentence: which machine, what is missing, and in
// what sense it is missing.
export function atomWords(atom: string, tr: Translator): string {
    const negated = atom.startsWith("!");
    const body = negated ? atom.slice(1) : atom;
    const cut = body.indexOf(":");
    const kind = cut < 0 ? "tag" : body.slice(0, cut);
    const id = cut < 0 ? body : body.slice(cut + 1);
    const words = kindWords(kind, id, tr);
    return negated ? tr("fleet.atomNot", { what: words }) : words;
}

function kindWords(kind: string, id: string, tr: Translator): string {
    switch (kind) {
        case "tag": return tr("fleet.atomTag", { id });
        case "tool": return tr("fleet.atomTool", { id });
        case "harness": return tr("fleet.atomHarness", { id });
        case "mcp": return tr("fleet.atomMcp", { id });
        case "skill": return tr("fleet.atomSkill", { id });
        case "model": return tr("fleet.atomModel", { id });
        case "hardware": return tr("fleet.atomHardware", { id });
        case "network": return tr("fleet.atomNetwork", { id });
        case "credential": return tr("fleet.atomCredential", { id });
        case "a2a": return tr("fleet.atomA2a", { id });
        default: return tr("fleet.atomOther", { kind, id });
    }
}

function codeWords(code: string, tr: Translator): string {
    switch (code) {
        case "ABSENT": return tr("fleet.codeAbsent");
        case "UNAVAILABLE": return tr("fleet.codeUnavailable");
        case "DECLARED_ONLY": return tr("fleet.codeDeclaredOnly");
        case "UNKNOWN_COVERAGE": return tr("fleet.codeUnknownCoverage");
        case "STALE": return tr("fleet.codeStale");
        case "VERSION": return tr("fleet.codeVersion");
        case "ATTR": return tr("fleet.codeAttr");
        case "SCOPE": return tr("fleet.codeScope");
        case "NOT_SCHEDULABLE": return tr("fleet.codeNotSchedulable");
        case "PRESENT": return tr("fleet.codePresent");
        default: return code;
    }
}

export function conditionWords(c: Condition, tr: Translator): string {
    const what = atomWords(c.atom, tr);
    if (c.met || !c.code) return what;
    return tr("fleet.conditionWords", { what, code: codeWords(c.code, tr) });
}

// troubleWords is the whole reason in one line, naming the machine the
// way its owner named it. A reason the server has not classified keeps
// its own sentence, so a new one is never swallowed.
export function troubleWords(a: Agent, machine: string, tr: Translator): string {
    const unmet = (a.conditions || []).filter((c) => !c.met);
    switch (a.reason) {
        case "requirements": {
            const items = unmet.length ? unmet.map((c) => conditionWords(c, tr)) : (a.requires || []).map((r) => atomWords(r, tr));
            return tr("fleet.whyRequirements", { machine, items: items.join(tr("fleet.listJoin")) });
        }
        case "node_down":
            return tr("fleet.whyDown", { machine, detail: a.reason_detail ? tr("fleet.whyDetail", { detail: a.reason_detail }) : "" });
        case "node_unknown":
            return tr("fleet.whyUnknownNode");
        case "harness_missing":
            return tr("fleet.whyHarness", { machine, tool: a.harness, detail: a.reason_detail || "" });
        case "disk_full":
            return tr("fleet.whyDisk", { machine, free: a.reason_detail || "" });
        case "model_missing":
            return tr("fleet.whyModel", { machine, model: a.reason_detail || "" });
        case "bad_requirement":
            return tr("fleet.whyBadRequirement", { detail: a.reason_detail || "" });
    }
    return a.why || "";
}

// missingTags are the labels the machine would have to declare for this
// agent to run, and only those: a label is something its owner can add
// in one click, while a missing runtime or a full disk is not.
export function missingTags(a: Agent): string[] {
    const unmet = (a.conditions || []).filter((c) => !c.met);
    if (!unmet.length || a.reason !== "requirements") return [];
    if (!unmet.every((c) => c.atom.startsWith("tag:") && c.code === "ABSENT")) return [];
    return unmet.map((c) => c.atom.slice("tag:".length)).filter(Boolean);
}
