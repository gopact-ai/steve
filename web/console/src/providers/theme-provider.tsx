import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  APPEARANCE_KEY,
  defaultAppearance,
  normalizeAppearance,
  readAppearance,
  resolveAppearance,
  type Appearance,
} from "@/lib/appearance";
import { applyAppearance } from "@/lib/appearance-dom";
import type { Scheme } from "@/lib/themes";

interface ThemeContextType {
  appearance: Appearance;
  scheme: Scheme;
  updateAppearance: (
    update: (current: Appearance) => Appearance,
    options?: { requirePersistence?: boolean },
  ) => Promise<boolean>;
  storageError: boolean;
}
const ThemeContext = createContext<ThemeContextType | undefined>(undefined);
export function useTheme(): ThemeContextType {
  const value = useContext(ThemeContext);
  if (!value) throw new Error("useTheme must be used within ThemeProvider");
  return value;
}
const systemDark = () =>
  window.matchMedia("(prefers-color-scheme: dark)").matches;
function initial() {
  try {
    localStorage.getItem(APPEARANCE_KEY);
    return { appearance: readAppearance(localStorage), storageError: false };
  } catch {
    return { appearance: defaultAppearance(), storageError: true };
  }
}
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState(initial);
  const current = useRef(state.appearance);
  const pending = useRef<((a: Appearance) => Appearance)[]>([]);
  const [dark, setDark] = useState(systemDark);
  const { appearance, storageError } = state;
  const scheme = resolveAppearance(appearance, dark).scheme;
  const updateAppearance = useCallback(
    async (
      update: (current: Appearance) => Appearance,
      options?: { requirePersistence?: boolean },
    ) => {
      const commit = (canPersist: boolean) => {
        let base = current.current;
        let available = canPersist;
        if (available) {
          try {
            // The storage event may be delayed in a background window. Read
            // and update under the shared lock rather than overwriting its snapshot.
            // Windows in separate renderer processes see the store converge
            // asynchronously; the lock orders the saves, not that propagation,
            // so two saves within the same few milliseconds may still lose one.
            localStorage.getItem(APPEARANCE_KEY);
            base = readAppearance(localStorage);
            for (const retry of pending.current)
              base = normalizeAppearance(retry(base));
          } catch {
            available = false;
          }
        }
        const next = normalizeAppearance(update(base));
        let failed = !available;
        if (available) {
          try {
            localStorage.setItem(APPEARANCE_KEY, JSON.stringify(next));
          } catch {
            failed = true;
          }
        }
        // Explicit palette saves are transactional: failure leaves only the
        // editor's draft, not a queued mutation that could outlive Cancel/Reset.
        if (failed && options?.requirePersistence) {
          setState((state) => ({ ...state, storageError: true }));
          return false;
        }
        pending.current = failed ? [...pending.current, update] : [];
        current.current = next;
        setState({ appearance: next, storageError: failed });
        return !failed;
      };
      // Existing draft storage also requires Web Locks. Without a lock, keep
      // changes local and warn instead of claiming a safe multi-window save.
      if (!navigator.locks) return commit(false);
      try {
        return await navigator.locks.request(APPEARANCE_KEY, () =>
          commit(true),
        );
      } catch {
        return commit(false);
      }
    },
    [],
  );
  useLayoutEffect(() => {
    applyAppearance(appearance, dark);
  }, [appearance, dark]);
  useEffect(() => {
    const media = matchMedia("(prefers-color-scheme: dark)");
    const change = () => setDark(media.matches);
    const storage = (event: StorageEvent) => {
      if (event.key !== APPEARANCE_KEY && event.key !== null) return;
      if (pending.current.length) return; // keep explicitly unsaved local edits
      try {
        if (event.storageArea !== localStorage) return;
        // Read the latest value, rather than replaying an older event
        // after another window has already saved a newer preference.
        const next = readAppearance(localStorage);
        current.current = next;
        setState({ appearance: next, storageError: false });
      } catch {
        setState((s) => ({ ...s, storageError: true }));
      }
    };
    media.addEventListener("change", change);
    window.addEventListener("storage", storage);
    change();
    return () => {
      media.removeEventListener("change", change);
      window.removeEventListener("storage", storage);
    };
  }, []);
  const value = useMemo(
    () => ({ appearance, scheme, updateAppearance, storageError }),
    [appearance, scheme, updateAppearance, storageError],
  );
  return (
    <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
  );
}
