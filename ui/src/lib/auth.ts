import { useSyncExternalStore } from "react";

// The admin token lives in localStorage. `shpyrd cluster dashboard` opens the
// UI with `#token=...`; the fragment never reaches the server and is removed
// from the address bar once stored.

const KEY = "shpyrd.token";
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

export function getToken(): string | null {
  return localStorage.getItem(KEY);
}

export function setToken(token: string | null) {
  if (token) localStorage.setItem(KEY, token);
  else localStorage.removeItem(KEY);
  emit();
}

export function consumeTokenFragment() {
  const m = /(?:^#|&)token=([^&]+)/.exec(window.location.hash);
  if (m) {
    setToken(decodeURIComponent(m[1]));
    history.replaceState(
      null,
      "",
      window.location.pathname + window.location.search,
    );
  }
}

export function useToken(): string | null {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
    getToken,
    getToken,
  );
}
