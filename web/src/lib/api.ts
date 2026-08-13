/**
 * REST client. Property names match the JSON contract byte for byte
 * (docs/design/07-naming.md), so no case conversion happens anywhere.
 */

/** Machine-readable error codes; the UI renders errors.<CODE>. */
export type ErrorCode =
  | 'INVALID_CREDENTIALS'
  | 'SESSION_EXPIRED'
  | 'FORBIDDEN'
  | 'VALIDATION_FAILED'
  | 'NOT_FOUND'
  | 'CONFLICT'
  | 'USER_SUSPENDED'
  | 'SWITCH_DOWN'
  | 'STORAGE_DOWN'
  | 'RATE_LIMITED'
  | 'INTERNAL'

export type Role = 'AGENT' | 'SUPERVISOR' | 'ADMIN'

export interface Identity {
  userId: string
  username: string
  displayName: string
  role: Role
  locale: string | null
}

export interface ApiErrorBody {
  code: ErrorCode
  message: string
  params?: Record<string, unknown>
}

/** Error carrying the translatable code and its interpolation params. */
export class ApiError extends Error {
  readonly code: ErrorCode
  readonly params: Record<string, unknown>
  readonly status: number

  constructor(status: number, body: ApiErrorBody) {
    super(body.message)
    this.name = 'ApiError'
    this.status = status
    this.code = body.code
    this.params = body.params ?? {}
  }
}

const BASE = '/api/v1'

/** Mutations must carry this header; see docs/design/04-api-sse.md §2. */
const CSRF_HEADER = 'X-AICC-Csrf'

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = init.method ?? 'GET'
  const headers = new Headers(init.headers)
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
  if (method !== 'GET' && method !== 'HEAD') headers.set(CSRF_HEADER, '1')

  const response = await fetch(`${BASE}${path}`, {
    ...init,
    method,
    headers,
    credentials: 'same-origin',
  })

  if (response.status === 204) return undefined as T

  const text = await response.text()
  const payload: unknown = text ? JSON.parse(text) : null

  if (!response.ok) {
    const body = (payload as { error?: ApiErrorBody } | null)?.error
    throw new ApiError(
      response.status,
      body ?? { code: 'INTERNAL', message: response.statusText },
    )
  }
  return payload as T
}

export const api = {
  login: (username: string, password: string) =>
    request<{ user: Identity }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  logout: () => request<void>('/auth/logout', { method: 'POST' }),

  me: () => request<{ user: Identity }>('/auth/me'),

  health: () => request<{ sseClients: number; oldestSeq: number }>('/system/health'),
}
