import { useState } from 'react'
import { useApp } from '../app/context'
import { Button, Field, Input } from '../components/primitives'
import { Logo } from './Setup'

export function Login() {
  const { signIn } = useApp()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await signIn(username.trim(), password)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setBusy(false)
    }
  }

  return (
    <div className="mx-auto flex min-h-full max-w-sm flex-col justify-center px-5 py-12">
      <div className="mb-8">
        <Logo />
        <h1 className="display mt-5 text-2xl">登录网盘</h1>
      </div>

      {/* A real form so password managers offer to fill it. */}
      <form className="panel space-y-4 p-6 rise-in" onSubmit={submit}>
        <Field label="用户名">
          <Input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            autoFocus
          />
        </Field>
        <Field label="密码" error={error ?? undefined}>
          <Input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </Field>
        <Button type="submit" variant="primary" className="w-full" loading={busy}>
          登录
        </Button>
      </form>

      <p className="mt-5 text-center text-xs leading-relaxed text-[var(--faint)]">
        此账号密码同时适用于 WebDAV。
      </p>
    </div>
  )
}
