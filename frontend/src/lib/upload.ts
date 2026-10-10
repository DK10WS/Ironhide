import { readResponse } from "./api"
import { detectUpload, type PickedFile } from "./drop"
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
 * Builds the request body for a picked set, choosing its shape.
 *
 * A lone archive goes up as an archive, anything else as separate parts with a
 * shared wrapper directory already removed, so the site's index lands at the
 * root. The server applies the same rule, and would refuse a picked folder sent
 * with the wrapper still attached for having no root index.html.
 */
async function bodyFor(picked: PickedFile[]): Promise<{ body: FormData; kind: "zip" | "files" }> {
  const body = new FormData()
  const detected = await detectUpload(picked)

  if (detected.kind === "zip") {
    body.append("file", detected.archive, detected.archive.name)
    return { body, kind: "zip" }
  }
  for (const { file, path } of detected.files) {
    body.append("file", file, path)
  }
  return { body, kind: "files" }
}

/**
 * Posts a picked set to an endpoint that decides the shape for itself.
 *
 * Only the redeploy route works this way. The upload routes are separate and
 * have to be matched to the shape, or a set of loose files arrives at the
 * archive route and is refused for not being one.
 *
 * Flattening stays on and is deliberately not a caller choice. The server only
 * strips a directory when every entry shares one, so an already-correct archive
 * is untouched.
 */
export async function postPicked(path: string, picked: PickedFile[]): Promise<Response> {
  const { body } = await bodyFor(picked)
  return fetch(path, { method: "POST", body })
}

/** Uploads a picked set as a new site. */
export async function uploadPicked(picked: PickedFile[]): Promise<UploadResult> {
  const { body, kind } = await bodyFor(picked)
  const path = kind === "zip" ? "/api/upload/zip?flatten=true" : "/api/upload/files"
  return readResponse<UploadResult>(await fetch(path, { method: "POST", body }))
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
