import { useState } from "react"
import { BorderBeam } from "border-beam"
import { Dropzone } from "@/components/dropzone"
import { UploadResultCard } from "@/components/upload-result-card"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { ApiError } from "@/lib/api"
import { detectUpload, type PickedFile } from "@/lib/drop"
import { formatBytes, formatFileCount } from "@/lib/format"
import { usePrefersReducedMotion } from "@/lib/motion"
import { checkSize, uploadFiles, uploadZip, type UploadResult } from "@/lib/upload"

export function UploadPanel() {
  const [picked, setPicked] = useState<PickedFile[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<UploadResult | null>(null)
  const reduced = usePrefersReducedMotion()

  function onPick(files: PickedFile[]) {
    setError(null)
    // Refused here rather than on send: picking a 300 MB folder and then
    // finding the button does nothing is a worse experience than being told
    // straight away, and the pick can simply be made again smaller.
    const tooBig = checkSize(files)
    if (tooBig !== null) {
      setPicked([])
      setError(tooBig)
      return
    }
    setPicked(files)
  }

  async function send() {
    const tooBig = checkSize(picked)
    if (tooBig !== null) {
      setError(tooBig)
      return
    }

    setBusy(true)
    setError(null)
    try {
      setResult(await upload(picked))
      setPicked([])
    } catch (cause) {
      setError(messageFor(cause))
    } finally {
      setBusy(false)
    }
  }

  if (result !== null) {
    return (
      <UploadResultCard
        result={result}
        onReset={() => {
          setResult(null)
          setPicked([])
        }}
      />
    )
  }

  const totalBytes = picked.reduce((sum, { file }) => sum + file.size, 0)

  return (
    <div className="flex flex-col gap-4">
      <Dropzone busy={busy} onPick={onPick} />

      {picked.length > 0 && (
        // A large upload runs for seconds, so the card doing the work carries a
        // beam. The rounded card is the wrapper's first child because the beam
        // takes its corner radius from there.
        <BorderBeam size="md" colorVariant="sunset" theme="auto" active={busy && !reduced}>
          <div
            aria-busy={busy}
            className="flex flex-col gap-3 rounded-xl border bg-card p-4 sm:flex-row sm:items-center sm:justify-between"
          >
            <p className="min-w-0 text-sm">
              <span className="font-medium">{formatFileCount(picked.length)}</span>
              <span className="text-muted-foreground"> · {formatBytes(totalBytes)}</span>
              <span className="block truncate text-muted-foreground">
                {picked[0]?.path}
                {picked.length > 1 ? ` and ${picked.length - 1} more` : ""}
              </span>
            </p>
            <Button type="button" onClick={() => void send()} disabled={busy}>
              {busy && <Spinner />}
              {busy ? "Uploading…" : "Upload"}
            </Button>
          </div>
        </BorderBeam>
      )}

      {error !== null && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

/**
 * Sends whatever was picked.
 *
 * A lone archive goes up as an archive. Everything else goes as separate parts,
 * with a shared wrapper folder removed so the site's index lands at the root,
 * which is the same rule the server would otherwise apply.
 */
async function upload(picked: PickedFile[]): Promise<UploadResult> {
  if (picked.length === 0) {
    throw new Error("Choose something to upload first.")
  }

  const detected = await detectUpload(picked)
  if (detected.kind === "zip") {
    return uploadZip(detected.archive)
  }
  return uploadFiles(detected.files)
}

function messageFor(cause: unknown): string {
  if (cause instanceof ApiError) return cause.message
  if (cause instanceof Error) return cause.message
  return "That upload could not be completed. Try again in a moment."
}
