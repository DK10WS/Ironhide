import { FolderOpen, Upload } from "lucide-react"
import { useRef, useState } from "react"
import { cn } from "cn"
import { Button } from "@/components/ui/button"
import { pickFromDrop, pickFromInput } from "@/lib/drop"

type DropzoneProps = {
  busy: boolean
  onPick: (files: ReturnType<typeof pickFromInput>) => void
}

/**
 * The one place a site goes in.
 *
 * A folder button sits next to the files one because the operating system offers
 * two different dialogs for them. Beyond that there is nothing to choose: what
 * arrived decides how it is sent.
 */
export function Dropzone({ busy, onPick }: DropzoneProps) {
  const input = useRef<HTMLInputElement | null>(null)
  const folderInput = useRef<HTMLInputElement | null>(null)
  const [over, setOver] = useState(false)

  // React does not know the attribute, and setting it on every render would
  // reset the element each time the state changes.
  function attachFiles(node: HTMLInputElement | null) {
    input.current = node
  }

  function attachFolder(node: HTMLInputElement | null) {
    folderInput.current = node
    if (node !== null) node.setAttribute("webkitdirectory", "")
  }

  async function onDrop(event: React.DragEvent<HTMLDivElement>) {
    event.preventDefault()
    setOver(false)
    if (busy) return

    const picked = await pickFromDrop(event.dataTransfer)
    if (picked.length > 0) onPick(picked)
  }

  function onChange(event: React.ChangeEvent<HTMLInputElement>) {
    const files = event.target.files
    if (files === null || files.length === 0) return

    onPick(pickFromInput(files))
    // Cleared so picking the same files again after a failed upload still fires.
    event.target.value = ""
  }

  return (
    <div
      onDragOver={(event) => {
        event.preventDefault()
        if (!busy) setOver(true)
      }}
      onDragLeave={() => setOver(false)}
      onDrop={onDrop}
      className={cn(
        "group relative flex flex-col items-center gap-5 rounded-2xl border border-dashed border-input px-6 py-16 text-center transition-colors sm:py-24",
        over ? "bg-accent" : "hover:bg-muted/40",
        busy && "pointer-events-none opacity-60",
      )}
    >
      <span
        aria-hidden
        className={cn(
          "grid size-14 place-items-center rounded-full bg-accent transition-transform",
          over ? "scale-110" : "group-hover:scale-105",
        )}
      >
        <Upload className="size-6" />
      </span>

      <div className="flex flex-col gap-2">
        <p className="text-lg font-semibold tracking-[-0.01em]">Drop a site here</p>
        <p className="mx-auto max-w-sm text-muted-foreground">
          A zip, a project folder, or loose files with an{" "}
          <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm">index.html</code> at
          the root.
        </p>
      </div>

      <div className="flex flex-wrap items-center justify-center gap-2">
        <Button
          type="button"
          variant="outline"
          disabled={busy}
          onClick={() => input.current?.click()}
        >
          <Upload />
          Choose files
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={busy}
          onClick={() => folderInput.current?.click()}
        >
          <FolderOpen />
          Choose a folder
        </Button>
      </div>

      <input ref={attachFiles} type="file" multiple onChange={onChange} className="hidden" />
      <input ref={attachFolder} type="file" onChange={onChange} className="hidden" />
    </div>
  )
}
