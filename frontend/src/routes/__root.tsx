import { Link, Outlet, createRootRoute } from "@tanstack/react-router"
import { ThemeToggle } from "@/components/theme-toggle"

export const Route = createRootRoute({
  component: RootLayout,
})

function RootLayout() {
  return (
    <div className="flex min-h-dvh flex-col">
      <header className="border-b">
        <div className="mx-auto flex w-full max-w-3xl items-center justify-between gap-3 px-4 py-3 sm:px-6">
          <Link to="/" className="flex items-center gap-2 font-semibold tracking-tight">
            <span aria-hidden className="size-2.5 rounded-full bg-primary" />
            <span>Ironhide</span>
          </Link>
          <ThemeToggle />
        </div>
      </header>

      <main className="mx-auto w-full max-w-3xl flex-1 px-4 py-8 sm:px-6 sm:py-12">
        <Outlet />
      </main>

      <footer className="border-t">
        <div className="mx-auto w-full max-w-3xl px-4 py-6 text-sm text-muted-foreground sm:px-6">
          Every upload gets its own address. Nothing here is shared with anyone else.
        </div>
      </footer>
    </div>
  )
}
