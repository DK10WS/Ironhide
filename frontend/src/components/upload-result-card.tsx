import { Link } from "@tanstack/react-router"
import { ExternalLink, FileText, KeyRound } from "lucide-react"
import { CopyLink } from "@/components/copy-link"
import { SitePreview } from "@/components/site-preview"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { formatBytes, formatFileCount, formatUntil } from "@/lib/format"
import { claimTokenFrom, isSinglePDF, type UploadResult } from "@/lib/upload"

type UploadResultCardProps = {
  result: UploadResult
  onReset: () => void
}

export function UploadResultCard({ result, onReset }: UploadResultCardProps) {
  // Absent for an owned upload, which needs no claim link.
  const token = result.claim_url === undefined ? null : claimTokenFrom(result.claim_url)
  const isPDF = isSinglePDF(result)
  const file = result.files[0]
  // The stored key is prefixed with the deployment's own directory, so only the
  // last segment is a name anyone recognises.
  const filename = file === undefined ? "" : file.key.slice(file.key.lastIndexOf("/") + 1)

  return (
    <Card>
      <CardHeader>
        <CardTitle>{isPDF ? "Your PDF is up" : "Your site is up"}</CardTitle>
        <CardDescription>
          {formatFileCount(result.files.length)} · {formatBytes(result.bytes)}
          {result.expires_at === null
            ? " · no expiry"
            : ` · expires ${formatUntil(result.expires_at)}`}
        </CardDescription>
      </CardHeader>

      <CardContent className="flex flex-col gap-6">
        <div className="flex flex-wrap gap-2">
          <Button asChild className="w-fit">
            <a href={result.preview_url} target="_blank" rel="noreferrer noopener">
              {isPDF ? "Open the PDF" : "Open the site"}
              <ExternalLink />
            </a>
          </Button>
          <CopyLink value={result.preview_url} label="Copy link" className="w-fit" />
        </div>

        {isPDF ? (
          // No preview frame. A PDF renders through the browser's own viewer,
          // which refuses to draw inside a nested frame often enough that the
          // result is a blank box, and there is nothing to scale down to a
          // thumbnail either. Saying what was uploaded beats showing nothing.
          <div className="flex items-center gap-3 rounded-xl border bg-muted/30 p-4">
            <span
              aria-hidden
              className="grid size-10 shrink-0 place-items-center rounded-lg bg-card"
            >
              <FileText className="size-5" />
            </span>
            <div className="min-w-0">
              <p className="truncate font-medium">{filename}</p>
              <p className="text-sm text-muted-foreground">
                Served at the root of its address, so the link opens the document directly.
              </p>
            </div>
          </div>
        ) : (
          <SitePreview src={result.preview_url} title="Uploaded site preview" />
        )}

        {result.owned === true ? (
          <section className="flex flex-col gap-3 border-t pt-6">
            <h2 className="text-sm font-medium">It is yours</h2>
            <p className="text-sm text-muted-foreground">
              This one is in your account and does not expire.
            </p>
            <Button asChild variant="outline" className="w-fit">
              <Link to="/sites">Manage your sites</Link>
            </Button>
          </section>
        ) : (
          result.expires_at !== null && (
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
                <div className="flex flex-wrap gap-2">
                  <Button asChild variant="outline" className="w-fit">
                    <Link to="/claim/$token" params={{ token }}>
                      <KeyRound />
                      Keep this site
                    </Link>
                  </Button>
                  {/* Built on this origin rather than the one in the response,
                      so the address in the clipboard opens for whoever it is
                      sent to, the same as the button above. */}
                  <CopyLink
                    value={`${window.location.origin}/claim/${token}`}
                    label="Copy claim link"
                    className="w-fit"
                  />
                </div>
              )}
            </section>
          )
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
