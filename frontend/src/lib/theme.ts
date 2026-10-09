/**
 * The stored theme, or the one the system asks for.
 *
 * The same key is read by the inline script in index.html, which is what sets
 * the class before first paint. It cannot import this module, so the key is
 * written in both places.
 */
export function preferredTheme(): "light" | "dark" {
  const stored = localStorage.getItem("ironhide-theme")
  if (stored === "light" || stored === "dark") {
    return stored
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"
}

export function isDark(): boolean {
  return document.documentElement.classList.contains("dark")
}

export function applyTheme(theme: "light" | "dark"): void {
  document.documentElement.classList.toggle("dark", theme === "dark")
  localStorage.setItem("ironhide-theme", theme)
}
