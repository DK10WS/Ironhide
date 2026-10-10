import { useEffect, useRef, useState } from "react"

// Laid out at this width, then scaled down. A layout that only works at a phone
// width says nothing about how the site actually looks.
const VIEWPORT_WIDTH = 1280
const PREVIEW_HEIGHT = 420
const MAX_SCALE = 0.5

type SitePreviewProps = {
  src: string
  title: string
}

/**
 * Shows the uploaded site inside a scaled iframe.
 *
 * The frame keeps its own origin, so the page cannot reach this document or
 * anything the visitor has open here. allow-top-navigation is deliberately
 * absent, so an upload cannot frame-bust the app out from under the visitor.
 *
 * A site that sends its own X-Frame-Options or frame-ancestors will refuse to
 * be embedded. Cross-origin frames report nothing readable back, so that
 * refusal cannot be detected from here; the caption below says to use the link.
 */
export function SitePreview({ src, title }: SitePreviewProps) {
  const box = useRef<HTMLDivElement | null>(null)
  const [scale, setScale] = useState(0.4)
  const [loaded, setLoaded] = useState(false)

  // The scale follows the column, so a narrow screen shows the whole page
  // smaller rather than running off both sides.
  useEffect(() => {
    const element = box.current
    if (element === null) return

    const measure = () => {
      const width = element.clientWidth
      if (width > 0) {
        setScale(Math.min(MAX_SCALE, width / VIEWPORT_WIDTH))
      }
    }

    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    return () => observer.disconnect()
  }, [])

  return (
    <div className="flex flex-col gap-2">
      <span className="text-center text-sm font-medium">Preview</span>

      <div
        ref={box}
        style={{ height: PREVIEW_HEIGHT }}
        className="relative overflow-hidden rounded-lg border bg-muted"
      >
        {!loaded && (
          <span className="absolute inset-0 grid place-content-center text-sm text-muted-foreground">
            Loading the preview…
          </span>
        )}

        <iframe
          src={src}
          title={title}
          loading="lazy"
          referrerPolicy="no-referrer"
          sandbox="allow-scripts allow-same-origin allow-popups"
          onLoad={() => setLoaded(true)}
          style={{
            width: VIEWPORT_WIDTH,
            height: Math.round(PREVIEW_HEIGHT / scale),
            transform: `scale(${scale})`,
            transformOrigin: "top left",
            border: 0,
          }}
          className="absolute left-0 top-0 bg-white"
        />
      </div>

      <p className="text-xs text-muted-foreground">
        A scaled view at desktop width. If a site refuses to be embedded, the button above opens it
        directly.
      </p>
    </div>
  )
}
