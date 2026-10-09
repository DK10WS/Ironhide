import { FolderOpen, Upload } from "lucide-react"
import { useRef, useState } from "react"
import { cn } from "cn"
import { Button } from "@/components/ui/button"
import { pickFromDrop, pickFromInput } from "@/lib/drop"

type DropzoneProps = {
  /** When set, the file picker opens on a directory instead of on files. */
  directory: boolean
  busy: boolean
  onPick: (files: ReturnType<typeof pickFromInput>) => void
}

export function Dropzone({ directory, busy, onPick }: DropzoneProps) {
  const input = useRef<HTMLInputElement | null>(null)
  const [over, setOver] = useState(false)

  // React does not know the attribute, and toggling it per render would reset
  // the element on every mode change.
  function attach(node: HTMLInputElement | null) {
    input.current = node
    if (node === null) return
    if (directory) {
      node.setAttribute("webkitdirectory", "")
    } else {
      node.removeAttribute("webkitdirectory")
    }
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
        "flex flex-col items-center gap-4 rounded-xl border border-dashed px-6 py-10 text-center transition-colors sm:py-14",
        over ? "border-primary bg-accent" : "border-border bg-muted/30",
        busy && "pointer-events-none opacity-60",
      )}
    >
      <Upload className="size-6 text-muted-foreground" aria-hidden />
      <p className="text-sm text-muted-foreground">
        Drop {directory ? "a project folder" : "files"} here
      </p>
      <Button
        type="button"
        variant="outline"
        disabled={busy}
        onClick={() => input.current?.click()}
      >
        <FolderOpen />
        {directory ? "Choose a folder" : "Choose files"}
      </Button>

      <input
        ref={attach}
        type="file"
        multiple
        onChange={onChange}
        className="hidden"
        aria-label={directory ? "Choose a project folder" : "Choose files"}
      />
    </div>
  )
}
