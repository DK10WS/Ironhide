import { Check, Copy } from "lucide-react"
import { Button } from "@/components/ui/button"
import { useCopy } from "@/lib/use-copy"

type CopyLinkProps = {
  /** What lands on the clipboard. Usually the site's address. */
  value: string
  /** Spoken and shown when the write fails. */
  label?: string
  className?: string
}

/**
 * Copies a link and says so.
 *
 * The label changes for a couple of seconds afterwards, because a button that
 * copies silently reads as one that is broken — there is no visible effect to
 * confirm the click registered.
 */
export function CopyLink({ value, label = "Copy link", className }: CopyLinkProps) {
  const { copied, failed, copy } = useCopy()

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      className={className}
      onClick={() => void copy(value)}
    >
      {copied ? <Check /> : <Copy />}
      {failed ? "Copy failed" : copied ? "Copied" : label}
    </Button>
  )
}
