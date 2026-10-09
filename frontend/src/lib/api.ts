/** An error the server returned, carrying its status and its own message. */
export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
  }
}

/**
 * Turns a non-OK response into an ApiError.
 *
 * The backend answers failures with `{"error": "..."}` and hides driver detail
 * behind "internal error", so the body is the only useful thing to show.
 */
export async function readResponse<T>(response: Response): Promise<T> {
  const body = await readBody(response)

  if (!response.ok) {
    throw new ApiError(response.status, messageFrom(body, response.status))
  }
  return body as T
}

type ErrorBody = { error?: unknown }

function messageFrom(body: unknown, status: number): string {
  const message = (body as ErrorBody | null)?.error
  if (typeof message === "string" && message !== "") {
    return message
  }
  if (status >= 500) {
    return "The server had a problem. Try again in a moment."
  }
  return `The server refused that request (${status}).`
}

async function readBody(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch {
    // A proxy or an unhandled failure can answer with HTML, which is not a
    // message worth putting in front of a person.
    return null
  }
}
