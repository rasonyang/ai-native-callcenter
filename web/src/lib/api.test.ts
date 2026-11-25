import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, NetworkError, api, callApi, fileNameOf, requestFile } from './api'

const CALL_ID = '019a0000-0000-7000-8000-000000000221'

function reply(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'content-type': 'application/json' },
        }),
    ),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

// createCall answers 202 when the switch has not confirmed the placement: the
// phone may still ring, so click-to-dial must take it as placed, not show an
// error (#221).
describe('click-to-dial', () => {
  it('treats 202 like 201: the call is placed, not failed', async () => {
    reply(202, { callId: CALL_ID })
    await expect(callApi.dial('18688886666')).resolves.toEqual({ callId: CALL_ID })
  })

  it('still rejects SWITCH_DOWN', async () => {
    reply(503, { error: { code: 'SWITCH_DOWN', message: 'the switch is unreachable' } })
    await expect(callApi.dial('18688886666')).rejects.toBeInstanceOf(ApiError)
  })
})

// A proxy in front of a restarting server answers 502 with an HTML page. That
// is the server failing, so it is an ApiError the callers can classify by
// status, not the SyntaxError a JSON parse of the page would throw.
describe('a failure answered with a body that is not JSON', () => {
  it('is an ApiError carrying the status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('<html><body>502 Bad Gateway</body></html>', {
            status: 502,
            statusText: 'Bad Gateway',
            headers: { 'content-type': 'text/html' },
          }),
      ),
    )
    const failure = await api.me().catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(ApiError)
    expect(failure).toMatchObject({ status: 502, code: 'INTERNAL' })
  })

  it('still throws when a successful answer is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('<html></html>', { status: 200 })))
    await expect(api.me()).rejects.toBeInstanceOf(SyntaxError)
  })
})

// fetch rejects with a bare TypeError when the server is not reached, the same
// type a rendering bug throws; the client names it so the two stay apart.
describe('a request the server never answered', () => {
  it('is a NetworkError', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))))
    const failure = await api.me().catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(NetworkError)
    expect((failure as NetworkError).cause).toBeInstanceOf(TypeError)
  })

  it('leaves an abort as it is', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new DOMException('aborted', 'AbortError'))))
    const failure = await api.me().catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(DOMException)
    expect((failure as DOMException).name).toBe('AbortError')
  })
})

// A file the server makes (an export) is fetched with the session, like any
// request; a refusal is the error envelope and must never become the file.
describe('a file the server makes', () => {
  it('is the body, named as Content-Disposition names it, fetched with the session', async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response('\uFEFFCall ID\r\n', {
          status: 200,
          headers: {
            'content-type': 'text/csv; charset=utf-8',
            'content-disposition': 'attachment; filename="calls-20261201-20261201.csv"',
          },
        }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const file = await requestFile('/cdrs/export?from=a&to=b')
    expect(file.fileName).toBe('calls-20261201-20261201.csv')
    expect(new TextDecoder().decode(await file.blob.arrayBuffer())).toContain('Call ID')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/cdrs/export?from=a&to=b',
      expect.objectContaining({ method: 'GET', credentials: 'same-origin' }),
    )
  })

  it('is refused with the ApiError of the envelope, not a file', async () => {
    reply(400, {
      error: {
        code: 'VALIDATION_FAILED',
        message: 'an export covers at most two calendar months',
        params: { field: 'to', rule: 'EXPORT_WINDOW_TOO_LONG', maxMonths: 2 },
      },
    })
    const failure = await requestFile('/cdrs/export').catch((error: unknown) => error)
    expect(failure).toBeInstanceOf(ApiError)
    expect(failure).toMatchObject({
      status: 400,
      code: 'VALIDATION_FAILED',
      params: { field: 'to', rule: 'EXPORT_WINDOW_TOO_LONG', maxMonths: 2 },
    })
  })
})

describe('the name in a Content-Disposition header', () => {
  it('reads the quoted, the bare and the RFC 5987 forms', () => {
    expect(fileNameOf('attachment; filename="calls-20261201-20261231.csv"')).toBe('calls-20261201-20261231.csv')
    expect(fileNameOf('attachment; filename=calls.csv')).toBe('calls.csv')
    expect(fileNameOf("attachment; filename=\"x.csv\"; filename*=UTF-8''%E9%80%9A%E8%AF%9D.csv")).toBe('通话.csv')
  })

  it('keeps only the last path segment, and nothing when there is no name', () => {
    expect(fileNameOf('attachment; filename="../../etc/passwd"')).toBe('passwd')
    expect(fileNameOf('attachment')).toBeUndefined()
    expect(fileNameOf(null)).toBeUndefined()
  })
})
