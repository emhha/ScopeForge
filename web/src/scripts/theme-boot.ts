import { PREFERENCE_DEFAULTS, PREFERENCE_PERSISTENCE } from "@/lib/preferences/preferences-config";
import { getClientCookie } from "@/lib/cookie.client";
import { applyThemeMode } from "@/lib/preferences/theme-utils";

// Run before mounting the app so the first visible frame uses the saved theme.
export function applyInitialPreferences() {
  const root = document.documentElement;
  for (const key of Object.keys(PREFERENCE_DEFAULTS) as (keyof typeof PREFERENCE_DEFAULTS)[]) {
    let value: string = PREFERENCE_DEFAULTS[key];
    const persistence = PREFERENCE_PERSISTENCE[key];
    try {
      const saved =
        persistence === "localStorage"
          ? localStorage.getItem(key)
          : persistence === "client-cookie" || persistence === "server-cookie"
            ? getClientCookie(key)
            : null;
      if (saved) value = saved;
    } catch {
      // A browser may deny storage access; retain the configured default.
    }
    root.setAttribute(`data-${key.replaceAll("_", "-")}`, value);
  }
  const mode = root.getAttribute("data-theme-mode");
  applyThemeMode(mode === "dark" || mode === "system" ? mode : "light");
}
