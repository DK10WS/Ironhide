import { ApiError, readResponse } from "./api"
import type { PickedFile } from "./drop"
import { postPicked } from "./upload"

/** The signed-in account, as /api/me reports it. */
export type Account = {
  id: string
  email: string
  name: string
  picture: string
}

/** One site in the account's dashboard. */
export type Site = {
  id: number
  slug: string
  url: string
  bytes: number
  files: number
  /** RFC 3339, or empty when the site has never been deployed. */
  deployed_at: string
  live: boolean
}

export type Usage = {
  used: number
  limit: number
}

/**
 * Returns the signed-in account, or null when there is none.
 *
 * A 401 is the ordinary answer for a browser with no session, so it is turned
 * into null here rather than raised. Anything else is a real failure and is
 * left to the caller.
 */
export async function getAccount(): Promise<Account | null> {
  const response = await fetch("/api/me")
  if (response.status === 401) return null
  return readResponse<Account>(response)
}

export async function signOut(): Promise<void> {
  await readResponse(await fetch("/api/auth/logout", { method: "POST" }))
}

export async function listSites(): Promise<Site[]> {
  const body = await readResponse<{ sites: Site[] }>(await fetch("/api/my/sites"))
  return body.sites
}

export async function getUsage(): Promise<Usage> {
  return readResponse<Usage>(await fetch("/api/my/usage"))
}

export async function deleteSite(id: number): Promise<void> {
  await readResponse(await fetch(`/api/my/sites/${id}`, { method: "DELETE" }))
}

/**
 * Replaces a site's contents, keeping its address.
 *
 * The same detection the upload page uses decides whether this goes up as an
 * archive or as parts, and flatten is on for the same reason: a site zipped
 * inside a wrapper directory is refused without it. The server accepts both
 * shapes on one route, which is what keeps this to one button.
 */
export async function redeploySite(id: number, picked: PickedFile[]): Promise<void> {
  await readResponse(await postPicked(`/api/my/sites/${id}/deploy?flatten=true`, picked))
}

export function isUnauthorized(cause: unknown): boolean {
  return cause instanceof ApiError && cause.status === 401
}
