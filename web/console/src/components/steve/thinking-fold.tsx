import { ChevronDown } from "@untitledui/icons";
import { useFollowTail } from "@/hooks/use-follow-tail";
import { Md } from "./markdown";

// ThinkingFold is the agent's one-line-per-step summary of what it was
// thinking, folded; it is a summary, not the thinking.
export function ThinkingFold({ text, open, live }: { text: string; open?: boolean; live?: boolean }) {
    const { followTail, ...scroll } = useFollowTail(text, live);
    return (
        <details open={open} className="group/think min-w-0" onToggle={followTail}>
            <summary className="flex cursor-pointer list-none items-center gap-1.5 py-0.5 text-xs text-tertiary hover:text-primary" title="AI 工具在每一步之前给出的一句话概要；它不暴露完整的思考过程。">
                <span>思考摘要</span>
                <ChevronDown className="size-3.5 shrink-0 transition group-open/think:rotate-180" />
            </summary>
            <div {...scroll} tabIndex={0} className="ml-2 max-h-60 overflow-y-auto border-l border-secondary pl-3 [overflow-anchor:none]">
                <Md size="xs" text={text} className="text-tertiary" />
            </div>
        </details>
    );
}
