import { ChevronDown } from "@untitledui/icons";
import { CodeBlock } from "@/components/steve/markdown";
import type { MCPTool } from "@/lib/types";

export function MCPToolList({ tools, summaries }: { tools: MCPTool[]; summaries?: Record<string, string> }) {
    return (
        <ul className="min-w-0 divide-y divide-secondary border-y border-secondary">
            {tools.map((tool) => (
                <li key={tool.name}>
                    <details className="group/mcp-tool min-w-0">
                        <summary className="flex min-h-12 cursor-pointer list-none items-center gap-3 rounded-md px-3 py-3 hover:bg-primary_hover [&::-webkit-details-marker]:hidden">
                            <span className="grid min-w-0 flex-1 gap-x-6 gap-y-1 sm:grid-cols-[14rem_minmax(0,1fr)] sm:items-baseline">
                                <code translate="no" className="font-mono text-sm font-medium break-all text-primary">{tool.name}</code>
                                <span className="line-clamp-2 text-sm leading-6 break-words text-secondary [overflow-wrap:anywhere]">{summaries?.[tool.name] || tool.description || "暂无说明"}</span>
                            </span>
                            <ChevronDown aria-hidden="true" className="size-4 shrink-0 text-fg-tertiary group-open/mcp-tool:rotate-180" />
                        </summary>
                        <div className="flex min-w-0 flex-col gap-3 px-3 pt-1 pb-4">
                            <p className="text-xs font-medium text-tertiary">完整说明</p>
                            <p className="max-w-3xl text-sm leading-6 whitespace-pre-wrap break-words text-secondary [overflow-wrap:anywhere]">{tool.description || "此工具未提供说明。"}</p>
                            {tool.input_schema ? <CodeBlock code={JSON.stringify(tool.input_schema, null, 2)} lang="json" label="输入参数" maxHeight={240} /> : null}
                        </div>
                    </details>
                </li>
            ))}
        </ul>
    );
}
