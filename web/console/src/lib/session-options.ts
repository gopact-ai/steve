import type { ConversationContext, Selector, SelectorOption, Workspace } from "./types";

export interface SessionOption {
    id: string;
    name: string;
    category?: string;
    type?: string;
    current?: string;
    choices: { value: string; label: string }[];
}
const optionalString = (value: unknown): string | undefined => typeof value === "string" && value !== "" ? value : undefined;

// Both HTTP projections carry explicit Type. A value "false" or a choice
// table never turns an untyped descriptor into a boolean.
export function sessionOption(option: Selector | SelectorOption): SessionOption {
    if ("ID" in option) return {
        id: option.ID, name: option.Name || option.ID, category: optionalString(option.Category),
        type: optionalString(option.Type), current: typeof option.Current === "string" ? option.Current : undefined,
        choices: (option.Choices || []).map(choice => ({ value: choice.Value, label: choice.Label || choice.Value })),
    };
    return {
        id: option.id, name: option.name || option.id, category: optionalString(option.category),
        type: optionalString(option.type), current: typeof option.current === "string" ? option.current : undefined,
        choices: (option.values || []).map((value, index) => ({ value, label: option.choices?.[index] || value })),
    };
}

export function booleanPreference(value: string | undefined): value is "true" | "false" {
    return value === "true" || value === "false";
}

// Prefix UI choice keys so opaque Agent values cannot collide with Unfixed.
export const optionChoiceKey = (value: string) => `value:${value}`;
export const optionDefaultKey = "default";
export function optionChoiceValue(key: string): string | undefined {
    return key.startsWith("value:") ? key.slice("value:".length) : undefined;
}
export function reportedOption(option: SessionOption): string | undefined {
    if (option.type === "boolean") return booleanPreference(option.current) ? option.current : undefined;
    return option.current;
}
export function withOptionPreference(options: Record<string, string> | undefined, id: string, value: string | undefined): Record<string, string> {
    const next = { ...options };
    if (value === undefined) { delete next[id]; return next; }
    return { ...next, [id]: value };
}
export function ownPreference(options: Record<string, string> | undefined, id: string): string | undefined {
    return options && Object.hasOwn(options, id) && typeof options[id] === "string" ? options[id] : undefined;
}
export function sessionOptionRole(option: SelectorOption): "model" | "approval" | "reasoning" | undefined {
    if (option.Type !== "select") return undefined;
    // An explicit unknown category stays generic, regardless of its name.
    const role = option.Category || option.ID;
    if (role === "model") return "model";
    if (role === "mode") return "approval";
    if (role === "thought_level") return "reasoning";
    return undefined;
}
// Client context identity, not a server-issued CAS or a fabricated source
// revision. Only fields actually supplied by /context enter the scope.
export function sessionPreferenceScope(conversation: string, context: ConversationContext | null, workspace?: Workspace): string {
    const agent = context?.agent, project = context?.project, place = agent?.place;
    return JSON.stringify([conversation, agent?.id, agent?.node, agent?.harness,
        place?.node, place?.workspace, place?.kind,
        project?.id, project?.node, project?.path, project?.repo, project?.version, project?.bound,
        workspace?.id, workspace?.node, workspace?.path, workspace?.kind, workspace?.state, workspace?.origin, workspace?.source]);
}
