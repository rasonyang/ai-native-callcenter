import { describe, expect, it } from 'vitest'

import { phoneChipFor } from '@/lib/phone'
import type { ExtensionState } from '@/lib/phone-bridge'

/**
 * What the phone chip says, for every pair of facts that can reach it.
 *
 * The two sources answer different questions and are allowed to disagree —
 * the switch knows whether a registration exists, the extension knows whose
 * it is and how it is going. Each disagreement has one reading, and this table
 * is that reading. A row changed here is a behaviour changed on the bar.
 */
const MINE = '1001'

function ext(overrides: Partial<ExtensionState> = {}): ExtensionState {
  return {
    registration: 'REGISTERED',
    account: MINE,
    sipDomain: 'aicc.local',
    credentialSource: 'PROVISIONED',
    microphone: 'GRANTED',
    error: null,
    ...overrides,
  }
}

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

  // The way out of an override is the Options page that made it. Offering to
  // re-provision would replace a credential the extension has been told to
  // hold dormant, and flush the registration the manual account is using.
  it('never offers to re-provision an overridden phone', () => {
    for (const isDeviceRegistered of [true, false]) {
      for (const account of [MINE, '1002']) {
        const chip = phoneChipFor(
          isDeviceRegistered,
          ext({ account, credentialSource: 'MANUAL', provisionStatus: 'OVERRIDDEN' }),
          MINE,
        )
        expect(chip.action).toBe('options')
      }
    }
  })

  // The gate is the point of the chip: everything that is not "the phone is
  // where it should be and the switch agrees" keeps the agent out of the queue.
  it('allows READY in exactly one reading', () => {
    const readings = [
      phoneChipFor(false, undefined, MINE),
      phoneChipFor(false, null, MINE),
      phoneChipFor(false, ext(), MINE),
      phoneChipFor(true, ext({ account: '1002' }), MINE),
      phoneChipFor(true, ext({ registration: 'FAILED' }), MINE),
      phoneChipFor(true, ext(), MINE),
    ]
    expect(readings.filter((chip) => chip.isReadyAllowed)).toHaveLength(1)
  })
})
