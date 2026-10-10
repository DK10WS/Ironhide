import { readResponse } from "./api"
import type { PickedFile } from "./drop"

/** One stored object, as the upload result reports it. */
export type StoredFile = {
  key: string
  size: number
  etag?: string
}

export type UploadResult = {
  id: number
  /** Where the site is served from, on the data plane. */
  preview_url: string
  /**
   * The single-use link that makes the drop permanent.
   *
   * Absent when the upload was made while signed in: such a site is already
   * owned and permanent, so there is nothing to redeem and no credential worth
   * putting in a response.
   */
  claim_url?: string
  /** True when the upload was attached to a signed-in account. */
  owned?: boolean
  /** RFC 3339 deadline, or null when the drop never expires. */
  expires_at: string | null
  bytes: number
  files: StoredFile[]
}

export type ClaimResult = {
  id: number
  site_url: string
  claimed_at: string
}

/**
 * Posts a zip archive. Exactly one part, and it must really be a zip.
 *
 * Flattening is always on and deliberately not a caller choice. The server only
 * strips a directory when every entry shares one, so an archive that is already
 * correct is untouched, while an archive wrapped in `mysite/` — which is
 * otherwise rejected with a 422 and no root index.html — just works. Without it
 * that upload is refused rather than silently served at the wrong depth.
 */
export async function uploadZip(archive: File): Promise<UploadResult> {
  const body = new FormData()
  body.append("file", archive, archive.name)

  const response = await fetch("/api/upload/zip?flatten=true", {
    method: "POST",
    body,
  })
  return readResponse<UploadResult>(response)
}

/** Posts several files, which keep the paths they were picked under. */
export async function uploadFiles(picked: PickedFile[]): Promise<UploadResult> {
  const body = new FormData()
  for (const { file, path } of picked) {
    body.append("file", file, path)
  }

  const response = await fetch("/api/upload/files", { method: "POST", body })
  return readResponse<UploadResult>(response)
}

/** Redeems a claim token, making an unclaimed drop permanent. */
export async function claimDeployment(token: string): Promise<ClaimResult> {
  const response = await fetch(`/api/claim/${encodeURIComponent(token)}`, {
    method: "POST",
  })
  return readResponse<ClaimResult>(response)
}

/**
 * Pulls the token out of a claim link.
 *
 * The link is built from the backend's own public URL, which is not the address
 * this page was served from, so the token is taken from its last segment and
 * reused on this origin.
 */
export function claimTokenFrom(claimUrl: string): string | null {
  try {
    const last = new URL(claimUrl).pathname.split("/").filter(Boolean).pop()
    return last ?? null
  } catch {
    return null
  }
}
