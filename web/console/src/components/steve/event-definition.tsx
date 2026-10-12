import type { EventDefinition as Definition } from "@/lib/assistant-types";
import { eventDefinitionText } from "@/lib/event-definition";
import { useI18n } from "@/providers/locale-provider";

export function EventDefinition({ type, definition, compact = false, showName = true, showType = true }: { type: string; definition?: Definition; compact?: boolean; showName?: boolean; showType?: boolean }) {
    const { locale } = useI18n();
    const text = eventDefinitionText(definition, locale);
    if (!showType && !(showName && text.name) && !text.description) return null;
    return <div className="min-w-0" data-event-definition={type}>
        {showName && text.name && text.name !== type ? <p title={text.name} className="break-words text-xs font-medium text-secondary [overflow-wrap:anywhere]">{text.name}</p> : null}
        {showType ? <code className="break-all text-xs text-tertiary" translate="no">{type}</code> : null}
        {text.description ? <p title={text.description} className={`mt-1 whitespace-pre-wrap break-words text-xs leading-5 text-tertiary [overflow-wrap:anywhere] ${compact ? "line-clamp-2" : ""}`}>{text.description}</p> : null}
    </div>;
}
