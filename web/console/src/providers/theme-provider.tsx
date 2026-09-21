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
  updateAppearance: (update: (current: Appearance) => Appearance) => void;
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
  const [dark, setDark] = useState(systemDark);
  const { appearance, storageError } = state;
  const scheme = resolveAppearance(appearance, dark).scheme;
  const updateAppearance = useCallback(
    (update: (current: Appearance) => Appearance) => {
      const next = normalizeAppearance(update(current.current));
      let failed = false;
      try {
        localStorage.setItem(APPEARANCE_KEY, JSON.stringify(next));
      } catch {
        failed = true;
      }
      current.current = next;
      setState({ appearance: next, storageError: failed });
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
