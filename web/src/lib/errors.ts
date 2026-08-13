import type { TFunction } from 'i18next'

import { ApiError } from './api'

/**
 * Renders any thrown value as a localized message.
 *
 * The backend sends a code plus params and never a finished sentence, so the
 * whole wording lives in the translation files.
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
