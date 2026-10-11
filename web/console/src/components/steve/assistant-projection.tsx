import { Button } from "@/components/base/buttons/button";
import { useI18n } from "@/providers/locale-provider";

// Shared projection feedback and navigation. Requests and pagination state are
// supplied by the page; this component owns no protocol or data fetching.
export function AssistantFeedback({ error, loading, onRefresh }: { error: string; loading: boolean; onRefresh: () => void }) {
    const { t } = useI18n();
    if (error) return <div role="alert" className="flex min-w-0 flex-wrap items-center gap-3"><p className="min-w-0 flex-1 break-words text-sm text-error-primary">{t("assistant.errorHint")} {error}</p><Button size="sm" color="secondary" onClick={onRefresh}>{t("assistant.refresh")}</Button></div>;
    return loading ? <p role="status" className="text-sm text-tertiary">{t("assistant.loading")}</p> : null;
}
export function AssistantPager({ next, cursor, onPage, disabled }: { next: string; cursor: string; onPage: (value: string) => void; disabled?: boolean }) {
    const { t } = useI18n();
    if (!next && !cursor) return null;
    return <div className="flex flex-wrap gap-2">{cursor ? <Button size="sm" color="secondary" isDisabled={disabled} onClick={() => onPage("")}>{t("assistant.first")}</Button> : null}{next ? <Button size="sm" color="secondary" isDisabled={disabled} onClick={() => onPage(next)}>{t("assistant.next")}</Button> : null}</div>;
}
