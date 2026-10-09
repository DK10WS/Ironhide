import { useState } from "react"
import { ExternalLink, KeyRound } from "lucide-react"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Spinner } from "@/components/ui/spinner"
import { ApiError } from "@/lib/api"
import { formatDateTime } from "@/lib/format"
import { claimDeployment, type ClaimResult } from "@/lib/upload"
import { createFileRoute } from "@tanstack/react-router"

export const Route = createFileRoute("/claim/$token")({
  component: ClaimPage,
})

function ClaimPage() {
  const { token } = Route.useParams()
  const [claimed, setClaimed] = useState<ClaimResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function claim() {
    setBusy(true)
    setError(null)
    try {
      setClaimed(await claimDeployment(token))
    } catch (cause) {
      setError(messageFor(cause))
    } finally {
      setBusy(false)
    }
  }

  if (claimed !== null) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Kept</CardTitle>
          <CardDescription>
            This drop no longer expires. Claimed {formatDateTime(claimed.claimed_at)}.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Button asChild className="w-fit">
            <a href={claimed.site_url} target="_blank" rel="noreferrer noopener">
              Open the site
              <ExternalLink />
            </a>
          </Button>
        </CardContent>
      </Card>
    )
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold tracking-tight sm:text-3xl">Keep this site</h1>
        <p className="text-muted-foreground">
          This drop is on a timer. Keeping it clears that timer, so the address stops changing your
          mind and the site stays put.
        </p>
      </div>

      <div className="flex flex-col items-start gap-3">
        <Button type="button" onClick={() => void claim()} disabled={busy}>
          {busy ? <Spinner /> : <KeyRound />}
          {busy ? "Keeping…" : "Keep this site"}
        </Button>
        <p className="text-sm text-muted-foreground">
          It happens once and cannot be undone. Anyone who has this link can keep it.
        </p>
      </div>

      {error !== null && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function messageFor(cause: unknown): string {
  if (cause instanceof ApiError) return cause.message
  if (cause instanceof Error) return cause.message
  return "That link could not be redeemed. Try again in a moment."
}
