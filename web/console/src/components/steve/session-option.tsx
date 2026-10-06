import { Select } from "@/components/base/select/select";
import { useI18n } from "@/providers/locale-provider";
import { booleanPreference, optionChoiceKey, optionChoiceValue, optionDefaultKey, reportedOption, type SessionOption } from "@/lib/session-options";

// Three preference states, separate from the Agent's reported Actual.
export function SessionOptionControl({ option, requested, disabled, editable = true, onChange }: {
    option: SessionOption;
    requested?: string;
    disabled?: boolean;
    editable?: boolean;
    onChange?: (value: string | undefined) => void;
}) {
    const { t } = useI18n();
    const reported = reportedOption(option);
    const reservedBoolean = option.type === "boolean" && (["model", "mode"].includes(option.id) || ["model", "mode"].includes(option.category || ""));
    const supported = (option.type === "select" || option.type === "boolean") && !reservedBoolean;
    const pinned = requested !== undefined && requested !== "";
    const choices = option.type === "boolean" ? [{ value: "true", label: "true" }, { value: "false", label: "false" }] : option.choices;
    const items = [{ id: optionDefaultKey, label: t("consoleChrome.optionUnfixed") }, ...choices.filter(choice => choice.value !== "").map(choice => ({ id: optionChoiceKey(choice.value), label: choice.label }))];
    if (pinned && !choices.some(choice => choice.value === requested)) items.push({ id: optionChoiceKey(requested), label: requested });
    const requestedLabel = pinned ? choices.find(choice => choice.value === requested)?.label || requested : t("consoleChrome.optionUnfixed");
    const validBoolean = option.type !== "boolean" || !pinned || booleanPreference(requested);
    return <fieldset aria-label={option.name} className="min-w-0 space-y-2">
        <legend className="mb-1 text-sm font-medium text-primary [overflow-wrap:anywhere]">{option.name}</legend>
        <p translate="no" className="break-all font-mono text-xs text-quaternary">{option.id}{option.category ? ` · ${option.category}` : ""}</p>
        {editable && supported && onChange ? <Select size="sm" label={t("consoleChrome.optionRequested")}
            isDisabled={disabled} selectedKey={pinned ? optionChoiceKey(requested) : optionDefaultKey} items={items}
            onSelectionChange={key => {
                if (key == null) return;
                const value = optionChoiceValue(String(key));
                if (option.type === "boolean" && value !== undefined && !booleanPreference(value)) return;
                onChange(value);
            }}>
            {item => <Select.Item id={item.id}>{item.label}</Select.Item>}
        </Select> : <p className="break-words text-xs text-secondary">{t("consoleChrome.optionRequestedValue", { value: requestedLabel })}</p>}
        <p className="break-words text-xs text-tertiary">{t("consoleChrome.optionReported", { value: reported === undefined || reported === "" ? t("consoleChrome.optionNotReported") : reported })}</p>
        {!supported && <p className="text-xs text-tertiary">{t("consoleChrome.optionUnsupported")}</p>}
        {!validBoolean && <p className="text-xs text-warning-primary">{t("consoleChrome.optionInvalidSaved")}</p>}
    </fieldset>;
}
