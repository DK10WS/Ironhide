import { useState } from "react"
import { cn } from "cn"
import { BorderBeam } from "border-beam"
import { Dropzone } from "@/components/dropzone"
import { UploadResultCard } from "@/components/upload-result-card"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { ApiError } from "@/lib/api"
import { zipPicked, type PickedFile } from "@/lib/drop"
import { formatBytes, formatFileCount } from "@/lib/format"
import { usePrefersReducedMotion } from "@/lib/motion"
import { uploadFiles, uploadZip, type UploadResult } from "@/lib/upload"

type Mode = "zip" | "folder" | "files"

const modes: { value: Mode; label: string; hint: string }[] = [
  {
    value: "zip",
    label: "Zip",
    hint: "One archive, sent exactly as you packed it.",
  },
  {
    value: "folder",
    label: "Folder",
    hint: "A project directory, packed here in the browser before anything is sent.",
  },
  {
    value: "files",
    label: "Files",
    hint: "Several files sent side by side, with the paths you picked them under.",
  },
]

export function UploadPanel() {
  const [mode, setMode] = useState<Mode>("zip")
  const [picked, setPicked] = useState<PickedFile[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<UploadResult | null>(null)
  const reduced = usePrefersReducedMotion()

  function choose(next: Mode) {
    setMode(next)
    setPicked([])
    setError(null)
  }

  function onPick(files: PickedFile[]) {
    setPicked(files)
    setError(null)
  }

  async function send() {
    setBusy(true)
    setError(null)
    try {
      setResult(await upload(mode, picked))
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
    <div className="flex flex-col gap-6">
      <div
        role="radiogroup"
        aria-label="How to send it"
        className="grid grid-cols-3 gap-1 rounded-lg bg-muted p-1"
      >
        {modes.map((entry) => (
          <button
            key={entry.value}
            type="button"
            role="radio"
            aria-checked={mode === entry.value}
            disabled={busy}
            onClick={() => choose(entry.value)}
            className={cn(
              "rounded-md px-3 py-2 text-sm font-medium transition-colors disabled:opacity-50",
              mode === entry.value
                ? "bg-background text-foreground shadow-xs"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {entry.label}
          </button>
        ))}
      </div>

      <p className="-mt-3 text-sm text-muted-foreground">
        {modes.find((m) => m.value === mode)?.hint}
      </p>

      <Dropzone directory={mode === "folder"} busy={busy} onPick={onPick} />

      {picked.length > 0 && (
        // A large upload runs for seconds, so the card doing the work carries a
        // beam. The rounded card is the wrapper's first child because the beam
        // takes its corner radius from there.
        <BorderBeam size="md" colorVariant="ocean" theme="auto" active={busy && !reduced}>
          <div
            aria-busy={busy}
            className="flex flex-col gap-3 rounded-lg border bg-card p-4 sm:flex-row sm:items-center sm:justify-between"
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

async function upload(mode: Mode, picked: PickedFile[]): Promise<UploadResult> {
  const only = picked[0]
  if (only === undefined) {
    throw new Error("Choose something to upload first.")
  }

  if (mode === "zip") {
    return uploadZip(only.file)
  }
  if (mode === "folder") {
    return uploadZip(await zipPicked(picked))
  }
  return uploadFiles(picked)
}

function messageFor(cause: unknown): string {
  if (cause instanceof ApiError) return cause.message
  if (cause instanceof Error) return cause.message
  return "That upload could not be completed. Try again in a moment."
}
