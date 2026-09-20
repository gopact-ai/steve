import { IconButton } from "@/components/steve/icon-button";
import { Palette } from "@untitledui/icons";

import { Dropdown } from "@/components/base/dropdown/dropdown";
import { useI18n } from "@/providers/locale-provider";
import { PALETTES, paletteOf, type ThemeId } from "@/lib/themes";
import { useTheme } from "@/providers/theme-provider";

// The palette a person works in is theirs, so the control sits in the
// toolbar rather than three clicks into settings. Each row carries the
// scheme's own background, accent and text, which is how someone
// recognises Dracula or Nord without reading the name.
export function ThemeMenu() {
    const { t } = useI18n();
    const { theme, setTheme } = useTheme();
    const basics: { id: ThemeId; label: string }[] = [
        { id: "system", label: t("settings.system") },
        { id: "light", label: t("settings.light") },
        { id: "dark", label: t("settings.dark") },
    ];
    const current = paletteOf(theme)?.name ?? basics.find((b) => b.id === theme)?.label ?? "";
    return (
        <Dropdown.Root>
            <IconButton label={`${t("consoleChrome.theme")}: ${current}`} icon={Palette} />
            <Dropdown.Popover placement="bottom end" className="w-56">
                <Dropdown.Menu aria-label={t("consoleChrome.theme")}>
                    <Dropdown.Section selectionMode="single" disallowEmptySelection selectedKeys={[theme]}
                        onSelectionChange={(keys) => { const pick = [...keys][0]; if (pick) setTheme(String(pick) as ThemeId); }}>
                        <Dropdown.SectionHeader className="conversation-arrange-label">{t("consoleChrome.themeBasic")}</Dropdown.SectionHeader>
                        {basics.map((b) => <Dropdown.Item key={b.id} id={b.id} textValue={b.label}><span className="theme-row"><Swatch colors={swatchOf(b.id)} /><span className="truncate">{b.label}</span></span></Dropdown.Item>)}
                    </Dropdown.Section>
                    <Dropdown.Separator />
                    <Dropdown.Section selectionMode="single" disallowEmptySelection selectedKeys={[theme]}
                        onSelectionChange={(keys) => { const pick = [...keys][0]; if (pick) setTheme(String(pick) as ThemeId); }}>
                        <Dropdown.SectionHeader className="conversation-arrange-label">{t("consoleChrome.themePalettes")}</Dropdown.SectionHeader>
                        {PALETTES.map((p) => <Dropdown.Item key={p.id} id={p.id} textValue={p.name}><span className="theme-row"><Swatch colors={p.swatch} /><span className="truncate">{p.name}</span></span></Dropdown.Item>)}
                    </Dropdown.Section>
                </Dropdown.Menu>
            </Dropdown.Popover>
        </Dropdown.Root>
    );
}

// "Follow system" shows both ends of what it can turn into.
const swatchOf = (id: ThemeId): [string, string, string] =>
    id === "dark" ? ["#202124", "#0a84ff", "#f2f2f7"]
        : id === "light" ? ["#ffffff", "#0071e3", "#1d1d1f"]
            : ["#ffffff", "#0071e3", "#202124"];

function Swatch({ colors }: { colors: [string, string, string] }) {
    return (
        <span className="theme-swatch" aria-hidden="true">
            {colors.map((color, i) => <span key={i} style={{ background: color }} />)}
        </span>
    );
}
