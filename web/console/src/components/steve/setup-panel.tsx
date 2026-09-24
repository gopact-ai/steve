import { Button } from "@/components/base/buttons/button";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { AlertCircle, ChevronDown, Loading01 } from "@untitledui/icons";
import { useI18n } from "@/providers/locale-provider";
import { bytes as fmtBytes } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { useNodeLabel } from "@/lib/node-name";
import { fetchSetup } from "@/lib/api/console";
import { fetchMCP } from "@/lib/api/mcp";
import type { InstructionSection, MCPTool, MCPView, SessionSetup } from "@/lib/types";
import { CodeBlock } from "./code-block";
import { Md } from "./markdown";
import { MCPToolList } from "./mcp-tools";
import { Chips, Panel } from "./page";
import { Mono } from "./ui";

// The setup is read from disk on demand, so keep the last answer for a
// conversation: switching rail tabs should not reassemble it.
const cache = new Map<string, SessionSetup>();

// SetupPanel answers "what is this agent working with" — the question a
// turn-by-turn trace never answers. It shows the instructions the
// session opened with (as markdown, with the source a click away), what
// those instructions are made of piece by piece, the MCP servers joined
// to it and their tools, and the commands its machine reported.
export function SetupPanel({ conversation, agent, node }: { conversation: string; agent?: string; node?: string }) {
    const { t } = useI18n();
    const key = `${conversation}\u0000${agent ?? ""}`;
    const [setup, setSetup] = useState<SessionSetup | null>(() => cache.get(key) ?? null);
    const [error, setError] = useState("");
    const [attempt, setAttempt] = useState(0);
    // The last answer is shown at once and refreshed behind it: skills and
    // memory change under a conversation, so a cached panel that never
    // asks again would quietly go stale.
    useEffect(() => {
        const held = cache.get(key) ?? null;
        setSetup(held);
        const stop = new AbortController();
        fetchSetup(conversation, agent, stop.signal)
            .then((data) => { if (data.setup) { cache.set(key, data.setup); setSetup(data.setup); } setError(""); })
            .catch((e: unknown) => { if (!stop.signal.aborted) setError(e instanceof Error ? e.message : String(e)); });
        return () => stop.abort();
    }, [conversation, agent, key, attempt]);
    return (
        <Panel variant="section" title={t("setup.title")} description={t("setup.hint")}
            badge={setup ? <span className={`text-xs ${setup.applied ? "text-success-primary" : "text-tertiary"}`} title={t("setup.appliedHint")}>{setup.applied ? t("setup.applied") : t("setup.pending")}</span> : undefined}>
            {error && !setup ? (
                <div className="flex min-w-0 flex-col items-start gap-2 text-xs">
                    <span className="flex min-w-0 items-start gap-1.5 text-error-primary"><AlertCircle aria-hidden="true" className="mt-px size-3.5 shrink-0" /><span className="min-w-0 break-words [overflow-wrap:anywhere]">{t("setup.failed", { error })}</span></span>
                    <Button size="xs" color="secondary" onClick={() => setAttempt((n) => n + 1)}>{t("setup.retry")}</Button>
                </div>
            ) : !setup ? (
                <span className="flex items-center gap-1.5 text-xs text-tertiary"><Loading01 aria-hidden="true" className="size-3 animate-spin motion-reduce:animate-none" />{t("setup.loading")}</span>
            ) : (
                <div className="flex min-w-0 flex-col gap-3">
                    {error && <span className="text-xs text-error-primary">{t("setup.failed", { error })}</span>}
                    <Instructions text={setup.instructions || ""} sections={setup.sections || []} />
                    <MCPBlock servers={setup.mcp_servers || []} agent={setup.agent} node={node} />
                    <MachineSkills node={node} harness={setup.harness} />
                    <Commands node={node} />
                </div>
            )}
        </Panel>
    );
}

// Instructions is the assembled text: read as markdown by default,
// because that is how the agent reads it, with the raw source one click
// away for anyone checking exactly what was sent.
export function Instructions({ text, sections, title }: { text: string; sections: InstructionSection[]; title?: string }) {
    const { t, locale } = useI18n();
    // Sections are counted in UTF-8 bytes, the way the file is written;
    // a string's length in the browser counts code units, so Chinese
    // text would otherwise read as a third of the sum of its pieces.
    const size = useMemo(() => new TextEncoder().encode(text).length, [text]);
    if (!text) return <span className="text-xs text-tertiary">{t("setup.instructionsEmpty")}</span>;
    return (
        <div className="flex min-w-0 flex-col gap-2">
            {sections.length > 0 && <Composition sections={sections} />}
            <Fold summary={`${title ?? t("setup.instructions")} · ${fmtBytes(size, locale)}`}>
                <Prose text={text} label={title ?? t("setup.instructions")} lang="markdown" />
            </Fold>
        </div>
    );
}

