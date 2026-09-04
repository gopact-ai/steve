import type { KeyboardEvent, RefObject } from "react";
import { ArrowUp, ChevronDown, Folder, Plus, Square } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import type { ConversationContext, Project, Suggestion, Verb } from "@/lib/types";
import { placeLabel } from "@/lib/workspaces";

// Composer is the console's input, in the proportions of a chat app's:
// a textarea that grows, a row of small round controls under it — the
// verbs, the project, the agent — and a send / stop button. The
// completion popover sits above it. It holds no state of its own.
export interface ComposerProps {
    value: string;
    onChange: (s: string) => void;
    onSubmit: () => void;
    onStop: () => void;
    busy: boolean;
    boxRef: RefObject<HTMLTextAreaElement | null>;
    onKey: (e: KeyboardEvent<HTMLTextAreaElement>) => void;
    suggestions: Suggestion[];
    pick: number;
    onApply: (s: Suggestion) => void;
    verbs: Verb[];
    onVerb: (command: string) => void;
    projects: Project[];
    project?: ConversationContext["project"] | null;
    onProject: (id: string) => void;
    agents: ConversationContext["agents"];
    agent?: ConversationContext["agent"] | null;
    onAgent: (id: string) => void;
}

const chip = "flex h-7 items-center gap-1.5 rounded-full px-2 text-xs outline-none transition hover:bg-secondary";

export function Composer(p: ComposerProps) {
    return (
        <div className="relative mx-auto max-w-3xl">
            {p.suggestions.length > 0 && (
                <div className="absolute bottom-full left-0 z-10 mb-2 w-full max-w-2xl overflow-hidden rounded-xl bg-primary shadow-lg ring-1 ring-secondary">
                    <ul className="max-h-72 overflow-y-auto py-1">
                        {p.suggestions.map((sg, i) => (
                            <li key={sg.insert + i}>
                                <button type="button" onMouseDown={(e) => { e.preventDefault(); p.onApply(sg); }}
                                    className={`flex w-full items-baseline gap-3 px-3 py-1.5 text-left text-sm ${i === p.pick ? "bg-secondary" : "hover:bg-secondary"} ${sg.muted ? "opacity-60" : ""}`}>
                                    <span className="shrink-0 font-mono text-xs text-primary">{sg.label}</span>
                                    {sg.args && <span className="shrink-0 font-mono text-xs text-quaternary">{sg.args}</span>}
                                    <span className="truncate text-xs text-tertiary">{sg.detail}</span>
                                </button>
                            </li>
                        ))}
                    </ul>
                    <div className="border-t border-secondary px-3 py-1 text-[11px] text-quaternary">↑↓ 选择 · Tab 填入 · Enter 发送 · Esc 收起</div>
                </div>
            )}
            <div className="flex flex-col rounded-2xl bg-primary shadow-xs ring-1 ring-secondary transition focus-within:ring-brand">
                <textarea
                    ref={p.boxRef}
                    aria-label="Message"
                    value={p.value}
                    rows={1}
                    placeholder={p.busy ? "正在进行…" : "想做什么"}
                    onChange={(e) => p.onChange(e.target.value)}
                    onKeyDown={p.onKey}
                    className="max-h-[200px] w-full resize-none bg-transparent px-4 pb-1 pt-3 text-sm text-primary outline-none placeholder:text-placeholder"
                />
                <div className="flex items-center gap-0.5 px-2 pb-2">
                    <Dropdown.Root>
                        <AriaButton aria-label="动词" className="flex size-7 items-center justify-center rounded-full text-fg-quaternary outline-none transition hover:bg-secondary hover:text-fg-quaternary_hover">
                            <Plus className="size-4" />
                        </AriaButton>
                        <Dropdown.Popover placement="top start" className="w-80">
                            <Dropdown.Menu onAction={(k) => p.onVerb(String(k))}>
                                <Dropdown.Section>
                                    <Dropdown.SectionHeader className="px-2 py-1 text-[11px] text-quaternary">动词 · 选一个填进输入框</Dropdown.SectionHeader>
                                    {p.verbs.map((v) => (
                                        <Dropdown.Item key={v.command} id={v.command} textValue={v.command}>
                                            <div className="flex min-w-0 flex-col">
                                                <span className="font-mono text-xs text-primary">{v.command} <span className="text-quaternary">{v.args || ""}</span></span>
                                                <span className="truncate text-xs text-tertiary">{v.summary}</span>
                                            </div>
                                        </Dropdown.Item>
                                    ))}
                                </Dropdown.Section>
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    <Dropdown.Root>
                        <AriaButton aria-label="项目" className={`${chip} text-tertiary hover:text-secondary`}>
                            <Folder className="size-3.5" />
                            <span>{p.project?.id || "项目"}</span>
                            {p.project && (p.agent?.place ? <span className="text-quaternary">{placeLabel(p.agent.place)}</span> : p.agent ? <span className="text-error-primary">{p.agent.node} 上没有工作区</span> : <span className="text-quaternary">{p.project.node}</span>)}
                            <ChevronDown className="size-3 text-fg-quaternary" />
                        </AriaButton>
                        <Dropdown.Popover placement="top start" className="w-80">
                            <Dropdown.Menu onAction={(k) => { if (String(k) !== p.project?.id) p.onProject(String(k)); }}>
                                {p.projects.map((x) => <Dropdown.Item key={x.id} id={x.id} textValue={x.id} label={x.id} addon={x.node} />)}
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    <span className="flex-1" />
                    <Dropdown.Root>
                        <AriaButton aria-label="Agent" className={`${chip} text-secondary`}>
                            <span>{p.agent?.id || "Agent"}</span>
                            {p.agent?.model && <span className="text-quaternary">{p.agent.model}</span>}
                            <ChevronDown className="size-3 text-fg-quaternary" />
                        </AriaButton>
                        <Dropdown.Popover placement="top end" className="w-96">
                            <Dropdown.Menu onAction={(k) => { if (String(k) !== p.agent?.id) p.onAgent(String(k)); }}>
                                {(p.agents ?? []).map((a) => (
                                    <Dropdown.Item key={a.id} id={a.id} textValue={a.id} isDisabled={!a.usable}>
                                        <div className="flex min-w-0 flex-col">
                                            <span className="text-sm text-primary">{a.id} <span className="text-xs text-quaternary">{a.node} · {a.harness}{a.model ? " · " + a.model : ""}</span></span>
                                            {!a.usable && <span className="truncate text-xs text-tertiary">{a.because || a.why}</span>}
                                        </div>
                                    </Dropdown.Item>
                                ))}
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    {p.busy ? (
                        <button type="button" aria-label="停止" title="停止（/cancel）" onClick={p.onStop}
                            className="ml-1 flex size-8 items-center justify-center rounded-full bg-secondary text-fg-secondary ring-1 ring-secondary transition hover:bg-tertiary">
                            <Square className="size-3.5" />
                        </button>
                    ) : (
                        <button type="button" aria-label="发送" title="发送（Enter）" disabled={!p.value.trim()} onClick={p.onSubmit}
                            className="ml-1 flex size-8 items-center justify-center rounded-full bg-brand-solid text-white transition hover:bg-brand-solid_hover disabled:bg-disabled disabled:text-fg-disabled">
                            <ArrowUp className="size-4" />
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}
