import { useRef, useState } from "react"
import { FolderOpen, Upload } from "lucide-react"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { ApiError } from "@/lib/api"
import { redeploySite } from "@/lib/account"
import { pickFromDrop, pickFromInput, type PickedFile } from "@/lib/drop"
import { formatBytes, formatFileCount } from "@/lib/format"
import { checkSize } from "@/lib/upload"

type RedeployControlProps = {
  siteId: number
  /** Used in the confirmation, so it is clear which site is being replaced. */
  hostname: string
  /** Hides the panel and returns the card to its normal buttons. */
  onCancel: () => void
  /** Called once the site has been replaced, to refresh its size and date. */
  onDone: () => void
}

/**
 * Replaces a site's contents without changing its address.
 *
 * Opens a panel rather than picking straight away, because the two choices are
 * not the same dialog and this keeps both available on one button. The panel
 * stays open until the upload succeeds or is cancelled, so a rejected one can be
 * corrected in place instead of starting over.
 */
export function RedeployControl({ siteId, hostname, onCancel, onDone }: RedeployControlProps) {
  const [picked, setPicked] = useState<PickedFile[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const filesInput = useRef<HTMLInputElement | null>(null)
  const folderInput = useRef<HTMLInputElement | null>(null)

  function attachFiles(node: HTMLInputElement | null) {
    filesInput.current = node
  }

  function attachFolder(node: HTMLInputElement | null) {
    folderInput.current = node
    if (node !== null) node.setAttribute("webkitdirectory", "")
  }

  function close() {
    setPicked([])
    setError(null)
    onCancel()
  }

  function onPick(found: PickedFile[]) {
    const tooBig = checkSize(found)
    if (tooBig !== null) {
      setPicked([])
      setError(tooBig)
      return
    }
    setPicked(found)
    setError(null)
  }

  function onChange(event: React.ChangeEvent<HTMLInputElement>) {
    const files = event.target.files
    if (files === null || files.length === 0) return
    onPick(pickFromInput(files))
    // Cleared so choosing the same files again after a failure still fires.
    event.target.value = ""
  }

  async function onDrop(event: React.DragEvent<HTMLDivElement>) {
    event.preventDefault()
    if (busy) return
    const found = await pickFromDrop(event.dataTransfer)
    if (found.length > 0) onPick(found)
  }

  async function send() {
    if (picked.length === 0) return
    const tooBig = checkSize(picked)
    if (tooBig !== null) {
      setError(tooBig)
      return
    }

    setBusy(true)
    setError(null)
    try {
      await redeploySite(siteId, picked)
      close()
      // Sizes and dates have all changed, so the list is stale either way.
      onDone()
    } catch (cause) {
      setError(
        cause instanceof ApiError || cause instanceof Error
          ? cause.message
          : "That could not be redeployed.",
      )
    } finally {
      setBusy(false)
    }
  }

  const total = picked.reduce((sum, { file }) => sum + file.size, 0)

  return (
    <div
      onDragOver={(event) => event.preventDefault()}
      onDrop={(event) => void onDrop(event)}
      className="flex w-full flex-col gap-3 rounded-lg border bg-muted/30 p-3"
    >
      <p className="text-sm text-muted-foreground">
        Replacing everything on <span className="font-medium text-foreground">{hostname}</span>. The
        address stays the same.
      </p>

      {picked.length > 0 ? (
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="min-w-0 text-sm">
            <span className="font-medium">{formatFileCount(picked.length)}</span>
            <span className="text-muted-foreground"> · {formatBytes(total)}</span>
            <span className="block truncate text-muted-foreground">{picked[0]?.path}</span>
          </p>
          <div className="flex gap-2">
            <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={close}>
              Cancel
            </Button>
            <Button type="button" size="sm" disabled={busy} onClick={() => void send()}>
              {busy && <Spinner />}
              {busy ? "Redeploying…" : "Redeploy"}
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => filesInput.current?.click()}
          >
            <Upload />
            Choose files
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => folderInput.current?.click()}
          >
            <FolderOpen />
            Choose a folder
          </Button>
          <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={close}>
            Cancel
          </Button>
        </div>
      )}

      {error !== null && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <input ref={attachFiles} type="file" multiple onChange={onChange} className="hidden" />
      <input ref={attachFolder} type="file" onChange={onChange} className="hidden" />
    </div>
  )
}
