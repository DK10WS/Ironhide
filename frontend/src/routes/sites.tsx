import { useCallback, useEffect, useState } from "react"
import { ExternalLink, RefreshCw, Trash2, Upload } from "lucide-react"
import { Link, createFileRoute, useLocation } from "@tanstack/react-router"
import { useAccount } from "@/components/account-provider"
import { CopyLink } from "@/components/copy-link"
import { RedeployControl } from "@/components/redeploy-control"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Spinner } from "@/components/ui/spinner"
import { deleteSite, getUsage, listSites, type Site, type Usage } from "@/lib/account"
import { formatBytes, formatDateTime } from "@/lib/format"

export const Route = createFileRoute("/sites")({
  component: SitesPage,
})

type Load =
  | { status: "loading" }
  | { status: "ready"; sites: Site[]; usage: Usage | null }
  | { status: "error"; message: string }

function SitesPage() {
  const { state } = useAccount()
  const [load, setLoad] = useState<Load>({ status: "loading" })
  const [busy, setBusy] = useState(false)

  // A failed sign-in comes back here with the reason. Shown once and then
  // dropped, so it does not reappear on every later visit to the page.
  const signInError = useLocation().search["error"]
  const [shown, setShown] = useState<string | undefined>(signInError)
  useEffect(() => {
    if (signInError !== undefined) setShown(signInError)
  }, [signInError])

  const fetchSites = useCallback(async () => {
    try {
      // Both requests are independent, so they go together rather than in
      // sequence: the allowance is shown beside the list, not after it.
      const [sites, usage] = await Promise.all([listSites(), getUsage()])
      setLoad({ status: "ready", sites, usage })
    } catch (cause) {
      setLoad({
        status: "error",
        message: cause instanceof Error ? cause.message : "Your sites could not be loaded.",
      })
    }
  }, [])

  useEffect(() => {
    if (state.status === "signed-in") {
      void fetchSites()
    }
  }, [state.status, fetchSites])

  async function remove(id: number) {
    setBusy(true)
    try {
      await deleteSite(id)
      // Re-read rather than splicing the local copy, so what is on screen is
      // what the server now believes.
      await fetchSites()
    } catch (cause) {
      setLoad({
        status: "error",
        message: cause instanceof Error ? cause.message : "That site could not be deleted.",
      })
    } finally {
      setBusy(false)
    }
  }

  if (state.status === "loading") {
    return <Pending label="Checking your session…" />
  }

  if (state.status !== "signed-in") {
    return <SignedOut errored={state.status === "error"} />
  }

  if (load.status === "loading") {
    return <Pending label="Loading your sites…" />
  }

  if (load.status === "error") {
    return (
      <div className="flex flex-col gap-4">
        <Alert variant="destructive" role="alert">
          <AlertDescription>{load.message}</AlertDescription>
        </Alert>
        <Button type="button" variant="outline" className="w-fit" onClick={() => void fetchSites()}>
          <RefreshCw />
          Try again
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-6">
      {shown !== undefined && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{shown}</AlertDescription>
        </Alert>
      )}

      <div className="flex flex-col gap-2">
        <h1 className="text-3xl leading-[1.05] font-bold tracking-[-0.03em] sm:text-4xl">
          Your sites
        </h1>
        <p className="text-muted-foreground">
          Sites you upload while signed in live here and never expire.
        </p>
      </div>

      {load.usage !== null && <UsageBar usage={load.usage} />}

      {load.sites.length === 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>Nothing here yet</CardTitle>
            <CardDescription>Upload something and it will show up on this page.</CardDescription>
          </CardHeader>
          <CardContent>
            <Button asChild className="w-fit">
              <Link to="/">Upload a site</Link>
            </Button>
          </CardContent>
        </Card>
      ) : (
        <ul className="flex flex-col gap-3">
          {load.sites.map((site) => (
            <SiteRow
              key={site.id}
              site={site}
              busy={busy}
              onDelete={() => void remove(site.id)}
              onChanged={() => void fetchSites()}
            />
          ))}
        </ul>
      )}
    </div>
  )
}

