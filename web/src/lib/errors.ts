import type { TFunction } from 'i18next'

import { ApiError } from './api'

/**
 * Renders any thrown value as a localized message.
 *
 * The backend picks a code plus params and the wording lives in the
 * translation files. (Its `message` is a plain English sentence for people
 * reading raw responses; the UI never shows it.)
 */
export function describeError(error: unknown, t: TFunction): string {
  if (error instanceof ApiError) {
    return t(`errors.${error.code}`, {
      ...error.params,
      defaultValue: t('errors.UNKNOWN'),
    })
  }
  return t('errors.UNKNOWN')
}

/**
 * The field a VALIDATION_FAILED refusal belongs to, with its localized rule
 * copy — or undefined when the error is something else, names no field, or
 * names a different one. Forms pass each input's wire name and render the
 * text under the one input the server actually refused.
 */
export function fieldErrorText(error: unknown, field: string, t: TFunction): string | undefined {
  if (!(error instanceof ApiError) || error.code !== 'VALIDATION_FAILED') return undefined
  if (error.params.field !== field || typeof error.params.rule !== 'string') return undefined
  return t(`errors.rules.${error.params.rule}`, {
    defaultValue: t('errors.VALIDATION_FAILED'),
  })
}
