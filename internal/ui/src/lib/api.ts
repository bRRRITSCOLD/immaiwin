// Central API client. Two pieces:
//
//   1. installCredentialsDefault() — patches window.fetch so EVERY
//      request in the app sends cookies (`credentials: 'include'`).
//      Imported once from src/routes/__root.tsx for its side effect.
//      The 51-call legacy fetch sites in this codebase don't pass
//      `credentials` themselves; without this patch the auth cookie
//      never travels cross-origin and every protected route 401s.
//
//   2. apiFetch / api.{get,post,delete,put} — typed JSON helper used
//      by new auth code. Throws ApiError on !ok with parsed body.
//
// API_BASE defaults to http://localhost:8080 — matches the existing
// per-component pattern. Override with VITE_API_URL.

// Default to https://localhost:8080 — the API runs TLS in dev (cert in
// .private/certs). Use the same host the OAuth REDIRECT_URLs and the
// API itself use; cookie host scoping is strict so a localhost vs
// localhost mismatch silently breaks the post-OAuth session.
export const API_BASE: string =
  (import.meta.env['VITE_API_URL'] as string | undefined) ?? 'https://localhost:8080'

let installed = false

export function installCredentialsDefault() {
  if (installed) return
  installed = true
  if (typeof window === 'undefined') return
  const orig = window.fetch.bind(window)
  window.fetch = (input, init) => {
    const merged: RequestInit = { credentials: 'include', ...(init ?? {}) }
    if (!('credentials' in (init ?? {}))) merged.credentials = 'include'
    return orig(input, merged)
  }
}

export class ApiError extends Error {
  status: number
  body: unknown
  constructor(message: string, status: number, body: unknown) {
    super(message)
    this.status = status
    this.body = body
  }
}

interface ApiOptions {
  // path is appended to API_BASE; should start with /api/v1/... or /auth/...
  signal?: AbortSignal
  headers?: Record<string, string>
}

async function apiFetch<T = unknown>(
  method: string,
  path: string,
  body?: unknown,
  opts: ApiOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) }
  if (body !== undefined) headers['Content-Type'] ??= 'application/json'
  const res = await fetch(`${API_BASE}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'include',
    signal: opts.signal,
  })
  let parsed: unknown = null
  const text = await res.text()
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = text
    }
  }
  if (!res.ok) {
    const msg =
      (parsed && typeof parsed === 'object' && 'error' in (parsed as Record<string, unknown>)
        ? String((parsed as Record<string, unknown>).error)
        : null) ?? `HTTP ${res.status}`
    throw new ApiError(msg, res.status, parsed)
  }
  return parsed as T
}

export const api = {
  get: <T = unknown>(path: string, opts?: ApiOptions) => apiFetch<T>('GET', path, undefined, opts),
  post: <T = unknown>(path: string, body?: unknown, opts?: ApiOptions) =>
    apiFetch<T>('POST', path, body, opts),
  put: <T = unknown>(path: string, body?: unknown, opts?: ApiOptions) =>
    apiFetch<T>('PUT', path, body, opts),
  delete: <T = unknown>(path: string, opts?: ApiOptions) =>
    apiFetch<T>('DELETE', path, undefined, opts),
}

// getWSToken mints a 60s JWT for a WebSocket upgrade. Browser WS
// API can't set Authorization headers and the auth cookie may not
// travel cross-origin on the upgrade handshake, so the UI passes the
// token via the `?token=` query string. The middleware honours it
// alongside cookie + Bearer-header auth.
export async function getWSToken(): Promise<string> {
  const res = await api.post<{ token: string; expires_in: number }>('/api/v1/auth/ws_token')
  return res.token
}

// buildWSURL converts an http(s) API path to a ws(s) URL with a
// freshly-minted ws_token appended as `?token=`. Path should start
// with `/api/v1/...`.
export async function buildWSURL(path: string, extraQuery: Record<string, string> = {}): Promise<string> {
  const tok = await getWSToken()
  const base = API_BASE.replace(/^http/, 'ws')
  const params = new URLSearchParams({ ...extraQuery, token: tok })
  return `${base}${path}?${params.toString()}`
}
