import { readResponse } from "./api"
import type { PickedFile } from "./drop"
import { formatBytes } from "./format"

/**
 * The largest upload the server will accept, in bytes.
 *
 * This mirrors MAX_TOTAL_BYTES in backend/internal/config/config.go. It is
 * duplicated rather than fetched because there is no config endpoint, so the
 * two can drift; the server's own 413 wins either way, and this check only
 * exists to avoid sending 50 MB only to be refused. Raise one, then the other.
 */
export const MAX_UPLOAD_BYTES = 50 * 1024 * 1024

/**
 * Checks a pick against the limit, before it is sent.
 *
 * For a zip the compressed size is all that can be checked without reading the
 * archive: the server measures what it extracts, so a small archive that
 * expands past the limit is caught there and not here.
 *
 * Returns null when the pick is fine, or a sentence explaining the refusal.
 */
export function checkSize(picked: PickedFile[]): string | null {
  if (picked.length === 0) return null

  const total = picked.reduce((sum, { file }) => sum + file.size, 0)
  if (total <= MAX_UPLOAD_BYTES) return null

  return `That is ${formatBytes(total)}. The limit is ${formatBytes(MAX_UPLOAD_BYTES)} — try trimming large images or video.`
}

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
 * True when the upload is a single PDF, which the server serves as the site
 * root rather than as a page.
 *
 * Kept in step with isLonePDF on the backend, which is what decides this.
 */
export function isSinglePDF(result: UploadResult): boolean {
  if (result.files.length !== 1) return false
  const file = result.files[0]
  if (file === undefined) return false
  return /\.pdf$/i.test(file.key)
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
