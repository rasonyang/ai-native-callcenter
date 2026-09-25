import { describe, expect, it } from 'vitest'

import { phoneChipFor } from '@/lib/phone'
import { extensionStateFixture as ext } from '@/test/harness'

/**
 * What the phone chip says, for every pair of facts that can reach it.
 *
 * The two sources answer different questions and are allowed to disagree —
 * the switch knows whether a registration exists, the extension knows whose
 * it is and how it is going. Each disagreement has one reading, and this table
 * is that reading. A row changed here is a behaviour changed on the bar.
 */
const MINE = '1001'

describe('the phone chip', () => {
  it.each([
    [
      'no extension in this browser',
      false,
      undefined,
      { kind: 'setup', labelKey: 'phone.chip.setup', action: 'setup', isReadyAllowed: false },
    ],
    [
      'an extension that has said hello and nothing else',
      false,
      null,
      { kind: 'connecting', labelKey: 'phone.chip.connecting', isReadyAllowed: false },
    ],
    [
      'a phone still registering',
      false,
      ext({ registration: 'REGISTERING' }),
      { kind: 'connecting', labelKey: 'phone.chip.connecting', isReadyAllowed: false },
    ],
    [
      'a registration the switch refused',
      false,
      ext({ registration: 'FAILED', error: 'REGISTRATION_FAILED' }),
      {
        kind: 'error',
        labelKey: 'phone.chip.errors.REGISTRATION_FAILED',
        action: 'retry',
        isReadyAllowed: false,
      },
    ],
    [
      'a microphone the browser will not give up',
      false,
      ext({ registration: 'FAILED', error: 'MIC_UNAVAILABLE' }),
      {
        kind: 'error',
        labelKey: 'phone.chip.errors.MIC_UNAVAILABLE',
        action: 'retry',
        isReadyAllowed: false,
      },
    ],
    [
      'a socket that went away',
      false,
      ext({ registration: 'UNREGISTERED', error: 'WSS_LOST' }),
      {
        kind: 'error',
        labelKey: 'phone.chip.errors.WSS_LOST',
        action: 'retry',
        isReadyAllowed: false,
      },
    ],
    [
      'a phone somebody typed another extension into',
      false,
      ext({ account: '1002', credentialSource: 'MANUAL' }),
      {
        kind: 'wrongAccount',
        labelKey: 'phone.chip.wrongAccount',
        action: 'reprovision',
        isReadyAllowed: false,
      },
    ],
    [
      'a phone the platform provisioned, registered and seen by the switch',
      true,
      ext(),
      { kind: 'ready', labelKey: 'phone.chip.ready', isReadyAllowed: true },
    ],
    [
      'a phone the agent registered by hand at their own extension',
      true,
      ext({ credentialSource: 'MANUAL' }),
      { kind: 'ready', labelKey: 'phone.chip.ready', isReadyAllowed: true },
    ],
    [
      'a registered phone signed in as somebody else',
      true,
      ext({ account: '1002' }),
      {
        kind: 'wrongAccount',
        labelKey: 'phone.chip.wrongAccount',
        action: 'reprovision',
        isReadyAllowed: false,
      },
    ],
    [
      'a registration this browser is not the one making',
      true,
      ext({ registration: 'UNREGISTERED' }),
      {
        kind: 'displaced',
        labelKey: 'phone.chip.displaced',
        action: 'reprovision',
        isReadyAllowed: false,
      },
    ],
    [
      'the same displacement reported as an outright failure',
      true,
      ext({ registration: 'FAILED' }),
      {
        kind: 'displaced',
        labelKey: 'phone.chip.displaced',
        action: 'reprovision',
        isReadyAllowed: false,
      },
    ],
    [
      'an extension detected but silent, with the switch already registered',
      true,
      null,
      { kind: 'connecting', labelKey: 'phone.chip.connecting', isReadyAllowed: false },
    ],
    [
      'a manual account saved over our provision, at this agent’s own extension',
      true,
      ext({ credentialSource: 'MANUAL', provisionStatus: 'OVERRIDDEN' }),
      {
        kind: 'overridden',
        labelKey: 'phone.chip.overridden',
        action: 'options',
        isReadyAllowed: true,
      },
    ],
    [
      'the same override before the switch has a registration for it',
      false,
      ext({ credentialSource: 'MANUAL', provisionStatus: 'OVERRIDDEN' }),
      {
        kind: 'overridden',
        labelKey: 'phone.chip.overridden',
        action: 'options',
        isReadyAllowed: false,
      },
    ],
    [
      'an override onto somebody else’s extension',
      true,
      ext({ account: '1002', credentialSource: 'MANUAL', provisionStatus: 'OVERRIDDEN' }),
      {
        kind: 'overridden',
        labelKey: 'phone.chip.overridden',
        action: 'options',
        isReadyAllowed: false,
      },
    ],
    [
      'a provision that is simply in use',
      true,
      ext({ provisionStatus: 'ACTIVE' }),
      { kind: 'ready', labelKey: 'phone.chip.ready', isReadyAllowed: true },
    ],
  ])(
    'reads %s',
    (_case, isDeviceRegistered, extension, expected) => {
      expect(phoneChipFor(isDeviceRegistered, extension, MINE)).toMatchObject(expected)
    },
  )

  /**
   * Not detected is two different browsers. One has never had the extension
   * and is told to install it; one had it a moment ago, still holds the
   * registration in the extension's own worker, and has only lost the content
   * script in this tab. Telling the second to set up a phone it already has
   * is the bug this branch exists to prevent — the chip says reload instead.
   */
  it('tells a browser that has lost contact to reload, not to set up a phone', () => {
    const lost = phoneChipFor(true, undefined, MINE, true)
    expect(lost).toMatchObject({
      kind: 'lost',
      labelKey: 'phone.chip.lost',
      isReadyAllowed: false,
    })
    expect(lost.action).toBeUndefined()
    // It reads as a fault, like the other chips that need attention.
    expect(lost.dot).toBe('var(--state-breach)')
  })

  // Lost contact says nothing about what the extension last reported: a
  // reading it is still making outranks a page that cannot hear it.
  it('ignores the loss for an extension that is answering', () => {
    expect(phoneChipFor(true, ext(), MINE, true)).toMatchObject({
      kind: 'ready',
      isReadyAllowed: true,
    })
  })
})
