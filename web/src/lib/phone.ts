import type { ExtensionState } from './phone-bridge'

export type { SipSession } from './api'

/**
 * What the phone chip in the softphone bar says, and whether the agent may go
 * ready.
 *
 * Two facts decide it, and they answer different questions. The server's
 * `isDeviceRegistered` is the one that gates READY: the switch either holds a
 * registration for this agent's extension or it does not, and a queue cannot
 * offer a call to a phone that is not there. The extension's own state is what
 * makes the chip *actionable* — it is the only thing that knows whether the
 * phone is still connecting, was refused, lost its socket, or is signed in as
 * somebody else.
 *
 * They disagree in both directions, and each disagreement has a name. The
 * server sees a registration this browser did not make (the agent opened a
 * second browser and the newer session displaced this one). The phone reports
 * REGISTERED before the switch has noticed. Neither is an error to hide: the
 * chip says which one happened.
 */
export type PhoneChipKind =
  | 'setup'
  | 'connecting'
  | 'ready'
  | 'error'
  | 'wrongAccount'
  | 'displaced'
  | 'overridden'

/** What the agent can do about it, if anything. */
export type PhoneChipAction = 'setup' | 'retry' | 'reprovision' | 'options'

export interface PhoneChip {
  kind: PhoneChipKind
  /** A colour token; the chip paints a dot with it and nothing else. */
  dot: string
  labelKey: string
  action?: PhoneChipAction
  /** True only where the phone is where it should be and the switch agrees. */
  isReadyAllowed: boolean
}

const CHIP: Record<PhoneChipKind, Omit<PhoneChip, 'labelKey'>> = {
  setup: { kind: 'setup', dot: 'var(--state-offline)', action: 'setup', isReadyAllowed: false },
  connecting: { kind: 'connecting', dot: 'var(--state-ringing)', isReadyAllowed: false },
  ready: { kind: 'ready', dot: 'var(--state-available)', isReadyAllowed: true },
  error: { kind: 'error', dot: 'var(--state-breach)', action: 'retry', isReadyAllowed: false },
  wrongAccount: {
    kind: 'wrongAccount',
    dot: 'var(--state-breach)',
    action: 'reprovision',
    isReadyAllowed: false,
  },
  displaced: {
    kind: 'displaced',
    dot: 'var(--state-breach)',
    action: 'reprovision',
    isReadyAllowed: false,
  },
  // A deliberate choice somebody made in the extension's own Options, not a
  // fault: the way out is that same page, and never a Re-provision button
  // this page would be wrong to offer.
  overridden: {
    kind: 'overridden',
    dot: 'var(--state-breach)',
    action: 'options',
    isReadyAllowed: false,
  },
}

const chip = (kind: PhoneChipKind, labelKey: string): PhoneChip => ({ ...CHIP[kind], labelKey })

/**
 * The chip for a given pair of facts.
 *
 * `extension` carries the third state the two booleans cannot: `undefined` is
 * no extension detected on this page at all, `null` is one that has said hello
 * but has not reported yet.
 */
export function phoneChipFor(
  isDeviceRegistered: boolean,
  extension: ExtensionState | null | undefined,
  myExtension: string | undefined,
): PhoneChip {
  if (extension === undefined) return chip('setup', 'phone.chip.setup')
  if (extension === null) return chip('connecting', 'phone.chip.connecting')

  const isMine = Boolean(
    extension.account && myExtension && extension.account === myExtension,
  )
  const isSomebodyElses = Boolean(extension.account && myExtension && !isMine)

  // Somebody saved a manual account in the extension while a provisioned one
  // was held. The manual account is what the phone is using, ours is kept and
  // dormant, and neither this page nor its Re-provision button may undo that
  // — the same Options page that set it is where it is unset. It still works
  // as a phone, so it is READY-able when it is registered at this agent's own
  // extension and the switch agrees.
  if (extension.provisionStatus === 'OVERRIDDEN' && extension.credentialSource === 'MANUAL') {
    return {
      ...CHIP.overridden,
      labelKey: 'phone.chip.overridden',
      dot: isDeviceRegistered && isMine ? 'var(--state-available)' : 'var(--state-breach)',
      isReadyAllowed: isDeviceRegistered && isMine,
    }
  }

  if (isDeviceRegistered) {
    if (isSomebodyElses) return chip('wrongAccount', 'phone.chip.wrongAccount')
    if (extension.registration === 'REGISTERED') return chip('ready', 'phone.chip.ready')
    if (extension.registration === 'REGISTERING') return chip('connecting', 'phone.chip.connecting')
    // The switch holds a registration and this phone is not making it: the
    // agent signed in somewhere else and that session took the phone with it.
    if (extension.credentialSource === 'PROVISIONED') {
      return chip('displaced', 'phone.chip.displaced')
    }
    return chip('error', errorKey(extension))
  }

  if (extension.registration === 'REGISTERING') return chip('connecting', 'phone.chip.connecting')
  if (extension.error) return chip('error', errorKey(extension))
  if (extension.registration === 'FAILED') return chip('error', errorKey(extension))
  if (isSomebodyElses) return chip('wrongAccount', 'phone.chip.wrongAccount')
  // Provisioned and on its way, or registered here a moment before the switch
  // says so. Either way the agent has nothing to do but wait.
  return chip('connecting', 'phone.chip.connecting')
}

function errorKey(extension: ExtensionState): string {
  return `phone.chip.errors.${extension.error ?? 'REGISTRATION_FAILED'}`
}
