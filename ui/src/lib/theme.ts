import { useEffect, useSyncExternalStore } from "react";

// Theme handling like the website's selector: light, dark or system. The
// choice is stored in localStorage; index.html applies it before React
// renders to avoid a flash.

export type Theme = "light" | "dark" | "system";

const KEY = "shpyrd.theme";
const listeners = new Set<() => void>();

export function getTheme(): Theme {
  const v = localStorage.getItem(KEY);
  return v === "light" || v === "dark" ? v : "system";
}

export function setTheme(t: Theme) {
  if (t === "system") localStorage.removeItem(KEY);
  else localStorage.setItem(KEY, t);
  applyTheme();
  for (const l of listeners) l();
}

export function resolvedTheme(): "light" | "dark" {
  const t = getTheme();
  if (t !== "system") return t;
  return window.matchMedia("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

export function applyTheme() {
  document.documentElement.classList.toggle("dark", resolvedTheme() === "dark");
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const theme = useSyncExternalStore(
    (cb) => {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
    getTheme,
    getTheme,
  );
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => applyTheme();
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return [theme, setTheme];
}
