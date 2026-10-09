import { useEffect, useState } from "react"

/**
 * Tracks prefers-reduced-motion.
 *
 * border-beam's rotate types ship no reduced-motion handling of their own, so a
 * beam has to be switched off here rather than merely slowed down.
 */
export function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false)

  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)")
    setReduced(query.matches)

    const onChange = (event: MediaQueryListEvent) => setReduced(event.matches)
    query.addEventListener("change", onChange)
    return () => query.removeEventListener("change", onChange)
  }, [])

  return reduced
}