function SiteRow({
  site,
  busy,
  onDelete,
  onChanged,
}: {
  site: Site
  busy: boolean
  onDelete: () => void
  onChanged: () => void
}) {
  // The hostname without its scheme is what a person would recognise. The slug
  // on its own means nothing outside the database.
  const host = site.url.replace(/^https?:\/\//, "").replace(/\/$/, "")
  const [redeploying, setRedeploying] = useState(false)

  return (
    <li>
      <Card className="gap-2 transition-colors hover:bg-muted/30">
        <CardHeader className="gap-2">
          <CardTitle className="truncate font-semibold tracking-[-0.01em]">{host}</CardTitle>
          <CardDescription className="flex flex-wrap items-center gap-x-2">
            {site.live ? (
              <span className="inline-flex items-center gap-1.5 text-positive">
                <span aria-hidden className="size-1.5 rounded-full bg-positive" />
                Live
              </span>
            ) : (
              <span className="inline-flex items-center gap-1.5">
                <span aria-hidden className="size-1.5 rounded-full bg-muted-foreground" />
                Not live
              </span>
            )}
            <span aria-hidden>·</span>
            <span>{formatBytes(site.bytes)}</span>
            <span aria-hidden>·</span>
            <span>{site.files === 1 ? "1 file" : `${site.files} files`}</span>
            {site.deployed_at !== "" && (
              <>
                <span aria-hidden>·</span>
                <span>{formatDateTime(site.deployed_at)}</span>
              </>
            )}
          </CardDescription>
        </CardHeader>

        <CardContent className="flex flex-col gap-3">
          {redeploying ? (
            // The panel takes the row's place rather than sitting below it, so
            // there is only ever one thing to do here and no second Redeploy
            // button to tell apart from this one.
            <RedeployControl
              siteId={site.id}
              hostname={host}
              onCancel={() => setRedeploying(false)}
              onDone={onChanged}
            />
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <Button asChild size="sm">
                <a href={site.url} target="_blank" rel="noreferrer noopener">
                  Open site
                  <ExternalLink />
                </a>
              </Button>
              <CopyLink value={site.url} />
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setRedeploying(true)}
              >
                <Upload />
                Redeploy
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={onDelete}
                className="text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
              >
                <Trash2 />
                Delete
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
    </li>
  )
}

/** The allowance, so the number is not something a person has to remember. */
function UsageBar({ usage }: { usage: Usage }) {
  const percent = usage.limit > 0 ? Math.min(100, (usage.used / usage.limit) * 100) : 0
  const full = percent >= 100

  return (
    <div className="rounded-xl border bg-card p-4">
      <div className="flex items-baseline justify-between gap-3">
        <span className="eyebrow">Storage</span>
        <span className={full ? "font-semibold text-destructive" : "text-muted-foreground"}>
          {formatBytes(usage.used)} of {formatBytes(usage.limit)}
        </span>
      </div>
      <div
        role="progressbar"
        aria-label="Storage used"
        aria-valuenow={Math.round(percent)}
        aria-valuemin={0}
        aria-valuemax={100}
        className="mt-3 h-2 w-full overflow-hidden rounded-full bg-muted"
      >
        <div
          className={full ? "h-full bg-destructive" : "h-full rounded-full bg-primary"}
          style={{ width: `${percent}%` }}
        />
      </div>
    </div>
  )
}

function SignedOut({ errored }: { errored: boolean }) {
  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight sm:text-3xl">Your sites</h1>
      {errored ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            Your session could not be checked. Try again in a moment.
          </AlertDescription>
        </Alert>
      ) : (
        <p className="text-muted-foreground">
          Sign in to see the sites you have uploaded. Uploads made without signing in still work,
          they just expire.
        </p>
      )}
      <Button asChild className="w-fit">
        <a href="/api/auth/google">Sign in with Google</a>
      </Button>
    </div>
  )
}

function Pending({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-2 text-muted-foreground">
      <Spinner />
      <span className="text-sm">{label}</span>
    </div>
  )
}
