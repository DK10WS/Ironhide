import { LogOut, Settings } from "lucide-react"
import { Link } from "@tanstack/react-router"
import { useAccount } from "@/components/account-provider"
import { Button } from "@/components/ui/button"

/**
 * Sign-in and sign-out for the header.
 *
 * Sign-in is a plain link to the backend's OAuth start rather than a scripted
 * call: the flow is a redirect to Google and back, so there is nothing for the
 * page to do but point at it.
 */
export function AccountControl() {
  const { state, signOut } = useAccount()

  if (state.status === "loading") {
    // Reserves the header's height so nothing jumps once the answer arrives.
    return <span className="block h-9 w-24" aria-hidden />
  }

  if (state.status === "error") {
    return (
      <span className="text-sm text-destructive" role="alert">
        Could not check your session
      </span>
    )
  }

  if (state.status === "signed-out") {
    return (
      <Button asChild variant="outline" size="sm" className="rounded-full">
        <a href="/api/auth/google">Sign in</a>
      </Button>
    )
  }

  return (
    <div className="flex items-center gap-2">
      <Button asChild variant="ghost" size="sm">
        <Link to="/sites">
          <Settings />
          <span className="hidden sm:inline">Your sites</span>
        </Link>
      </Button>

      <span className="max-w-32 truncate text-sm text-muted-foreground" title={state.account.email}>
        {state.account.name || state.account.email}
      </span>

      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        onClick={() => void signOut()}
        aria-label="Sign out"
      >
        <LogOut />
      </Button>
    </div>
  )
}
