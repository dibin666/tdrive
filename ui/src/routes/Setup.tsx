import { useEffect, useState } from 'react'
import clsx from 'clsx'
import { Check, ExternalLink, Hash, Loader2, Plus, ShieldCheck } from 'lucide-react'
import { api, type ChannelOption } from '../lib/api'
import { useApp } from '../app/context'
import { Button, Field, Input, Spinner, toast } from '../components/primitives'

/**
 * The first-run wizard. The account is the only mandatory first-run step;
 * Telegram API credentials, login and the storage channel remain available in
 * Settings when an operator wants to finish them later.
 *
 * Which step to show is derived from server state rather than tracked
 * locally, so a reload mid-way lands back exactly where it left off.
 */
export function Setup() {
  const { status, user, completeSetup, refreshStatus } = useApp()

  const step = !user
    ? 0
    : status?.telegram.state === 'unconfigured'
      ? 1
      : status?.telegram.state !== 'ready'
        ? 2
        : !status.hasChannel
          ? 3
          : 4

  return (
    <div className="mx-auto flex min-h-full max-w-lg flex-col justify-center px-5 py-12">
      <header className="mb-8">
        <Logo />
        <h1 className="display mt-5 text-2xl">把 Telegram 变成一块硬盘</h1>
        <p className="mt-2 text-sm leading-relaxed text-[var(--muted)]">
          先创建管理员账号即可进入网盘。Telegram 存储可以稍后在设置中完成，超过 2 GB 的文件会自动分卷。
        </p>
      </header>

      <ol className="mb-7 flex items-center gap-2" aria-label="设置进度">
        {['账号', 'API', '登录', '频道'].map((label, i) => (
          <li key={label} className="flex flex-1 items-center gap-2">
            <span
              className={clsx(
                'flex size-6 shrink-0 items-center justify-center rounded-full text-[11px] font-medium transition-colors',
                i < step
                  ? 'bg-[var(--color-clay)] text-white'
                  : i === step
                    ? 'border border-[var(--color-clay)] text-[var(--color-clay)]'
                    : 'border border-[var(--line)] text-[var(--faint)]',
              )}
            >
              {i < step ? <Check size={12} /> : i + 1}
            </span>
            <span
              className={clsx(
                'text-xs',
                i === step ? 'text-[var(--ink)]' : 'text-[var(--faint)]',
              )}
            >
              {label}
            </span>
            {i < 3 && <span className="h-px flex-1 bg-[var(--line)]" />}
          </li>
        ))}
      </ol>

      <div className="panel p-6 rise-in">
        {step === 0 && <AccountStep onSubmit={completeSetup} />}
        {step === 1 && <CredentialsStep onDone={refreshStatus} />}
        {step === 2 && <LoginStep onDone={refreshStatus} />}
        {step === 3 && <ChannelStep onDone={refreshStatus} />}
        {step === 4 && <DoneStep />}
      </div>
    </div>
  )
}

export function Logo() {
  return (
    <div className="flex items-center gap-2.5">
      <span className="flex size-8 items-center justify-center rounded-[10px] bg-[var(--color-clay)] text-white">
        <Hash size={17} />
      </span>
      <span className="display text-lg">TDrive</span>
    </div>
  )
}

