import { useState, type FormEvent } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { LanguageSwitch } from '@/components/language-switch'
import { describeError } from '@/lib/errors'
import { roleHomeFor } from '@/lib/nav'
import { useLogin } from '@/lib/session'

export const Route = createFileRoute('/login')({ component: LoginPage })

function LoginPage() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const login = useLogin()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  const onSubmit = (event: FormEvent) => {
    event.preventDefault()
    login.mutate(
      { username, password },
      {
        onSuccess: ({ user }) => {
          void navigate({ to: roleHomeFor(user.role) })
        },
      },
    )
  }

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-sm">
        <div className="mb-4 flex items-baseline justify-between">
          <div>
            <div className="text-lg font-semibold">{t('app.name')}</div>
            <div className="text-xs text-muted-foreground">{t('app.tagline')}</div>
          </div>
          <LanguageSwitch />
        </div>

        <form onSubmit={onSubmit} className="rounded-md border bg-card p-4">
          <h1 className="text-base font-medium">{t('auth.signInTitle')}</h1>
          <p className="mt-1 mb-4 text-xs text-muted-foreground">{t('auth.signInSubtitle')}</p>

          <div className="space-y-3">
            <div className="space-y-1">
              <Label htmlFor="username">{t('auth.username')}</Label>
              <Input
                id="username"
                autoComplete="username"
                autoFocus
                required
                value={username}
                onChange={(event) => setUsername(event.target.value)}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="password">{t('auth.password')}</Label>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
            </div>
          </div>

          {login.isError && (
            <p role="alert" className="mt-3 text-xs text-destructive">
              {describeError(login.error, t)}
            </p>
          )}

          <Button type="submit" className="mt-4 w-full" disabled={login.isPending}>
            {login.isPending ? t('auth.signingIn') : t('common.signIn')}
          </Button>
        </form>
      </div>
    </div>
  )
}