// Composition names the pieces in the order they were written, with the
// weight of each: it is the difference between "the agent has a long
// prompt" and "a skill folded 12 KB in behind it".
export function Composition({ sections }: { sections: InstructionSection[] }) {
    const { t, locale } = useI18n();
    const word = (kind: string) => kind === "identity" ? t("setup.kind.identity") : kind === "language" ? t("setup.kind.language")
        : kind === "prompt" ? t("setup.kind.prompt") : kind === "skill" ? t("setup.kind.skill")
            : kind === "memory" ? t("setup.kind.memory") : kind === "extra" ? t("setup.kind.extra") : kind;
    return (
        <div className="flex min-w-0 flex-col gap-1">
            <div className="u-label">{t("setup.composition")}</div>
            <ul className="flex min-w-0 flex-col divide-y divide-secondary rounded-md ring-1 ring-secondary">
                {sections.map((s, i) => (
                    <li key={`${s.kind}:${s.name ?? ""}:${i}`} className="flex min-w-0 items-baseline gap-2 px-2 py-1.5 text-xs" title={s.path || undefined}>
                        <span className="shrink-0 text-tertiary">{word(s.kind)}</span>
                        <span className="min-w-0 flex-1 truncate text-primary">{s.name || ""}</span>
                        <span className="shrink-0 tabular-nums text-quaternary">{fmtBytes(s.bytes, locale)}</span>
                    </li>
                ))}
            </ul>
        </div>
    );
}

// Prose shows a piece of text the way it was meant to be read, with the
// source behind a switch. Markdown that is only ever printed as code is
// how a prompt ends up unreadable in a 360px column.
export function Prose({ text, label, lang }: { text: string; label: string; lang?: string }) {
    const { t } = useI18n();
    const [raw, setRaw] = useState(false);
    return (
        <div className="flex min-w-0 flex-col gap-1.5">
            <div className="flex justify-end">
                <div className="inline-flex rounded-md p-0.5 ring-1 ring-secondary" role="group">
                    {[{ id: "md", on: !raw, word: t("setup.rendered") }, { id: "raw", on: raw, word: t("setup.raw") }].map((o) => (
                        <button key={o.id} type="button" aria-pressed={o.on} onClick={() => setRaw(o.id === "raw")}
                            className={`rounded px-1.5 py-0.5 text-xs ${o.on ? "bg-secondary text-primary" : "text-tertiary hover:text-primary"}`}>{o.word}</button>
                    ))}
                </div>
            </div>
            {raw ? <CodeBlock code={text} lang={lang} label={label} maxHeight={384} />
                : <div className="max-h-96 min-w-0 overflow-auto rounded-md px-2 py-1.5 ring-1 ring-secondary"><Md size="xs" text={text} className="md-compact" /></div>}
        </div>
    );
}

