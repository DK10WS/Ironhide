import { createFileRoute } from "@tanstack/react-router"
import { UploadPanel } from "@/components/upload-panel"

export const Route = createFileRoute("/")({
  component: HomePage,
})

function HomePage() {
  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold tracking-tight sm:text-3xl">
          Put a site on the internet
        </h1>
        <p className="text-muted-foreground">
          Upload a built site and it gets its own address straight away. There is nothing to
          configure and no account to make.
        </p>
      </div>

      <UploadPanel />
    </div>
  )
}
