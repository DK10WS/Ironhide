import { useCallback, useEffect, useRef, useState } from "react"

/**
 * Copies text and reports whether it worked.
 *
 * The Clipboard API is unavailable outside a secure context, and writing to it
 * can still be refused by the browser. Both cases fall back to the old
 * selection-and-exec route, which works wherever the page does.
 */
export function useCopy(resetAfter = 2000) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // A pending reset must not fire after the row that owns it has gone.
  useEffect(() => {
    return () => {
      if (timer.current !== null) clearTimeout(timer.current)
    }
  }, [])

  const copy = useCallback(
    async (text: string) => {
      if (timer.current !== null) clearTimeout(timer.current)

      // Flipped before the write resolves. The browser's clipboard call takes
      // a moment to come back, and a button that sits unchanged in the
      // meantime reads as one that did nothing at all. If the write then
      // fails, the label corrects itself.
      setCopied(true)
      setFailed(false)

      const ok = await write(text)
      setCopied(ok)
      setFailed(!ok)

      timer.current = setTimeout(() => {
        setCopied(false)
        setFailed(false)
      }, resetAfter)
    },
    [resetAfter],
  )

  return { copied, failed, copy }
}

async function write(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText !== undefined) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Not a secure context, or the browser refused. Try the fallback.
    }
  }

  return writeBySelection(text)
}

function writeBySelection(text: string): boolean {
  const area = document.createElement("textarea")
  area.value = text
  area.setAttribute("readonly", "")
  // Kept in the layout but out of sight, so selecting it does not scroll the
  // page or flash a box the size of the text.
  area.style.position = "fixed"
  area.style.top = "0"
  area.style.left = "0"
  area.style.opacity = "0"
  document.body.appendChild(area)

  try {
    area.select()
    return document.execCommand("copy")
  } catch {
    return false
  } finally {
    area.remove()
  }
}
