import { Link, Outlet, createRootRoute } from "@tanstack/react-router"
import { AccountControl } from "@/components/account-control"
import { AccountProvider } from "@/components/account-provider"
import { GithubMark } from "@/components/github-mark"
import { ThemeToggle } from "@/components/theme-toggle"

export const Route = createRootRoute({
  component: RootLayout,
})

function RootLayout() {
  return (
    <AccountProvider>
      <div className="flex min-h-dvh flex-col">
        <header className="border-b">
          <div className="mx-auto flex w-full max-w-3xl items-center justify-between gap-3 px-4 py-4 sm:px-6">
            <Link to="/" className="text-base font-bold tracking-[-0.02em]">
              Ironhide
            </Link>
            <div className="flex items-center gap-2">
              <AccountControl />
              <ThemeToggle />
            </div>
          </div>
        </header>

        <main className="mx-auto w-full max-w-3xl flex-1 px-4 py-12 sm:px-6 sm:py-16">
          <Outlet />
        </main>

        <footer className="border-t">
          <div className="mx-auto flex w-full max-w-3xl items-center justify-between gap-4 px-4 py-8 text-sm text-muted-foreground sm:px-6">
            <p>Every upload gets its own address. Nothing here is shared with anyone else.</p>
            <a
              href="https://github.com/dk10ws/ironhide"
              target="_blank"
              rel="noreferrer noopener"
              aria-label="Ironhide on GitHub"
              className="-mr-2 shrink-0 rounded-full p-2 transition-colors hover:bg-accent hover:text-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
            >
              <GithubMark className="size-5" />
            </a>
          </div>
        </footer>
      </div>
    </AccountProvider>
  )
}
