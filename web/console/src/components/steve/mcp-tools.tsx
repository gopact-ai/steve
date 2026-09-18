import { useI18n } from "@/providers/locale-provider";
import { ChevronDown } from "@untitledui/icons";
import { CodeBlock } from "@/components/steve/code-block";
import type { MCPTool } from "@/lib/types";

// dense is for a narrow column, where the page's two-column row would
// squeeze the description to nothing: the viewport is wide, the
// container is not, and a media query cannot tell the difference. Name
// and summary stack instead.
export function MCPToolList({ tools, summaries, dense }: { tools: MCPTool[]; summaries?: Record<string, string>; dense?: boolean }) {
    const { t: tr } = useI18n();
    return (
        <ul className="min-w-0 divide-y divide-secondary border-y border-secondary">
            {tools.map((tool) => (
                <li key={tool.name}>
                    <details className="group/mcp-tool min-w-0">
                        <summary className={`flex cursor-pointer list-none rounded-md hover:bg-primary_hover [&::-webkit-details-marker]:hidden ${dense ? "items-start gap-2 px-2 py-2" : "min-h-12 items-center gap-3 px-3 py-3"}`}>
                            <span className={dense ? "flex min-w-0 flex-1 flex-col gap-0.5" : "grid min-w-0 flex-1 gap-x-6 gap-y-1 sm:grid-cols-[14rem_minmax(0,1fr)] sm:items-baseline"}>
                                <code translate="no" className={`font-mono font-medium break-all text-primary ${dense ? "text-xs" : "text-sm"}`}>{tool.name}</code>
                                <span className={`line-clamp-2 break-words text-secondary [overflow-wrap:anywhere] ${dense ? "text-xs leading-5" : "text-sm leading-6"}`}>{summaries?.[tool.name] || tool.description || tr("mcpTools.noSummary")}</span>
                            </span>
                            <ChevronDown aria-hidden="true" className={`shrink-0 text-fg-tertiary group-open/mcp-tool:rotate-180 ${dense ? "mt-0.5 size-3.5" : "size-4"}`} />
                        </summary>
                        <div className={`flex min-w-0 flex-col ${dense ? "gap-2 px-2 pb-3" : "gap-3 px-3 pt-1 pb-4"}`}>
                            <p className="text-xs font-medium text-tertiary">{tr("mcpTools.fullDescription")}</p>
                            <p className={`max-w-3xl whitespace-pre-wrap break-words text-secondary [overflow-wrap:anywhere] ${dense ? "text-xs leading-5" : "text-sm leading-6"}`}>{tool.description || tr("mcpTools.noDescription")}</p>
                            {tool.input_schema ? <CodeBlock code={JSON.stringify(tool.input_schema, null, 2)} lang="json" label={tr("mcpTools.inputParameters")} maxHeight={240} /> : null}
                        </div>
                    </details>
                </li>
            ))}
        </ul>
    );
}
