import type { Translator } from "./i18n";
import type { HistoryEntry } from "./types";

// A history record arrives as a machine would write it: node IDs, English
// verbs, a bundle hash. This turns one into what a person came to read —
// a subject they recognise, a verb, and the supporting facts beside it,
// in that order of importance. Every record keeps its original sentence
// in `e.text`; an unknown kind falls back to it rather than guessing.

export type HistoryFamily = "machine" | "skills" | "work" | "content" | "ledger" | "other";
export type HistoryTone = "good" | "bad" | "warn" | "info" | "quiet";

export interface HistoryLine {
    family: HistoryFamily;
    tone: HistoryTone;
    /** What happened, in two or three words. The badge. */
    label: string;
    /** One sentence: who, and what they did. */
    title: string;
    /** The supporting facts, each short enough to read at a glance. */
    facts: string[];
    /** Why it went wrong, when it did. */
    note?: string;
    /** The identifier a reader may need to search for elsewhere. */
    mono?: string;
}

/** The six families a reader filters by. */
export const historyFamilies: HistoryFamily[] = ["machine", "skills", "work", "content", "ledger", "other"];

export function familyOf(entry: HistoryEntry): HistoryFamily {
    const kind = bareKind(entry);
    if (kind === "node.up" || kind === "node.down" || kind === "node.manifest") return "machine";
    if (kind === "node.skills") return "skills";
    if (kind === "landing" || kind === "worktree.sweep" || kind === "task.idle") return "work";
    if (kind.startsWith("content.")) return "content";
    if (entry.kind === "ledger") return "ledger";
    return "other";
}

const bareKind = (e: HistoryEntry) => e.kind.startsWith("observe.") ? e.kind.slice("observe.".length) : e.kind;

// The ledger names an operation by the prefix of its ID.
function operationWord(id: string, tr: Translator): string {
    if (id.startsWith("att-")) return tr("history.opAttempt");
    if (id.startsWith("landing-") || id.startsWith("land-")) return tr("history.opLanding");
    if (id.startsWith("disc-")) return tr("history.opDisclosure");
    if (id.startsWith("intent-") || id.startsWith("eff-")) return tr("history.opEffect");
    return tr("history.opGeneric");
}

// A machine that simply went away says so as "disconnected"; anything
// else is a real error and is shown as it came.
const reasonWord = (reason: string, tr: Translator) => reason === "disconnected" || reason === "" ? tr("history.reasonClosed") : reason;

// One manifest change, encoded as "key\tfrom\tto".
function abilityWord(row: string, tr: Translator): string {
    const [key = "", from = "", to = ""] = row.split("\t");
    if (!from) return tr("history.abilityAppeared", { key, state: to });
    if (!to) return tr("history.abilityGone", { key });
    return tr("history.abilityChanged", { key, from, to });
}

const lines = (value?: string) => (value || "").split("\n").filter(Boolean);

/**
 * describeHistory renders one record. `stateWord` translates a ledger
 * state, `nodeName` a node ID; both fall back to what they were given,
 * so an unknown token is shown rather than swallowed.
 */
export function describeHistory(entry: HistoryEntry, tr: Translator, nodeName: (id: string) => string, stateWord: (state: string) => string): HistoryLine {
    const d = entry.data || {};
    const subject = entry.subject || "";
    const machine = nodeName(subject);
    switch (bareKind(entry)) {
        case "node.up": {
            const platform = [d.os, d.arch].filter(Boolean).join("/");
            return {
                family: "machine", tone: "good", label: tr("history.kindNodeUp"),
                title: tr("history.lineNodeUp", { machine }),
                facts: [d.host || "", platform, d.build ? tr("history.factBuild", { build: d.build }) : ""].filter(Boolean),
            };
        }
        case "node.down":
            return {
                family: "machine", tone: "bad", label: tr("history.kindNodeDown"),
                title: tr("history.lineNodeDown", { machine }),
                facts: [], note: reasonWord(d.reason ?? "", tr),
            };
        case "node.skills": {
            if (d.error) {
                return {
                    family: "skills", tone: "bad", label: tr("history.kindSkills"),
                    title: tr("history.lineSkillsFailed", { machine }),
                    facts: d.hash ? [tr("history.factBundle", { hash: d.hash })] : [], note: d.error,
                };
            }
            return {
                family: "skills", tone: "info", label: tr("history.kindSkills"),
                title: tr("history.lineSkills", { machine }),
                facts: [d.count ? tr("history.factSkillCount", { count: d.count }) : "", d.hash ? tr("history.factBundle", { hash: d.hash }) : ""].filter(Boolean),
            };
        }
        case "node.manifest": {
            // A record written before the facts were kept apart still has
            // its sentence; strip the node ID it opens with and show the
            // rest as the change it describes.
            const changes = lines(d.changes);
            const said = entry.text.startsWith(subject + ": ") ? entry.text.slice(subject.length + 2) : entry.text;
            return {
                family: "machine", tone: "info", label: tr("history.kindManifest"),
                title: tr("history.lineManifest", { machine, count: changes.length || said.split("; ").length }),
                facts: changes.length ? changes.map((row) => abilityWord(row, tr)) : said.split("; ").filter(Boolean),
            };
        }
        case "landing":
            return {
                family: "work", tone: "good", label: tr("history.kindLanding"),
                title: tr("history.lineLanding", { project: d.project || subject }),
                facts: [d.paths ? tr("history.factFiles", { count: d.paths }) : "", d.state ? stateWord(d.state) : ""].filter(Boolean),
                mono: d.artifact,
            };
        case "worktree.sweep": {
            const items = lines(d.items);
            return {
                family: "work", tone: "quiet", label: tr("history.kindWorktree"),
                title: tr("history.lineWorktree", { machine, count: d.count || items.length }),
                facts: items,
            };
        }
        case "task.idle":
            return {
                family: "work", tone: "quiet", label: tr("history.kindTaskIdle"),
                title: tr("history.lineTaskIdle", { task: d.task || subject }),
                facts: [d.member || "", d.idle ? tr("history.factIdle", { idle: d.idle }) : ""].filter(Boolean),
            };
        case "channel.error":
            return {
                family: "other", tone: "bad", label: tr("history.kindChannel"),
                title: tr("history.lineChannel"), facts: [], note: entry.text,
            };
        case "ledger": {
            const id = entry.operation || subject;
            const word = operationWord(id, tr);
            const actor = entry.actor || "steve";
            return {
                family: "ledger", tone: entry.to === "failed" ? "bad" : "info", label: word,
                title: entry.from
                    ? tr("history.lineMoved", { kind: word, from: stateWord(entry.from), to: stateWord(entry.to || "") })
                    : tr("history.lineOpened", { kind: word, state: stateWord(entry.to || "") }),
                facts: [tr("history.factActor", { actor })], mono: id,
            };
        }
        default: {
            // content.* already speaks the reader's language; anything
            // else is shown as the server wrote it.
            const kind = bareKind(entry);
            const bad = /unavailable|invalid|blocked|degraded/.test(kind);
            return {
                family: familyOf(entry),
                tone: kind.endsWith("repaired") || kind.endsWith("recovered") || kind.endsWith("healthy") ? "good" : bad ? "warn" : "quiet",
                label: kind.startsWith("content.") ? tr("history.kindContent") : kind,
                title: entry.text, facts: [], mono: d.object || subject,
            };
        }
    }
}
