import { createContext, useCallback, useContext, useEffect, useState } from "react"
import type { ReactNode } from "react"
import { getAccount, signOut as signOutRequest, type Account } from "@/lib/account"

/**
 * Whether sign-in has been resolved yet.
 *
 * "loading" is kept apart from "signed out" because a header that flashes a
 * Sign in link before asking the server looks broken to anyone who is signed
 * in. "error" is separate again, so a network failure is never shown as
 * "you are signed out".
 */
export type AccountState =
  | { status: "loading"; account: null }
  | { status: "signed-out"; account: null }
  | { status: "signed-in"; account: Account }
  | { status: "error"; account: null }

type AccountContextValue = {
  state: AccountState
  /** Re-reads the session, for after a sign-in that happened elsewhere. */
  refresh: () => Promise<void>
  signOut: () => Promise<void>
}

const AccountContext = createContext<AccountContextValue | null>(null)

export function AccountProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AccountState>({ status: "loading", account: null })

  const refresh = useCallback(async () => {
    try {
      const account = await getAccount()
      setState(
        account === null
          ? { status: "signed-out", account: null }
          : { status: "signed-in", account },
      )
    } catch {
      setState({ status: "error", account: null })
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const signOut = useCallback(async () => {
    await signOutRequest()
    setState({ status: "signed-out", account: null })
  }, [])

  return (
    <AccountContext
      value={{
        state,
        refresh,
        signOut,
      }}
    >
      {children}
    </AccountContext>
  )
}

/** Reads the account state, for the header and anywhere sign-in changes. */
export function useAccount(): AccountContextValue {
  const value = useContext(AccountContext)
  if (value === null) {
    throw new Error("useAccount must be used inside AccountProvider")
  }
  return value
}
