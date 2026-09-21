import {
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type CSSProperties,
  type ReactNode,
} from "react";
import {
  Dialog,
  Modal,
  ModalOverlay,
} from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import {
  DialogBody,
  DialogFooter,
  DialogHeader,
  DialogSurface,
} from "@/components/steve/dialog-surface";
import { Md } from "@/components/steve/markdown";
import {
  choosePalette,
  contrastRatio,
  defaultAppearance,
  exportPalette,
  FONT_LIMITS,
  fontStack,
  importPalette,
  MAX_PALETTES,
  paletteChoices,
  removePalette,
  validFamily,
  type Appearance,
  type FontRole,
} from "@/lib/appearance";
import { appearanceStyle } from "@/lib/appearance-dom";
import { COLOR_KEYS, type Palette, type Scheme } from "@/lib/themes";
import { useI18n } from "@/providers/locale-provider";
import { useTheme } from "@/providers/theme-provider";
import "@/styles/appearance-settings.css";

const schemes = ["light", "dark"] as const;
const roles = ["ui", "reading", "code", "heading"] as const;
const presets = ["system", "serif", "mono"] as const;
const hex = /^#[0-9a-fA-F]{6}$/;
const MAX_FILE_BYTES = 64 * 1024;
type Draft = { palette: Palette; original: Palette; isNew: boolean };
const nameIsValid = (name: string) =>
  !!name.trim() && name.length <= 60 && !/[\u0000-\u001f]/.test(name);
const paletteEqual = (a: Palette, b: Palette) =>
  a.name === b.name &&
  a.scheme === b.scheme &&
  COLOR_KEYS.every((key) => a.colors[key] === b.colors[key]);
const colorsAreValid = (palette: Palette) =>
  COLOR_KEYS.every((key) => hex.test(palette.colors[key]));
const cleanPalette = (palette: Palette): Palette => ({
  ...palette,
  name: palette.name.trim(),
  swatch: [palette.colors.bg, palette.colors.accent, palette.colors.keyword],
});
const subscribeSystem = (listener: () => void) => {
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  media.addEventListener("change", listener);
  return () => media.removeEventListener("change", listener);
};
const systemSnapshot = () =>
  window.matchMedia("(prefers-color-scheme: dark)").matches;
const serverSnapshot = () => false;

// Patch only fields edited in this draft; unrelated changes from another window survive.
function mergeDraft(current: Appearance, draft: Draft): Appearance {
  const existing = current.custom.find((p) => p.id === draft.palette.id);
  if (draft.isNew ? current.custom.length >= MAX_PALETTES : !existing)
    return current;
  let palette = cleanPalette(draft.palette);
  if (!draft.isNew && existing) {
    const colors = { ...existing.colors };
    for (const key of COLOR_KEYS)
      if (draft.palette.colors[key] !== draft.original.colors[key])
        colors[key] = draft.palette.colors[key];
    palette = cleanPalette({
      ...existing,
      colors,
      name:
        draft.palette.name !== draft.original.name
          ? palette.name
          : existing.name,
      scheme:
        draft.palette.scheme !== draft.original.scheme
          ? palette.scheme
          : existing.scheme,
    });
  }
  const next = {
    ...current,
    custom: existing
      ? current.custom.map((p) => (p.id === palette.id ? palette : p))
      : [...current.custom, palette],
  };
  for (const scheme of schemes)
    if (next[scheme] === palette.id && scheme !== palette.scheme)
      next[scheme] = defaultAppearance()[scheme];
  return choosePalette(next, palette.scheme, palette.id);
}