function AccountStep({
  onSubmit,
}: {
  onSubmit: (username: string, password: string) => Promise<void>
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    if (password !== confirm) return setError('两次输入的密码不一致')
    if (password.length < 8) return setError('密码至少 8 位')
    setBusy(true)
    setError(null)
    try {
      await onSubmit(username.trim(), password)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <StepHeader
        title="创建管理员账号"
        description="这个账号用来登录网盘，也是 WebDAV 的用户名密码。创建后可以先进入网盘，Telegram 账号和频道稍后再配置。"
      />
      <Field label="用户名">
        <Input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus autoComplete="username" />
      </Field>
      <Field label="密码" hint="至少 8 位">
        <Input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="new-password"
        />
      </Field>
      <Field label="确认密码" error={error ?? undefined}>
        <Input
          type="password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && void submit()}
          autoComplete="new-password"
        />
      </Field>
      <Button variant="primary" className="w-full" loading={busy} onClick={() => void submit()}>
        创建账号并进入网盘
      </Button>
    </div>
  )
}

export function CredentialsStep({ onDone }: { onDone: () => Promise<void> }) {
  const [appId, setAppId] = useState('')
  const [appHash, setAppHash] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    const id = Number(appId.trim())
    if (!Number.isInteger(id) || id <= 0) return setError('api_id 应该是一串数字')
    setBusy(true)
    setError(null)
    try {
      await api.configureTelegram(id, appHash.trim())
      await onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <StepHeader
        title="填入 Telegram API 凭据"
        description="Telegram 要求第三方客户端使用自己的 api_id 和 api_hash。它们只保存在你这台服务器上。"
      />
      <a
        href="https://my.telegram.org/apps"
        target="_blank"
        rel="noreferrer"
        className="flex items-center gap-1.5 text-sm text-[var(--color-clay)] hover:underline"
      >
        去 my.telegram.org 申请
        <ExternalLink size={13} />
      </a>
      <Field label="api_id">
        <Input value={appId} onChange={(e) => setAppId(e.target.value)} inputMode="numeric" autoFocus />
      </Field>
      <Field label="api_hash" error={error ?? undefined}>
        <Input
          value={appHash}
          onChange={(e) => setAppHash(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && void submit()}
          className="font-[family-name:var(--font-mono)] text-xs"
        />
      </Field>
      <Button variant="primary" className="w-full" loading={busy} onClick={() => void submit()}>
        保存并连接
      </Button>
    </div>
  )
}

export function LoginStep({ onDone }: { onDone: () => Promise<void> }) {
  const { status } = useApp()
  const [phone, setPhone] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [hint, setHint] = useState<string | undefined>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const stage = status?.telegram.awaitingPassword
    ? 'password'
    : status?.telegram.awaitingCode
      ? 'code'
      : 'phone'

  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  if (status?.telegram.state === 'connecting') {
    return (
      <div className="flex items-center gap-3 py-6">
        <Spinner />
        <p className="text-sm text-[var(--muted)]">正在连接 Telegram…</p>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <StepHeader
        title="登录 Telegram"
        description="用你自己的账号登录。文件会存进这个账号的一个私有频道。"
      />

      {stage === 'phone' && (
        <>
          <Field label="手机号" hint="带国家区号，例如 +8613800138000" error={error ?? undefined}>
            <Input
              value={phone}
              onChange={(e) => setPhone(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && void run(async () => { await api.sendCode(phone) })}
              placeholder="+86…"
              autoFocus
              inputMode="tel"
            />
          </Field>
          <Button
            variant="primary"
            className="w-full"
            loading={busy}
            onClick={() => void run(async () => { await api.sendCode(phone) })}
          >
            发送验证码
          </Button>
        </>
      )}

      {stage === 'code' && (
        <>
          <Field
            label="验证码"
            hint="Telegram 会把验证码发到你已登录的其它设备上"
            error={error ?? undefined}
          >
            <Input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              onKeyDown={(e) =>
                e.key === 'Enter' &&
                void run(async () => {
                  const res = await api.signIn(code)
                  setHint(res.passwordHint)
                })
              }
              placeholder="12345"
              autoFocus
              inputMode="numeric"
              className="text-center text-lg tracking-[0.4em] font-[family-name:var(--font-mono)]"
            />
          </Field>
          <Button
            variant="primary"
            className="w-full"
            loading={busy}
            onClick={() =>
              void run(async () => {
                const res = await api.signIn(code)
                setHint(res.passwordHint)
              })
            }
          >
            登录
          </Button>
          <button
            className="w-full text-xs text-[var(--muted)] hover:text-[var(--ink)]"
            onClick={() => void run(async () => { await api.sendCode(phone) })}
          >
            没收到？重新发送
          </button>
        </>
      )}

      {stage === 'password' && (
        <>
          <div className="flex items-start gap-2 rounded-[var(--radius-control)] bg-[var(--sunk)] p-3 text-xs text-[var(--muted)]">
            <ShieldCheck size={15} className="mt-px shrink-0 text-[var(--color-clay)]" />
            <span>这个账号开启了两步验证{hint ? `，密码提示：${hint}` : ''}。</span>
          </div>
          <Field label="两步验证密码" error={error ?? undefined}>
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              onKeyDown={(e) =>
                e.key === 'Enter' && void run(async () => { await api.submitTelegramPassword(password) })
              }
              autoFocus
              autoComplete="off"
            />
          </Field>
          <Button
            variant="primary"
            className="w-full"
            loading={busy}
            onClick={() => void run(async () => { await api.submitTelegramPassword(password) })}
          >
            完成登录
          </Button>
        </>
      )}
    </div>
  )
}

export function ChannelStep({
  onDone,
  initialMode = 'create',
}: {
  onDone: () => Promise<void>
  initialMode?: 'create' | 'existing'
}) {
  const [options, setOptions] = useState<ChannelOption[] | null>(null)
  const [selected, setSelected] = useState<number>(0)
  const [title, setTitle] = useState('TDrive')
  const [busy, setBusy] = useState(false)
  const [mode, setMode] = useState<'create' | 'existing'>(initialMode)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    void api
      .channels()
      .then((r) => {
        setOptions(r.channels)
        setSelected(r.selected)
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
  }, [])

  const create = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.createChannel(title.trim() || 'TDrive')
      toast('频道已创建', 'success')
      await onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const choose = async (opt: ChannelOption) => {
    setBusy(true)
    setError(null)
    try {
      await api.selectChannel(opt.tgId, opt.accessHash ?? 0)
      toast(`已选择「${opt.title}」`, 'success')
      await onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <StepHeader
        title="选择存储频道"
        description="所有文件都存进这个频道。每个文件夹和每个分卷都是一条带 # 标签的消息，索引丢了也能从频道还原。"
      />

      <div className="flex gap-1 rounded-[var(--radius-control)] border border-[var(--line)] p-0.5">
        {(
          [
            ['create', '新建专用频道'],
            ['existing', '用已有频道'],
          ] as const
        ).map(([value, label]) => (
          <button
            key={value}
            onClick={() => setMode(value)}
            className={clsx(
              'flex-1 rounded-md px-3 py-1.5 text-xs font-medium transition-colors',
              mode === value ? 'bg-[var(--sunk)] text-[var(--ink)]' : 'text-[var(--muted)]',
            )}
          >
            {label}
          </button>
        ))}
      </div>

      {mode === 'create' ? (
        <>
          <Field label="频道名称" error={error ?? undefined}>
            <Input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && void create()}
            />
          </Field>
          <Button
            variant="primary"
            className="w-full"
            icon={<Plus size={15} />}
            loading={busy}
            onClick={() => void create()}
          >
            创建并使用
          </Button>
        </>
      ) : options === null ? (
        <div className="flex justify-center py-8">
          <Spinner />
        </div>
      ) : options.length === 0 ? (
        <p className="py-6 text-center text-sm text-[var(--muted)]">
          这个账号还没有可用的频道，先新建一个吧。
        </p>
      ) : (
        <div className="max-h-64 space-y-1 overflow-y-auto">
          {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
          {options.map((opt) => (
            <button
              key={opt.tgId}
              disabled={!opt.canPost || busy}
              onClick={() => void choose(opt)}
              className="row w-full justify-between disabled:cursor-not-allowed disabled:opacity-45"
              data-selected={opt.tgId === selected}
            >
              <span className="min-w-0 truncate text-sm">{opt.title}</span>
              {opt.tgId === selected ? (
                <Check size={14} className="shrink-0 text-[var(--color-clay)]" />
              ) : !opt.canPost ? (
                <span className="chip shrink-0">只读</span>
              ) : busy ? (
                <Loader2 size={13} className="animate-spin" />
              ) : null}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function DoneStep() {
  return (
    <div className="space-y-4 text-center">
      <span className="mx-auto flex size-11 items-center justify-center rounded-full bg-[var(--clay-soft)]">
        <Check size={20} className="text-[var(--color-clay)]" />
      </span>
      <div>
        <h2 className="display text-lg">设置好了</h2>
        <p className="mt-1.5 text-sm text-[var(--muted)]">现在可以开始上传了。</p>
      </div>
      <Button variant="primary" className="w-full" onClick={() => (window.location.href = '/files')}>
        进入网盘
      </Button>
    </div>
  )
}

function StepHeader({ title, description }: { title: string; description: string }) {
  return (
    <div>
      <h2 className="display text-lg">{title}</h2>
      <p className="mt-1.5 text-sm leading-relaxed text-[var(--muted)]">{description}</p>
    </div>
  )
}
