import { createFileRoute } from "@tanstack/react-router"
import { UploadPanel } from "@/components/upload-panel"

export const Route = createFileRoute("/")({
  component: HomePage,
})

function HomePage() {
  return (
    <div className="flex flex-col gap-12">
      <div className="flex flex-col items-center gap-5 text-center">
        {/* Line-height 1 and a slight negative tracking are what make the
            reference's display type read as set rather than as a paragraph. */}
        <h1 className="max-w-3xl text-4xl leading-[1.05] font-bold tracking-[-0.03em] text-balance sm:text-6xl">
          Put a site on the internet
        </h1>
        <p className="max-w-lg text-pretty text-base text-muted-foreground sm:text-lg">
          Drag a built site in. It gets its own address straight away — nothing to configure, no
          server to run.
        </p>
      </div>

      <UploadPanel />

      <p className="text-center text-sm text-muted-foreground">
        Sign in before uploading and your sites stay put instead of expiring.
      </p>
    </div>
  )
}
