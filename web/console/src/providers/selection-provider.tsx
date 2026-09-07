import {
  createContext,
  useCallback,
  useMemo,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { captureMaterial } from "@/lib/api/material";
import {
  getSubmissionSupport,
  subscribeSubmissionSupport,
  checkSubmissionSupport, fetchContext,
} from "@/lib/api/console";
import type { MaterialSelector } from "@/lib/types";
import type { CaptureSpec } from "@/components/steve/material-actions";
import { MaterialPreview } from "@/components/steve/material-shelf";
import { useMaterial } from "./material-provider";
import { useSideChat } from "./side-chat-provider";
import { useI18n } from "./locale-provider";
import "@/styles/selection.css";

export interface SelectedContent {
  capture: CaptureSpec;
  selector: MaterialSelector;
  excerpt: string;
  label?: string;
}
interface Surface {
  element: HTMLElement;
  resolve: (range: Range) => SelectedContent | null;
  version: string;
}
interface Current {
  manual?: boolean;
  surface: Surface;
  content: SelectedContent;
  range: Range;
  x: number;
  y: number;
  above: boolean;
}
interface SelectionContext {
  register: (id: string, surface: Surface) => () => void;
  show: (id: string, content: SelectedContent, element: HTMLElement) => void;
}
const Context = createContext<SelectionContext | null>(null);
export function SelectionProvider({ children }: { children: ReactNode }) {
  const surfaces = useRef(new Map<string, Surface>());
  const [current, setCurrent] = useState<Current | null>(null),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const [preview, setPreview] = useState<{
    project: string;
    id: string;
    selector: MaterialSelector;
  } | null>(null);
  const pending = useRef(false),
    toolbar = useRef<HTMLDivElement>(null),
    active = useRef(current);
  active.current = current;
  const materials = useMaterial(),
    side = useSideChat(),
    { t } = useI18n();
  const support = useSyncExternalStore(
    subscribeSubmissionSupport,
    getSubmissionSupport,
  );
  useEffect(() => {
    void checkSubmissionSupport();
  }, []);
  const position = (rect: DOMRect) => ({
    x: Math.max(
      8,
      Math.min(rect.left + rect.width / 2 - 175, window.innerWidth - 358),
    ),
    y: rect.top >= 64 ? rect.top - 8 : rect.bottom + 8,
    above: rect.top >= 64,
  });
  const register = useCallback((id: string, surface: Surface) => {
    surfaces.current.set(id, surface);
    return () => {
      surfaces.current.delete(id);
      setCurrent((value) => (value?.surface === surface ? null : value));
    };
  }, []);
  const show = useCallback(
    (id: string, content: SelectedContent, element: HTMLElement) => {
      const surface = surfaces.current.get(id);
      if (!surface) return;
      const range = document.createRange();
      range.selectNodeContents(element);
      setCurrent({
        surface,
        content,
        range,
        manual: true,
        ...position(element.getBoundingClientRect()),
      });
      setError("");
    },
    [],
  );
  useEffect(() => {
    let frame = 0;
    const update = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        if (
          pending.current ||
          toolbar.current?.contains(document.activeElement)
        )
          return;
        const selection = window.getSelection();
        if (!selection || selection.isCollapsed || selection.rangeCount !== 1) {
          if (!active.current?.manual) setCurrent(null);
          return;
        }
        const range = selection.getRangeAt(0),
          node =
            range.startContainer instanceof Element
              ? range.startContainer
              : range.startContainer.parentElement;
        const root = node?.closest<HTMLElement>("[data-selection-surface]"),
          id = root?.dataset.selectionSurface;
        const surface = id ? surfaces.current.get(id) : undefined;
        if (!surface || !surface.element.contains(range.endContainer)) {
          setCurrent(null);
          return;
        }
        const content = surface.resolve(range);
        if (!content) {
          setCurrent(null);
          return;
        }
        const rect = range.getBoundingClientRect();
        if (!rect.width && !rect.height) {
          setCurrent(null);
          return;
        }
        setCurrent({
          surface,
          content,
          range: range.cloneRange(),
          ...position(rect),
        });
        setError("");
      });
    };
    const keyboard = (event: KeyboardEvent) => {
      if (event.key === "Escape" && active.current) {
        event.preventDefault();
        event.stopPropagation();
        setCurrent(null);
        window.getSelection()?.removeAllRanges();
        return;
      }
      if (event.key === "F10" && event.shiftKey && active.current) {
        event.preventDefault();
        toolbar.current?.querySelector<HTMLButtonElement>("button")?.focus();
        return;
      }
      if (
        event.key === "Tab" &&
        active.current &&
        !toolbar.current?.contains(document.activeElement)
      ) {
        event.preventDefault();
        toolbar.current?.querySelector<HTMLButtonElement>("button")?.focus();
        return;
      }
      if (event.key.startsWith("Arrow") && event.shiftKey) update();
    };
    const down = (event: PointerEvent) => {
      if (!toolbar.current?.contains(event.target as Node) && !pending.current)
        setCurrent(null);
    };
    const move = () => {
      if (!pending.current) setCurrent(null);
    };
    document.addEventListener("pointerdown", down);
    document.addEventListener("selectionchange", update);
    document.addEventListener("pointerup", update);
    document.addEventListener("keyup", update);
    document.addEventListener("keydown", keyboard, true);
    window.addEventListener("scroll", move, true);
    window.addEventListener("resize", move);
    return () => {
      cancelAnimationFrame(frame);
      document.removeEventListener("pointerdown", down);
      document.removeEventListener("selectionchange", update);
      document.removeEventListener("pointerup", update);
      document.removeEventListener("keyup", update);
      document.removeEventListener("keydown", keyboard, true);
      window.removeEventListener("scroll", move, true);
      window.removeEventListener("resize", move);
    };
  }, []);
  async function action(kind: "chat" | "details" | "ask") {
    if (!current || pending.current) return;
    const selected = current,
      target = materials.target;
    if (kind === "chat" && !target) {
      setError(t("materials.noTarget"));
      return;
    }
    pending.current = true;
    setBusy(true);
    setError("");
    try {
      const material = await captureMaterial(
        selected.content.capture.project,
        selected.content.capture.source,
        selected.content.capture.title,
      );
      if (
        !selected.surface.element.isConnected ||
        !Array.from(surfaces.current.values()).includes(selected.surface)
      )
        throw new Error(t("selection.changed"));
      const ref = { id: material.id, selector: selected.content.selector };
      if (kind === "chat") {
                const { context } = await fetchContext(target!.conversation);
                if (context?.project?.id !== target!.project || !context.project.bound) throw new Error(t("materials.wrongProject"));
                materials.add(material, ref, target!);
            }
      if (kind === "details")
        setPreview({
          project: material.project,
          id: material.id,
          selector: ref.selector,
        });
      if (kind === "ask")
        side.open({
          material,
          ref,
          excerpt: selected.content.excerpt,
          originConversation: target?.conversation,
        });
      setCurrent(null);
      window.getSelection()?.removeAllRanges();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    } finally {
      pending.current = false;
      setBusy(false);
    }
  }
  useLayoutEffect(() => {
    if (!current || !toolbar.current) return;
    const anchor = current.range.getBoundingClientRect(),
      box = toolbar.current.getBoundingClientRect(),
      gap = 8;
    const x = Math.max(
      gap,
      Math.min(
        anchor.left + anchor.width / 2 - box.width / 2,
        window.innerWidth - box.width - gap,
      ),
    );
    const above = anchor.top - box.height - gap >= gap;
    const y = above
      ? anchor.top - gap
      : Math.max(
          gap,
          Math.min(anchor.bottom + gap, window.innerHeight - box.height - gap),
        );
    if (x !== current.x || y !== current.y || above !== current.above)
      setCurrent({ ...current, x, y, above });
  }, [current, busy, error]);
  const value = useMemo<SelectionContext>(
    () => ({ register, show }),
    [register, show],
  );
  return (
    <Context.Provider value={value}>
      {children}
      {current &&
        support.material_refs &&
        createPortal(
          <div
            ref={toolbar}
            className={`selection-toolbar ${current.above ? "is-above" : ""}`}
            style={{ left: current.x, top: current.y }}
            role="toolbar"
            aria-label={t("selection.actions")}
            onPointerDown={(e) => e.preventDefault()}
            onKeyDown={(event) => {
              if (
                !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)
              )
                return;
              const buttons = Array.from(
                  event.currentTarget.querySelectorAll<HTMLButtonElement>(
                    "button:enabled",
                  ),
                ),
                index = buttons.indexOf(
                  document.activeElement as HTMLButtonElement,
                );
              event.preventDefault();
              buttons[
                event.key === "Home"
                  ? 0
                  : event.key === "End"
                    ? buttons.length - 1
                    : (index +
                        (event.key === "ArrowRight" ? 1 : buttons.length - 1)) %
                      buttons.length
              ]?.focus();
            }}
          >
            {current.content.label && (
              <span className="selection-label">{current.content.label}</span>
            )}
            <div className="selection-buttons">
              <button
                type="button"
                disabled={
                  busy ||
                  !materials.target ||
                  materials.target.project !== current.content.capture.project
                }
                onClick={() => void action("chat")}
              >
                {t("selection.add")}
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => void action("details")}
              >
                {t("selection.details")}
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => void action("ask")}
              >
                {t("selection.ask")}
              </button>
              <button
                type="button"
                aria-label={t("selection.dismiss")}
                disabled={busy}
                onClick={() => {
                  setCurrent(null);
                  window.getSelection()?.removeAllRanges();
                }}
              >
                ×
              </button>
            </div>
            {busy && (
              <span role="status" className="selection-label">
                {t("selection.reading")}
              </span>
            )}
            {error && (
              <span role="alert" className="selection-error">
                {error}
              </span>
            )}
          </div>,
          current.surface.element,
        )}
      {preview && (
        <MaterialPreview
          project={preview.project}
          anchor={{ id: preview.id, selector: preview.selector }}
          onClose={() => setPreview(null)}
        />
      )}
    </Context.Provider>
  );
}
export function SelectionSurface({
  children,
  resolve,
  version,
  className,
}: {
  children: ReactNode;
  resolve: (range: Range, root: HTMLElement) => SelectedContent | null;
  version: string;
  className?: string;
}) {
  const context = useContext(Context),
    id = useId(),
    root = useRef<HTMLDivElement>(null),
    latest = useRef(resolve);
  latest.current = resolve;
  const register = context?.register;
  useLayoutEffect(() => {
    if (!root.current || !register) return;
    const element = root.current;
    return register(id, {
      element,
      version,
      resolve: (range) => latest.current(range, element),
    });
  }, [id, version, register]);
  return (
    <div ref={root} data-selection-surface={id} className={className}>
      {children}
    </div>
  );
}
export function useSelectionAction() {
  const context = useContext(Context);
  return (content: SelectedContent, element: HTMLElement) => {
    const surface = element.closest<HTMLElement>("[data-selection-surface]");
    if (surface?.dataset.selectionSurface)
      context?.show(surface.dataset.selectionSurface, content, element);
  };
}
