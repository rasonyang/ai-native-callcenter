import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, NetworkError, api, callApi } from './api'

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