// MCPBlock is the servers this agent is joined to and what each one can
// do. The tool lists come from the MCP page's own view, fetched when
// someone opens this — probing machines is too slow to do on a poll.
function MCPBlock({ servers, agent, node }: { servers: string[]; agent: string; node?: string }) {
    const { t } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const [view, setView] = useState<MCPView | null>(null);
    const [asked, setAsked] = useState(false);
    const [done, setDone] = useState(false);
    // Reading the MCP view asks every machine what it has configured, so
    // it waits for someone to open a server rather than loading with the panel.
    const load = useCallback(() => {
        if (asked) return;
        setAsked(true);
        fetchMCP().then(setView).catch(() => setView(null)).finally(() => setDone(true));
    }, [asked]);
    if (!servers.length) return null;
    const toolsOf = (name: string): { tools?: MCPTool[]; missing?: boolean } => {
        if (!view) return {};
        const platform = view.platform?.find((p) => p.name === name);
        if (platform) return { tools: platform.tools.map((tool) => ({ name: tool.name, description: tool.description })) };
        const mine = view.deployments?.filter((d) => d.name === name && d.agents?.includes(agent)) ?? [];
        const here = mine.find((d) => d.node === node) ?? mine[0];
        if (!here) return { missing: true };
        return { tools: here.probe?.tools };
    };
    return (
        <div className="flex min-w-0 flex-col gap-1">
            <div className="u-label">{t("setup.mcp")}</div>
            <ul className="flex min-w-0 flex-col gap-1">
                {servers.map((name) => {
                    const { tools, missing } = toolsOf(name);
                    return (
                        <li key={name}>
                            <details className="group/mcp min-w-0 rounded-md ring-1 ring-secondary" onToggle={load}>
                                <summary className="flex min-w-0 cursor-pointer list-none items-center gap-2 px-2 py-1.5 text-xs hover:bg-primary_hover [&::-webkit-details-marker]:hidden">
                                    <Mono className="min-w-0 flex-1 truncate text-primary">{name}</Mono>
                                    {tools?.length ? <span className="shrink-0 text-quaternary">{t("setup.mcpTools", { count: tools.length })}</span> : null}
                                    <ChevronDown aria-hidden="true" className="size-3.5 shrink-0 text-fg-quaternary group-open/mcp:rotate-180" />
                                </summary>
                                <div className="min-w-0 px-2 pb-2">
                                    {!done ? <span className="text-xs text-tertiary">{t("setup.loading")}</span>
                                        : missing ? <span className="text-xs text-tertiary">{t("setup.mcpMissing", { node: node ? nodeLabelOf(node) : "—" })}</span>
                                            : tools?.length ? <MCPToolList tools={tools} dense />
                                                : <span className="text-xs text-tertiary">{t("setup.mcpUnprobed")}</span>}
                                </div>
                            </details>
                        </li>
                    );
                })}
            </ul>
        </div>
    );
}

// MachineSkills is what the harness can load on that machine by itself.
// They never appear in the assembled instructions, so listing only the
// folded-in ones would say an agent has no skills when it has several.
function MachineSkills({ node, harness }: { node?: string; harness: string }) {
    const { t } = useI18n();
    const snap = useFleet((fleet) => fleet.snap);
    const skills = useMemo(() => {
        const machine = snap.nodes.find((n) => n.name === node);
        const offers = (machine?.snapshot?.offers || []).filter((c) => c.kind === "skill" && (!c.scope || c.scope === harness));
        return [...new Set(offers.map((c) => c.id))].sort();
    }, [snap.nodes, node, harness]);
    return (
        <div className="flex min-w-0 flex-col gap-1">
            <div className="u-label" title={t("setup.skillsHint")}>{t("setup.skills")}</div>
            <Chips tone="muted" items={skills.map((id) => ({ id }))}
                empty={<span className="text-xs text-tertiary">{t("setup.skillsEmpty", { harness })}</span>} />
        </div>
    );
}

// Commands is what the agent's machine reported it can run. The
// harness's own built-in tools are not ours to enumerate, and the hint
// says so rather than leaving the list looking complete.
function Commands({ node }: { node?: string }) {
    const { t } = useI18n();
    const snap = useFleet((fleet) => fleet.snap);
    const tools = useMemo(() => {
        const machine = snap.nodes.find((n) => n.name === node);
        return (machine?.snapshot?.offers || []).filter((c) => c.kind === "tool");
    }, [snap.nodes, node]);
    return (
        <div className="flex min-w-0 flex-col gap-1">
            <div className="u-label" title={t("setup.commandsHint")}>{t("setup.commands")}</div>
            <Chips tone="muted" items={tools.map((c) => ({ id: c.id, title: c.version?.value ? `${c.id} · ${c.version.value.slice(0, 16)}` : c.id }))}
                empty={<span className="text-xs text-tertiary">{t("setup.commandsEmpty")}</span>} />
            <p className="text-xs text-quaternary">{t("setup.builtinHint")}</p>
        </div>
    );
}

// Fold is a closed section with a one-line summary, the rail's way of
// holding something long without hiding that it is there.
export function Fold({ summary, children }: { summary: string; children: ReactNode }) {
    return (
        <details className="group/fold min-w-0 text-xs">
            <summary className="flex min-w-0 cursor-pointer list-none items-center gap-1.5 text-tertiary hover:text-primary [&::-webkit-details-marker]:hidden">
                <ChevronDown aria-hidden="true" className="size-3.5 shrink-0 group-open/fold:rotate-180" />
                <span className="min-w-0 truncate">{summary}</span>
            </summary>
            <div className="mt-1.5 min-w-0">{children}</div>
        </details>
    );
}
