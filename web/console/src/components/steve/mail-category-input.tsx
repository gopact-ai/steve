import { useId } from "react";
import { TextField, InputBase } from "@/components/base/input/input";
import { Label } from "@/components/base/input/label";
import { HintText } from "@/components/base/input/hint-text";
import { useI18n } from "@/providers/locale-provider";

export function MailCategoryInput({ value, onChange, groups, disabled }: { value: string; onChange: (value: string) => void; groups: string[]; disabled: boolean }) {
    const { t } = useI18n(); const id = useId();
    return <TextField name="mail-category" value={value} onChange={onChange} maxLength={80} autoComplete="off" isDisabled={disabled}>
        <Label isRequired={false}>{t("mail.category")}</Label><InputBase list={id} /><datalist id={id}>{groups.map(group => <option key={group} value={group} />)}</datalist><HintText>{t("mail.categoryHint")}</HintText>
    </TextField>;
}