export function SettingsAppearance() {
  const { t } = useI18n();
  const { appearance, updateAppearance, storageError } = useTheme();
  const systemDark = useSyncExternalStore(
    subscribeSystem,
    systemSnapshot,
    serverSnapshot,
  );
  const [draft, setDraft] = useState<Draft | null>(null);
  const [selectedCustom, setSelectedCustom] = useState("");
  const [confirmDelete, setConfirmDelete] = useState<Palette | null>(null);
  const [error, setError] = useState<
    "appearance.importInvalid" | "appearance.exportFailed" | null
  >(null);
  const [notice, setNotice] = useState<
    "appearance.saved" | "appearance.deleted" | null
  >(null);
  const [importing, setImporting] = useState(false);
  const [fontReset, setFontReset] = useState(0);
  const [savingDraft, setSavingDraft] = useState<Draft | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const editorInput = useRef<HTMLInputElement>(null);
  const importSequence = useRef(0);
  const custom =
    appearance.custom.find((p) => p.id === selectedCustom) ??
    appearance.custom[0];
  const atLimit = appearance.custom.length >= MAX_PALETTES;
  const missing =
    !!draft &&
    !draft.isNew &&
    !appearance.custom.some((p) => p.id === draft.palette.id);
  const blocked = !!draft && ((draft.isNew && atLimit) || missing);
  const draftDirty =
    !!draft && (draft.isNew || !paletteEqual(draft.palette, draft.original));
  const valid =
    !!draft && nameIsValid(draft.palette.name) && colorsAreValid(draft.palette);
  const resolvedScheme =
    appearance.mode === "system"
      ? systemDark
        ? "dark"
        : "light"
      : appearance.mode;
  const activePalette = paletteChoices(appearance, resolvedScheme).find(
    (p) => p.id === appearance[resolvedScheme],
  )!;
  // Invalid partial HEX values never enter CSS; retain the original seed until valid.
  const previewPalette = draft
    ? { ...draft.palette, colors: { ...draft.palette.colors } }
    : activePalette;
  if (draft)
    for (const key of COLOR_KEYS)
      if (!hex.test(previewPalette.colors[key]))
        previewPalette.colors[key] = draft.original.colors[key];
  const previewAppearance: Appearance = draft
    ? {
        ...appearance,
        mode: previewPalette.scheme,
        [previewPalette.scheme]: previewPalette.id,
        custom: [
          ...appearance.custom.filter((p) => p.id !== previewPalette.id),
          previewPalette,
        ],
      }
    : appearance;

  useEffect(() => {
    if (draft) editorInput.current?.focus();
  }, [draft?.palette.id]);
  useEffect(
    () => () => {
      importSequence.current++;
    },
    [],
  );
  useEffect(() => {
    if (!draftDirty) return;
    const href = window.location.href,
      historyState = window.history.state;
    let allowed = false;
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (!allowed) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    const click = (event: MouseEvent) => {
      if (
        event.defaultPrevented ||
        event.button !== 0 ||
        event.metaKey ||
        event.ctrlKey ||
        event.shiftKey ||
        event.altKey
      )
        return;
      const anchor = (event.target as Element)?.closest?.("a[href]");
      if (
        !(anchor instanceof HTMLAnchorElement) ||
        anchor.target === "_blank" ||
        anchor.hasAttribute("download") ||
        anchor.href === href
      )
        return;
      if (!window.confirm(t("appearance.discardDraft"))) {
        event.preventDefault();
        event.stopImmediatePropagation();
      } else allowed = true;
    };
    const pop = (event: PopStateEvent) => {
      if (allowed || window.location.href === href) return;
      if (window.confirm(t("appearance.discardDraft"))) {
        allowed = true;
        return;
      }
      event.stopImmediatePropagation();
      window.history.pushState(historyState, "", href);
    };
    // A separate capture listener leaves SettingsPage's single-slot back guard intact.
    window.addEventListener("beforeunload", beforeUnload);
    document.addEventListener("click", click, true);
    window.addEventListener("popstate", pop, true);
    return () => {
      window.removeEventListener("beforeunload", beforeUnload);
      document.removeEventListener("click", click, true);
      window.removeEventListener("popstate", pop, true);
    };
  }, [draftDirty, t]);
  useEffect(() => {
    if (!savingDraft) return;
    // The provider's functional update has committed. A concurrent limit/delete keeps the draft open.
    const saved = appearance.custom.find(
      (p) => p.id === savingDraft.palette.id,
    );
    if (saved) {
      setDraft(null);
      setSelectedCustom(saved.id);
      setNotice("appearance.saved");
    }
    setSavingDraft(null);
  }, [appearance, savingDraft]);

  function openDraft(palette: Palette, isNew: boolean) {
    setError(null);
    setNotice(null);
    setDraft({
      palette: { ...palette, colors: { ...palette.colors } },
      original: palette,
      isNew,
    });
  }
  function copyPalette(palette: Palette) {
    openDraft(
      {
        ...palette,
        id: `custom-${crypto.randomUUID()}`,
        name: t("appearance.copyName", { name: palette.name }).slice(0, 60),
      },
      true,
    );
  }
  async function readFile(file?: File) {
    if (!file || draft) return;
    const sequence = ++importSequence.current;
    setImporting(true);
    setError(null);
    setNotice(null);
    try {
      if (file.size > MAX_FILE_BYTES) throw new Error("file-too-large");
      const palette = importPalette(await file.text());
      if (sequence !== importSequence.current) return;
      openDraft({ ...palette, id: `custom-${crypto.randomUUID()}` }, true);
    } catch {
      if (sequence === importSequence.current)
        setError("appearance.importInvalid");
    } finally {
      if (sequence === importSequence.current) setImporting(false);
    }
  }
  function download(palette: Palette) {
    setError(null);
    try {
      const blob = new Blob([exportPalette(cleanPalette(palette))], {
        type: "application/json",
      });
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = "steve-palette.json";
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch {
      setError("appearance.exportFailed");
    }
  }
  function save() {
    if (!draft || !valid || blocked) return;
    setNotice(null);
    updateAppearance((current) => mergeDraft(current, draft));
    setSavingDraft(draft);
  }
  function patchPalette(patch: Partial<Palette>) {
    setDraft((current) =>
      current
        ? { ...current, palette: { ...current.palette, ...patch } }
        : current,
    );
  }

  return (
    <section className="appearance-settings" aria-label={t("appearance.title")}>
      <header className="appearance-heading">
        <h2>{t("appearance.title")}</h2>
        <p>{t("appearance.description")}</p>
      </header>
      {storageError && (
        <p className="appearance-warning" role="alert">
          {t("appearance.storageError")}
        </p>
      )}
      <div className="appearance-mode-row">
        <div>
          <h3>{t("appearance.mode")}</h3>
          <p className="appearance-help">{t("appearance.modeHint")}</p>
        </div>
        <Select
          size="sm"
          aria-label={t("appearance.mode")}
          selectedKey={appearance.mode}
          items={["system", ...schemes].map((id) => ({
            id,
            label: t(`appearance.${id as "system" | Scheme}`),
          }))}
          onSelectionChange={(key) => {
            if (key === "system" || key === "light" || key === "dark")
              updateAppearance((current) => ({ ...current, mode: key }));
          }}
        >
          {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
        </Select>
      </div>
      <div className="appearance-palette-pair">
        {schemes.map((scheme) => {
          const choices = paletteChoices(appearance, scheme);
          const chosen = choices.find((p) => p.id === appearance[scheme])!;
          return (
            <div className="appearance-palette-choice" key={scheme}>
              <Select
                size="sm"
                label={t(
                  scheme === "light"
                    ? "appearance.lightPalette"
                    : "appearance.darkPalette",
                )}
                selectedKey={appearance[scheme]}
                items={choices.map((p) => ({
                  id: p.id,
                  label: p.id === scheme ? t(`appearance.${scheme}`) : p.name,
                }))}
                onSelectionChange={(key) => {
                  if (key)
                    updateAppearance((current) =>
                      choosePalette(current, scheme, String(key)),
                    );
                }}
              >
                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
              </Select>
              <div className="appearance-choice-footer">
                <Swatches palette={chosen} />
                <Button
                  size="sm"
                  color="link-gray"
                  isDisabled={!!draft || importing || atLimit}
                  onClick={() => copyPalette(chosen)}
                >
                  {t("appearance.copy")}
                </Button>
              </div>
            </div>
          );
        })}
      </div>

      <Group title={t("appearance.fonts")} hint={t("appearance.fontsHint")}>
        {roles.map((role) => (
          <FontSetting
            key={`${role}-${fontReset}`}
            role={role}
            appearance={appearance}
            updateAppearance={updateAppearance}
          />
        ))}
        <p className="appearance-help">{t("appearance.fontFallback")}</p>
        <Button
          size="sm"
          color="link-gray"
          onClick={() => {
            updateAppearance((current) => ({
              ...current,
              fonts: defaultAppearance().fonts,
            }));
            setFontReset((value) => value + 1);
          }}
        >
          {t("appearance.resetFonts")}
        </Button>
      </Group>

      <Group
        title={t("appearance.custom")}
        hint={t("appearance.customHint", { max: MAX_PALETTES })}
      >
        <div className="appearance-actions">
          <span className="appearance-help appearance-count">
            {t("appearance.count", {
              count: appearance.custom.length,
              max: MAX_PALETTES,
            })}
          </span>
          <Button
            size="sm"
            color="secondary"
            isDisabled={!!draft || atLimit}
            isLoading={importing}
            onClick={() => fileInput.current?.click()}
          >
            {t("appearance.import")}
          </Button>
          <input
            ref={fileInput}
            type="file"
            accept=".json,application/json"
            hidden
            aria-label={t("appearance.import")}
            onChange={(event) => {
              const file = event.currentTarget.files?.[0];
              event.currentTarget.value = "";
              void readFile(file);
            }}
          />
        </div>
        <p className="appearance-help">{t("appearance.importHint")}</p>
        {atLimit && (
          <p className="appearance-warning" role="status">
            {t("appearance.limit", { max: MAX_PALETTES })}
          </p>
        )}
        {custom ? (
          <div className="appearance-custom-row">
            <Select
              size="sm"
              label={t("appearance.savedPalettes")}
              selectedKey={custom.id}
              isDisabled={!!draft || importing}
              items={appearance.custom.map((p) => ({
                id: p.id,
                label: p.name,
                supportingText: t(`appearance.${p.scheme}`),
              }))}
              onSelectionChange={(key) => {
                if (key) setSelectedCustom(String(key));
              }}
            >
              {(item) => (
                <Select.Item id={item.id} supportingText={item.supportingText}>
                  {item.label}
                </Select.Item>
              )}
            </Select>
            <div className="appearance-actions">
              <Button
                size="sm"
                color="secondary"
                isDisabled={!!draft || importing}
                onClick={() => openDraft(custom, false)}
              >
                {t("appearance.edit")}
              </Button>
              <Button
                size="sm"
                color="secondary"
                onClick={() => download(custom)}
              >
                {t("appearance.export")}
              </Button>
              <Button
                size="sm"
                color="secondary-destructive"
                isDisabled={!!draft || importing}
                onClick={() => setConfirmDelete(custom)}
              >
                {t("appearance.delete")}
              </Button>
            </div>
          </div>
        ) : (
          <p className="appearance-help">{t("appearance.customEmpty")}</p>
        )}
        {error && (
          <p className="appearance-error" role="alert">
            {t(error)}
          </p>
        )}
        {notice && (
          <p className="appearance-help" role="status">
            {t(notice)}
          </p>
        )}
      </Group>

      {draft && (
        <section
          className="appearance-editor"
          aria-label={t("appearance.editor")}
        >
          <header className="appearance-heading">
            <h3>{t("appearance.editor")}</h3>
            <p>{t("appearance.editorHint")}</p>
          </header>
          <div className="appearance-editor-fields">
            <Input
              ref={editorInput}
              size="sm"
              label={t("appearance.name")}
              value={draft.palette.name}
              onChange={(name) => patchPalette({ name })}
              isInvalid={!nameIsValid(draft.palette.name)}
              hint={
                !nameIsValid(draft.palette.name)
                  ? t("appearance.nameInvalid")
                  : undefined
              }
              autoComplete="off"
            />
            <Select
              size="sm"
              label={t("appearance.scheme")}
              selectedKey={draft.palette.scheme}
              items={schemes.map((id) => ({
                id,
                label: t(`appearance.${id}`),
              }))}
              onSelectionChange={(key) => {
                if (key === "light" || key === "dark")
                  patchPalette({ scheme: key });
              }}
            >
              {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>
          </div>
          <h4>{t("appearance.colors")}</h4>
          <p className="appearance-help">{t("appearance.colorsHint")}</p>
          <div className="appearance-colors">
            {COLOR_KEYS.map((key) => (
              <div className="appearance-color" key={key}>
                <input
                  type="color"
                  aria-label={t(`appearance.color.${key}`)}
                  value={previewPalette.colors[key]}
                  onChange={(event) =>
                    patchPalette({
                      colors: {
                        ...draft.palette.colors,
                        [key]: event.currentTarget.value,
                      },
                    })
                  }
                />
                <Input
                  size="sm"
                  label={`${t(`appearance.color.${key}`)} HEX`}
                  value={draft.palette.colors[key]}
                  isInvalid={!hex.test(draft.palette.colors[key])}
                  hint={
                    !hex.test(draft.palette.colors[key])
                      ? t("appearance.hexInvalid")
                      : undefined
                  }
                  onChange={(value) =>
                    patchPalette({
                      colors: { ...draft.palette.colors, [key]: value },
                    })
                  }
                  autoComplete="off"
                  spellCheck="false"
                  inputClassName="font-mono"
                />
              </div>
            ))}
          </div>
          {!colorsAreValid(draft.palette) && (
            <p className="appearance-help" role="status">
              {t("appearance.previewInvalid")}
            </p>
          )}
          {missing && (
            <p className="appearance-warning" role="alert">
              {t("appearance.missing")}
            </p>
          )}
          <div className="appearance-actions appearance-editor-actions">
            <Button
              size="sm"
              color="link-gray"
              isDisabled={paletteEqual(draft.palette, draft.original)}
              onClick={() => {
                if (window.confirm(t("appearance.discardDraft")))
                  setDraft((current) =>
                    current
                      ? {
                          ...current,
                          palette: {
                            ...current.original,
                            colors: { ...current.original.colors },
                          },
                        }
                      : current,
                  );
              }}
            >
              {t("appearance.resetPalette")}
            </Button>
            <Button
              size="sm"
              color="secondary"
              onClick={() => {
                if (
                  !draftDirty ||
                  window.confirm(t("appearance.discardDraft"))
                ) {
                  setDraft(null);
                  setError(null);
                }
              }}
            >
              {t("appearance.cancel")}
            </Button>
            <Button
              size="sm"
              color="secondary"
              isDisabled={!valid}
              onClick={() => download(draft.palette)}
            >
              {t("appearance.export")}
            </Button>
            <Button
              size="sm"
              color="primary"
              isDisabled={!valid || blocked}
              onClick={save}
            >
              {t("appearance.save")}
            </Button>
          </div>
        </section>
      )}
      <ReadingPreview
        appearance={previewAppearance}
        palette={previewPalette}
        isDraft={!!draft}
        systemDark={systemDark}
      />
      {confirmDelete && (
        <ModalOverlay
          isOpen
          isDismissable
          onOpenChange={(open) => {
            if (!open) setConfirmDelete(null);
          }}
        >
          <Modal className="max-w-md">
            <Dialog
              aria-label={t("appearance.deleteTitle", {
                name: confirmDelete.name,
              })}
            >
              <DialogSurface>
                <DialogBody>
                  <DialogHeader
                    title={t("appearance.deleteTitle", {
                      name: confirmDelete.name,
                    })}
                    description={t("appearance.deleteHint")}
                  />
                  <DialogFooter>
                    <Button
                      size="sm"
                      color="secondary"
                      autoFocus
                      onClick={() => setConfirmDelete(null)}
                    >
                      {t("appearance.cancel")}
                    </Button>
                    <Button
                      size="sm"
                      color="primary-destructive"
                      onClick={() => {
                        updateAppearance((current) =>
                          removePalette(current, confirmDelete.id),
                        );
                        setConfirmDelete(null);
                        setNotice("appearance.deleted");
                      }}
                    >
                      {t("appearance.delete")}
                    </Button>
                  </DialogFooter>
                </DialogBody>
              </DialogSurface>
            </Dialog>
          </Modal>
        </ModalOverlay>
      )}
    </section>
  );
}

function Group({
  title,
  hint,
  children,
}: {
  title: string;
  hint: string;
  children: ReactNode;
}) {
  return (
    <section className="appearance-group">
      <header className="appearance-heading">
        <h3>{title}</h3>
        <p>{hint}</p>
      </header>
      {children}
    </section>
  );
}
function Swatches({ palette }: { palette: Palette }) {
  return (
    <span className="appearance-swatches" aria-hidden="true">
      {palette.swatch.map((color, index) => (
        <span key={index} style={{ backgroundColor: color }} />
      ))}
    </span>
  );
}
function FontSetting({
  role,
  appearance,
  updateAppearance,
}: {
  role: FontRole;
  appearance: Appearance;
  updateAppearance: (update: (current: Appearance) => Appearance) => void;
}) {
  const { t } = useI18n();
  const family = appearance.fonts[role].family;
  const [localName, setLocalName] = useState<string | null>(null);
  const [customMode, setCustomMode] = useState(false);
  const previousFamily = useRef(family);
  useEffect(() => {
    if (previousFamily.current === family) return;
    const unsaved =
      localName !== null &&
      localName.trim() !== previousFamily.current &&
      localName.trim() !== family;
    previousFamily.current = family;
    if (!unsaved) {
      setLocalName(null);
      setCustomMode(false);
    }
  }, [family, localName]);
  const availablePresets = role === "code" ? (["mono"] as const) : presets;
  const isPreset = availablePresets.some((preset) => preset === family);
  const custom = customMode || !isPreset;
  const input = localName ?? (isPreset ? "" : family);
  const label = t(`appearance.font.${role}`);
  const setFamily = (next: string) =>
    updateAppearance((current) => ({
      ...current,
      fonts: {
        ...current.fonts,
        [role]: { ...current.fonts[role], family: next },
      },
    }));
  const sizes =
    role === "heading"
      ? []
      : Array.from(
          { length: FONT_LIMITS[role][1] - FONT_LIMITS[role][0] + 1 },
          (_, i) => ({
            id: String(FONT_LIMITS[role][0] + i),
            label: `${FONT_LIMITS[role][0] + i} px`,
          }),
        );
  return (
    <div className="appearance-font-row">
      <div
        className="appearance-font-label"
        style={{ fontFamily: fontStack(family, role) }}
      >
        {label}
      </div>
      <div className="appearance-font-controls">
        <Select
          size="sm"
          aria-label={t("appearance.family", { role: label })}
          selectedKey={custom ? "custom" : family}
          items={[...availablePresets, "custom" as const].map((id) => ({
            id,
            label: t(`appearance.preset.${id}`),
          }))}
          onSelectionChange={(key) => {
            if (key === "custom") {
              setCustomMode(true);
            } else if (availablePresets.some((p) => p === key)) {
              setCustomMode(false);
              setLocalName(null);
              setFamily(String(key));
            }
          }}
        >
          {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
        </Select>
        {role === "heading" ? (
          <Select
            size="sm"
            aria-label={t("appearance.scale")}
            selectedKey={String(appearance.fonts.heading.scale)}
            items={[
              { id: "0.9", label: t("appearance.scaleSmall") },
              { id: "1", label: t("appearance.scaleNormal") },
              { id: "1.15", label: t("appearance.scaleLarge") },
            ]}
            onSelectionChange={(key) => {
              if (key)
                updateAppearance((current) => ({
                  ...current,
                  fonts: {
                    ...current.fonts,
                    heading: { ...current.fonts.heading, scale: Number(key) },
                  },
                }));
            }}
          >
            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
          </Select>
        ) : (
          <Select
            size="sm"
            aria-label={t("appearance.size", { role: label })}
            selectedKey={String(appearance.fonts[role].size)}
            items={sizes}
            onSelectionChange={(key) => {
              if (key)
                updateAppearance((current) => ({
                  ...current,
                  fonts: {
                    ...current.fonts,
                    [role]: { ...current.fonts[role], size: Number(key) },
                  },
                }));
            }}
          >
            {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
          </Select>
        )}
        {custom && (
          <div className="appearance-local-font">
            <Input
              size="sm"
              aria-label={t("appearance.localFont", { role: label })}
              placeholder={t("appearance.fontPlaceholder")}
              value={input}
              onChange={setLocalName}
              isInvalid={localName !== null && !validFamily(input)}
              hint={
                localName !== null && !validFamily(input)
                  ? t("appearance.fontInvalid")
                  : localName !== null && input.trim() !== family
                    ? t("appearance.fontDraft", {
                        family: isPreset
                          ? t(
                              `appearance.preset.${family as (typeof presets)[number]}`,
                            )
                          : family,
                      })
                    : undefined
              }
              autoComplete="off"
              spellCheck="false"
              onKeyDown={(event) => {
                if (event.key === "Enter" && validFamily(input)) {
                  event.preventDefault();
                  setFamily(input.trim());
                }
              }}
            />
            <Button
              size="sm"
              color="secondary"
              isDisabled={!validFamily(input) || input.trim() === family}
              onClick={() => setFamily(input.trim())}
            >
              {t("appearance.applyFont")}
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}
function ReadingPreview({
  appearance,
  palette,
  isDraft,
  systemDark,
}: {
  appearance: Appearance;
  palette: Palette;
  isDraft: boolean;
  systemDark: boolean;
}) {
  const { t } = useI18n();
  const [clicked, setClicked] = useState(false);
  const scopedPalette = { ...palette, id: "custom-preview" };
  const scopedAppearance: Appearance = {
    ...appearance,
    mode: palette.scheme,
    [palette.scheme]: scopedPalette.id,
    custom: [
      ...appearance.custom.filter((item) => item.id !== scopedPalette.id),
      scopedPalette,
    ],
  };
  const textContrast = contrastRatio(palette.colors.text, palette.colors.bg);
  const mutedContrast = contrastRatio(palette.colors.muted, palette.colors.bg);
  return (
    <section className="appearance-group" aria-label={t("appearance.preview")}>
      <header className="appearance-heading">
        <h3>{t("appearance.preview")}</h3>
        <p>
          {t(isDraft ? "appearance.previewHint" : "appearance.previewLive")}
        </p>
      </header>
      <div
        data-appearance-preview
        data-theme={scopedPalette.id}
        className={`appearance-preview${palette.scheme === "dark" ? " dark-mode" : ""}`}
        style={appearanceStyle(scopedAppearance, systemDark) as CSSProperties}
      >
        <div className="appearance-preview-user">
          <span className="appearance-preview-meta">
            {t("appearance.previewUser")}
          </span>
          <p>{t("appearance.previewQuestion")}</p>
        </div>
        <div className="appearance-preview-answer">
          <div className="appearance-preview-meta">
            Steve{" "}
            <span className="appearance-preview-status">
              {t("appearance.previewStatus")}
            </span>
          </div>
          <Md variant="conversation" text={t("appearance.previewAnswer")} />
        </div>
        <div className="appearance-preview-controls">
          <Input
            size="sm"
            aria-label={t("appearance.previewInput")}
            placeholder={t("appearance.previewPlaceholder")}
          />
          <Button
            size="sm"
            color="secondary"
            onClick={() => setClicked((value) => !value)}
          >
            {t(
              clicked
                ? "appearance.previewActionDone"
                : "appearance.previewAction",
            )}
          </Button>
        </div>
      </div>
      <div className="appearance-contrast">
        <h4>{t("appearance.contrast")}</h4>
        <div className="appearance-contrast-values">
          <span>
            {t("appearance.contrastText", { ratio: textContrast.toFixed(2) })}
          </span>
          <span>
            {t("appearance.contrastMuted", { ratio: mutedContrast.toFixed(2) })}
          </span>
        </div>
        {(textContrast < 4.5 || mutedContrast < 4.5) && (
          <p className="appearance-warning" role="status">
            {t("appearance.contrastLow")}
          </p>
        )}
        <p className="appearance-help">{t("appearance.contrastHint")}</p>
      </div>
    </section>
  );
}
