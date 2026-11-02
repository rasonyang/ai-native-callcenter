import { describe, expect, it } from 'vitest'

import { ApiError, NetworkError } from './api'
import { isServerUnreachable } from './errors'

describe('isServerUnreachable', () => {
  it.each([
    ['a 500', new ApiError(500, { code: 'INTERNAL', message: 'x' }), true],
    ['a 503', new ApiError(503, { code: 'INTERNAL', message: 'x' }), true],
    ['a 502 from a proxy', new ApiError(502, { code: 'INTERNAL', message: 'Bad Gateway' }), true],
    ['a network failure', new NetworkError(new TypeError('Failed to fetch')), true],
    // A rendering bug throws a TypeError too; only fetch's own one is wrapped.
    ['a TypeError from a bug', new TypeError("Cannot read properties of undefined (reading 'map')"), false],
    ['an abort', new DOMException('aborted', 'AbortError'), true],
    ['a timeout', new DOMException('timed out', 'TimeoutError'), true],
    ['a 404', new ApiError(404, { code: 'NOT_FOUND', message: 'x' }), false],
    ['a 422', new ApiError(422, { code: 'VALIDATION_FAILED', message: 'x' }), false],
    ['a bug', new Error('undefined is not a function'), false],
    ['a non-error', 'oops', false],
    ['another DOMException', new DOMException('x', 'DataCloneError'), false],
  ])('classifies %s', (_what, error, expected) => {
    expect(isServerUnreachable(error)).toBe(expected)
  })
})
