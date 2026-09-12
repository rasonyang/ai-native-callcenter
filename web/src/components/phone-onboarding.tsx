import { Dialog } from 'radix-ui'
import { useTranslation } from 'react-i18next'
import type { ReactNode } from 'react'

import { StatusDot } from '@/components/status-pill'
import { Button } from '@/components/ui/button'
import { optionsUrl, usePhoneBridge, webStoreUrl } from '@/lib/phone-bridge'

/**
 * The three things an agent does once, before their first call.
 *
 * It blocks the screen while any of them is undone, because none of the rest
 * of the cockpit works without a phone: a queue cannot offer a call to a
 * browser that cannot ring. There is nothing to dismiss and nothing to
 * remember — the rows read the extension's own state, so each one completes
 * itself as the agent does it and the card leaves when the last one does.
 *
 * Step 2 has no signal of its own: a content script only runs on a site its
 * owner allowed, so the extension answering hello at all is the proof that
 * this one is allowed.
 */
export function PhoneOnboarding() {
  const { t } = useTranslation()
  const { detected, state, extensionId, isOnboardingForced, closeOnboarding } = usePhoneBridge()

  const isMicrophoneGranted = state?.microphone === 'GRANTED'
  const isRequired = !detected || !isMicrophoneGranted
  const open = isRequired || isOnboardingForced

  if (!open) return null

  return (
    // Nothing dismisses it while a step is undone. An agent who opened it
    // themselves after finishing may put it away again.
    <Dialog.Root open onOpenChange={(next) => !next && !isRequired && closeOnboarding()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content
          className="fixed left-1/2 top-1/2 z-50 w-[440px] -translate-x-1/2 -translate-y-1/2 rounded-md border bg-card p-4 shadow-md"
          // Setup is not a choice, so neither is closing this.
          onEscapeKeyDown={(event) => isRequired && event.preventDefault()}
          onPointerDownOutside={(event) => isRequired && event.preventDefault()}
          onInteractOutside={(event) => isRequired && event.preventDefault()}
        >
          <Dialog.Title className="text-base font-medium">
            {t('phone.onboarding.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-xs text-muted-foreground">
            {t('phone.onboarding.subtitle')}
          </Dialog.Description>
          <ol className="mt-4 space-y-3">
            <Step
              number={1}
              isDone={detected}
              title={t('phone.onboarding.install.title')}
              text={t('phone.onboarding.install.text')}
            >
              <ExternalLink href={webStoreUrl()}>
                {t('phone.onboarding.install.action')}
              </ExternalLink>
              <Button variant="outline" size="sm" onClick={() => window.location.reload()}>
                {t('phone.onboarding.install.reload')}
              </Button>
            </Step>
            <Step
              number={2}
              isDone={detected}
              title={t('phone.onboarding.site.title')}
              text={t('phone.onboarding.site.text', { host: window.location.hostname })}
            >
              <ExternalLink href={optionsUrl('site', extensionId)}>
                {t('phone.onboarding.site.action')}
              </ExternalLink>
            </Step>
            <Step
              number={3}
              isDone={isMicrophoneGranted}
              title={t('phone.onboarding.microphone.title')}
              text={t('phone.onboarding.microphone.text')}
            >
              <ExternalLink href={optionsUrl('microphone', extensionId)}>
                {t('phone.onboarding.microphone.action')}
              </ExternalLink>
            </Step>
          </ol>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/** One step: where it stands, what it is, and the one place to go and do it. */
function Step({
  number,
  isDone,
  title,
  text,
  children,
}: {
  number: number
  isDone: boolean
  title: string
  text: string
  children: ReactNode
}) {
  const { t } = useTranslation()
  return (
    <li className="flex gap-3">
      <span className="tabular mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full border text-xs text-muted-foreground">
        {number}
      </span>
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex items-center gap-1.5">
          <StatusDot color={isDone ? 'var(--state-available)' : 'var(--state-offline)'} />
          <span className={isDone ? 'text-sm text-muted-foreground' : 'text-sm font-medium'}>
            {title}
          </span>
          <span className="sr-only">
            {isDone ? t('phone.onboarding.done') : t('phone.onboarding.pending')}
          </span>
        </div>
        <p className="text-xs text-muted-foreground">{text}</p>
        {!isDone && <div className="flex items-center gap-2 pt-0.5">{children}</div>}
      </div>
    </li>
  )
}

/**
 * Links out of the page. `chrome-extension://` targets cannot be opened by
 * script, so these are anchors an agent clicks, never a navigation this code
 * performs.
 *
 * With no id to address — no extension has announced one and this build was
 * given none — there is no link to render. Chrome blocks a made-up
 * `chrome-extension://` origin outright, so the honest answer is the route
 * that always works: the options page, opened from chrome://extensions.
 */
function ExternalLink({ href, children }: { href: string | null; children: ReactNode }) {
  const { t } = useTranslation()
  if (!href) {
    return (
      <p className="text-xs text-muted-foreground">{t('phone.onboarding.noExtensionId')}</p>
    )
  }
  return (
    <Button asChild variant="outline" size="sm">
      <a href={href} target="_blank" rel="noopener noreferrer">
        {children}
      </a>
    </Button>
  )
}
