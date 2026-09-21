import { Palette } from "@untitledui/icons";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { IconButton } from "@/components/steve/icon-button";
import { useI18n } from "@/providers/locale-provider";
import { paletteChoices, resolveAppearance, type Mode } from "@/lib/appearance";
import { useTheme } from "@/providers/theme-provider";

// The toolbar and settings edit the same preferences. A quick palette pick
// explicitly switches mode; the other mode's palette remains untouched.
export function ThemeMenu() {
  const { t } = useI18n();
  const { appearance, scheme, updateAppearance, storageError } = useTheme();
  const { palette } = resolveAppearance(appearance, scheme === "dark");
  const name =
    palette.id === "light" || palette.id === "dark"
      ? t(`settings.${palette.id}`)
      : palette.name;
  return (
    <Dropdown.Root>
      <IconButton
        label={`${t("consoleChrome.theme")}: ${name}`}
        icon={Palette}
      />
      <Dropdown.Popover placement="bottom end" className="w-64">
        <Dropdown.Menu aria-label={t("consoleChrome.theme")}>
          <Dropdown.Section
            selectionMode="single"
            disallowEmptySelection
            selectedKeys={[appearance.mode]}
            onSelectionChange={(keys) => {
              const mode = [...keys][0] as Mode;
              if (mode) updateAppearance((a) => ({ ...a, mode }));
            }}
          >
            <Dropdown.SectionHeader className="conversation-arrange-label">
              {t("consoleChrome.themeBasic")}
            </Dropdown.SectionHeader>
            {(["system", "light", "dark"] as const).map((mode) => (
              <Dropdown.Item
                key={mode}
                id={mode}
                textValue={t(`settings.${mode}`)}
              >
                {t(`settings.${mode}`)}
              </Dropdown.Item>
            ))}
          </Dropdown.Section>
          {(["light", "dark"] as const).map((s) => (
            <Dropdown.Section
              key={s}
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={[appearance[s]]}
              onSelectionChange={(keys) => {
                const id = String([...keys][0] ?? "");
                if (id) updateAppearance((a) => ({ ...a, [s]: id, mode: s }));
              }}
            >
              <Dropdown.SectionHeader className="conversation-arrange-label">
                {t(`settings.${s}`)}
              </Dropdown.SectionHeader>
              {paletteChoices(appearance, s)
                .filter((p) => p.id !== s)
                .map((p) => (
                  <Dropdown.Item key={p.id} id={p.id} textValue={p.name}>
                    <span className="theme-row">
                      <span className="theme-swatch" aria-hidden="true">
                        {p.swatch.map((color, i) => (
                          <span key={i} style={{ background: color }} />
                        ))}
                      </span>
                      <span className="truncate">{p.name}</span>
                    </span>
                  </Dropdown.Item>
                ))}
            </Dropdown.Section>
          ))}
          <Dropdown.Separator />
          <Dropdown.Item
            id="appearance-settings"
            href="#/settings?section=appearance"
            textValue={t("settings.appearance")}
          >
            {t("settings.appearance")}
            {storageError ? " ⚠" : ""}
          </Dropdown.Item>
        </Dropdown.Menu>
      </Dropdown.Popover>
    </Dropdown.Root>
  );
}
