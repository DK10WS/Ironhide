import { Moon, Sun } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import { applyTheme, isDark } from "@/lib/theme"

export function ThemeToggle() {
  const [dark, setDark] = useState(isDark)

  function toggle() {
    const next = !dark
    applyTheme(next ? "dark" : "light")
    setDark(next)
  }

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      onClick={toggle}
      aria-label={dark ? "Switch to light theme" : "Switch to dark theme"}
    >
      {dark ? <Moon /> : <Sun />}
    </Button>
  )
}
