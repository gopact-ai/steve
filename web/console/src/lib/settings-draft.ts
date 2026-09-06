import type { HarnessSetting, MCPSetting, NodeSettings } from "./api/fleet";

export interface SettingRow<T> { key: number; id: string; value: T }
export type MapFormat = "lines" | "json";
export type MCPDraft = MCPSetting & { envText: string; headersText: string; envFormat: MapFormat; headersFormat: MapFormat };
export interface SettingsDraft extends Omit<NodeSettings, "harnesses" | "mcp_servers"> {
    harnesses: SettingRow<HarnessSetting>[];
    mcp_servers: SettingRow<MCPDraft>[];
}
const rows = <T>(value: Record<string, T>): SettingRow<T>[] => Object.entries(value).sort(([a], [b]) => a.localeCompare(b)).map(([id, value], key) => ({ key, id, value }));

function lineCompatible(value: Record<string, string>, separator: string): boolean {
    return Object.entries(value).every(([key, item]) => key && key.trim() === key && !key.includes(separator) && !/[\r\n]/.test(key + item));
}

function mapText(value: Record<string, string>, format: MapFormat, separator: string): string {
    return format === "json" ? JSON.stringify(value, null, 2) : Object.entries(value).map(([key, value]) => `${key}${separator}${separator === ":" ? " " : ""}${value}`).join("\n");
}

export function settingsDraft(settings: NodeSettings): SettingsDraft {
    const servers = Object.fromEntries(Object.entries(settings.mcp_servers).map(([id, value]) => {
        const envFormat: MapFormat = lineCompatible(value.env || {}, "=") ? "lines" : "json";
        const headersFormat: MapFormat = lineCompatible(value.headers || {}, ":") ? "lines" : "json";
        return [id, { ...value, envFormat, headersFormat, envText: mapText(value.env || {}, envFormat, "="), headersText: mapText(value.headers || {}, headersFormat, ":") }];
    }));
    return { ...settings, harnesses: rows(settings.harnesses), mcp_servers: rows(servers) };
}

function parseMap(text: string, format: MapFormat, separator: string, label: string): Record<string, string> {
    if (format === "lines") return parseLines(text, separator, label);
    let value: unknown;
    try { value = JSON.parse(text); } catch { throw new Error(`${label} JSON 无效，请检查双引号、逗号和转义字符`); }
    if (!value || typeof value !== "object" || Array.isArray(value) || Object.values(value).some((item) => typeof item !== "string")) {
        throw new Error(`${label} JSON 必须是字符串名称与字符串值组成的对象`);
    }
    // JSON.parse validates the grammar, but silently replaces duplicate keys.
    // Check the original flat map as well, including entries overwritten by a
    // later duplicate that would otherwise hide an invalid value.
    const tokens = text.match(/"(?:\\[\s\S]|[^"\\])*"|[^\s]/g) || [];
    const seen = new Set<string>();
    for (let i = 1; i < tokens.length - 1; i += 4) {
        if (!tokens[i].startsWith('"') || tokens[i + 1] !== ":" || !tokens[i + 2]?.startsWith('"') || tokens[i + 3] !== (i + 3 === tokens.length - 1 ? "}" : ",")) {
            throw new Error(`${label} JSON 必须是字符串名称与字符串值组成的对象`);
        }
        const key = JSON.parse(tokens[i]) as string;
        if (seen.has(key)) throw new Error(`${label} JSON 名称重复：${key}`);
        seen.add(key);
    }
    return value as Record<string, string>;
}

export function changeMapFormat(text: string, from: MapFormat, to: MapFormat, separator: string, label: string): string {
    const value = parseMap(text, from, separator, label);
    if (to === "lines" && !lineCompatible(value, separator)) throw new Error(`${label}包含多行值或特殊名称，请继续使用 JSON 对象格式`);
    return mapText(value, to, separator);
}

function parseLines(text: string, separator: string, label: string): Record<string, string> {
    const result: Record<string, string> = {};
    for (const [index, line] of text.split("\n").entries()) {
        if (!line.trim()) continue;
        const at = line.indexOf(separator);
        const key = at < 0 ? "" : line.slice(0, at).trim();
        if (!key) throw new Error(`${label}第 ${index + 1} 行缺少名称或分隔符 ${separator}`);
        if (Object.hasOwn(result, key)) throw new Error(`${label}第 ${index + 1} 行名称重复：${key}`);
        Object.defineProperty(result, key, { value: separator === ":" ? line.slice(at + 1).replace(/^ /, "") : line.slice(at + 1), enumerable: true, configurable: true, writable: true });
    }
    return result;
}

function named<T>(items: SettingRow<T>[], label: string): Record<string, T> {
    const seen = new Set<string>();
    return Object.fromEntries(items.map(({ id, value }, index) => {
        const key = id.trim();
        if (!key) throw new Error(`${label}第 ${index + 1} 项名称不能为空`);
        if (seen.has(key)) throw new Error(`${label}名称重复：${key}`);
        seen.add(key);
        return [key, value];
    }));
}

export function parseSettings(draft: SettingsDraft): NodeSettings {
    const harnesses = named(draft.harnesses, "AI 工具");
    const servers = named(draft.mcp_servers, "MCP 服务器");
    return { ...draft, harnesses, mcp_servers: Object.fromEntries(Object.entries(servers).map(([id, value]) => {
        const { envText, headersText, envFormat, headersFormat, ...setting } = value;
        return [id, { ...setting, env: parseMap(envText, envFormat, "=", `${id} 环境变量`), headers: parseMap(headersText, headersFormat, ":", `${id} 请求头`) }];
    })) };
}
