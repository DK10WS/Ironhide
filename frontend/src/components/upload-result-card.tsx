import { Link } from "@tanstack/react-router"
import { ExternalLink, KeyRound } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { formatBytes, formatFileCount, formatUntil } from "@/lib/format"
import { claimTokenFrom, type UploadResult } from "@/lib/upload"

type UploadResultCardProps = {
  result: UploadResult
  onReset: () => void
}

export function UploadResultCard({ result, onReset }: UploadResultCardProps) {
  const token = claimTokenFrom(result.claim_url)

  return (
    <Card>
      <CardHeader>
        <CardTitle>Your site is up</CardTitle>
        <CardDescription>
          {formatFileCount(result.files.length)} · {formatBytes(result.bytes)}
          {result.expires_at === null
            ? " · no expiry"
            : ` · expires ${formatUntil(result.expires_at)}`}
        </CardDescription>
      </CardHeader>

      <CardContent className="flex flex-col gap-6">
        <Button asChild className="w-fit">
          <a href={result.preview_url} target="_blank" rel="noreferrer noopener">
            Open the site
            <ExternalLink />
          </a>
        </Button>

        {result.expires_at !== null && (
          <section className="flex flex-col gap-3 border-t pt-6">
            <h2 className="text-sm font-medium">Keep it</h2>
            <p className="text-sm text-muted-foreground">
              Until then this is a temporary drop. The link below makes it permanent, once, for
              whoever holds it.
            </p>
            {token === null ? (
              <p role="alert" className="text-sm text-destructive">
                That claim link came back malformed, so it cannot be used from here.
              </p>
            ) : (
              <Button asChild variant="outline" className="w-fit">
                <Link to="/claim/$token" params={{ token }}>
                  <KeyRound />
                  Keep this site
                </Link>
              </Button>
            )}
          </section>
        )}

        <div className="border-t pt-6">
          <Button type="button" variant="ghost" onClick={onReset}>
            Upload another
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
